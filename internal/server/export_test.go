package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/store"
)

// newExportTestServer builds a Server backed by a real temp-dir SQLite store
// with the settings row seeded and one member with notification preferences.
func newExportTestServer(t *testing.T) (*Server, *store.Store, *store.User) {
	t.Helper()

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbPath := filepath.Join(t.TempDir(), "test.db")

	st, err := store.New(ctx, dbPath, logger)
	require.NoError(t, err)
	require.NoError(t, st.RunMigrations(ctx))
	require.NoError(t, st.SeedSettings(ctx))

	t.Cleanup(func() { _ = st.Close() })

	userID, err := st.CreateUser(ctx, "friend", "friend@example.com", "member")
	require.NoError(t, err)
	require.NoError(t, st.EnsureNotificationPreferences(ctx, userID))

	user, err := st.GetUserByID(ctx, userID)
	require.NoError(t, err)

	srv := &Server{
		store:  st,
		config: &config.Config{UploadPath: t.TempDir(), SessionSecret: "test-secret"},
		logger: logger,
	}

	return srv, st, user
}

// seedExportContent creates an issue with one question, one submitted
// response with a text block, and one comment. Returns the response ID.
func seedExportContent(
	t *testing.T, st *store.Store, user *store.User,
) int64 {
	t.Helper()

	ctx := context.Background()
	_, questionID := addIssueWithQuestion(t, st, "collecting")

	responseID, err := st.CreateResponse(ctx, user.ID, questionID)
	require.NoError(t, err)

	content := "A lovely week."
	_, err = st.CreateBlock(ctx, responseID, "text", &content, nil, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, st.SubmitResponse(ctx, responseID))

	_, err = st.CreateComment(ctx, user.ID, responseID, nil, "So glad to hear!")
	require.NoError(t, err)

	return responseID
}

// addPhotoBlock attaches a photo block referencing filePath to the response.
func addPhotoBlock(
	t *testing.T, st *store.Store, responseID int64, filePath string, sortOrder int,
) {
	t.Helper()

	_, err := st.CreateBlock(context.Background(), responseID, "photo",
		nil, &filePath, nil, nil, sortOrder)
	require.NoError(t, err)
}

func TestBuildExportPayload(t *testing.T) {
	srv, st, user := newExportTestServer(t)
	responseID := seedExportContent(t, st, user)

	payload, err := srv.buildExportPayload(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, payload.Version)
	assert.False(t, payload.ExportedAt.IsZero())
	require.NotNil(t, payload.Settings)
	assert.Equal(t, payload.Settings.LoopName, payload.LoopName)

	require.Len(t, payload.Users, 1)
	assert.Equal(t, user.ID, payload.Users[0].ID)

	require.Len(t, payload.Preferences, 1)
	require.Len(t, payload.Issues, 1)
	require.Len(t, payload.Questions, 1)

	require.Len(t, payload.Responses, 1)
	assert.Equal(t, responseID, payload.Responses[0].ID)
	require.Len(t, payload.Responses[0].Blocks, 1)
	require.NotNil(t, payload.Responses[0].Blocks[0].Content)
	assert.Equal(t, "A lovely week.", *payload.Responses[0].Blocks[0].Content)

	require.Len(t, payload.Comments, 1)
	assert.Equal(t, "So glad to hear!", payload.Comments[0].Body)
}

func TestBuildExportPayloadIncludesDrafts(t *testing.T) {
	srv, st, user := newExportTestServer(t)
	_, questionID := addIssueWithQuestion(t, st, "collecting")

	_, err := st.CreateResponse(context.Background(), user.ID, questionID)
	require.NoError(t, err)

	payload, err := srv.buildExportPayload(context.Background())
	require.NoError(t, err)

	require.Len(t, payload.Responses, 1)
	assert.True(t, payload.Responses[0].IsDraft)
}

func TestHandleExportZip(t *testing.T) {
	srv, st, user := newExportTestServer(t)
	responseID := seedExportContent(t, st, user)

	// One real upload, one missing file, and one path outside the upload
	// dir (must be rejected by the traversal guard even though it exists).
	uploadDir := filepath.Join(srv.config.UploadPath, "2026", "07")
	require.NoError(t, os.MkdirAll(uploadDir, 0o755))

	photoPath := filepath.Join(uploadDir, "photo.jpg")
	require.NoError(t, os.WriteFile(photoPath, []byte("jpeg-bytes"), 0o644))

	outsidePath := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outsidePath, []byte("secret"), 0o644))

	addPhotoBlock(t, st, responseID, photoPath, 1)
	addPhotoBlock(t, st, responseID, filepath.Join(srv.config.UploadPath, "gone.jpg"), 2)
	addPhotoBlock(t, st, responseID, outsidePath, 3)

	req := requestAsUser(t, http.MethodGet, "/api/admin/export.zip", "", user)
	rr := httptest.NewRecorder()

	srv.handleExportZip(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/zip", rr.Header().Get("Content-Type"))
	assert.Contains(t, rr.Header().Get("Content-Disposition"), "piecesoflife-export-")
	assert.Contains(t, rr.Header().Get("Content-Disposition"), ".zip")

	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	require.NoError(t, err)

	names := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = f
	}

	// Exactly the three expected entries: the missing and out-of-tree
	// files must be skipped.
	require.Len(t, zr.File, 3)
	require.Contains(t, names, "README.txt")
	require.Contains(t, names, "export.json")
	require.Contains(t, names, "uploads/2026/07/photo.jpg")

	assert.Equal(t, "jpeg-bytes", readZipEntry(t, names["uploads/2026/07/photo.jpg"]))
	assert.Contains(t, readZipEntry(t, names["README.txt"]), "canonical backup")

	var payload exportRoot
	require.NoError(t, json.Unmarshal(
		[]byte(readZipEntry(t, names["export.json"])), &payload))
	assert.Equal(t, 1, payload.Version)
	require.Len(t, payload.Responses, 1)
	assert.Len(t, payload.Responses[0].Blocks, 4)
}

func TestHandleExportZipDeduplicatesFiles(t *testing.T) {
	srv, st, user := newExportTestServer(t)
	responseID := seedExportContent(t, st, user)

	photoPath := filepath.Join(srv.config.UploadPath, "photo.jpg")
	require.NoError(t, os.WriteFile(photoPath, []byte("jpeg-bytes"), 0o644))

	addPhotoBlock(t, st, responseID, photoPath, 1)
	addPhotoBlock(t, st, responseID, photoPath, 2)

	req := requestAsUser(t, http.MethodGet, "/api/admin/export.zip", "", user)
	rr := httptest.NewRecorder()

	srv.handleExportZip(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	require.NoError(t, err)

	uploads := 0

	for _, f := range zr.File {
		if f.Name == "uploads/photo.jpg" {
			uploads++
		}
	}

	assert.Equal(t, 1, uploads, "duplicate file paths must be archived once")
}

// readZipEntry returns the full decompressed contents of one zip entry.
func readZipEntry(t *testing.T, f *zip.File) string {
	t.Helper()

	rc, err := f.Open()
	require.NoError(t, err)

	defer rc.Close()

	b, err := io.ReadAll(rc)
	require.NoError(t, err)

	return string(b)
}
