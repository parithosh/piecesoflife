package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckUploadIntegrityCleanDirectory verifies the healthy case: every
// file on disk is referenced and every referenced file exists.
func TestCheckUploadIntegrityCleanDirectory(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()
	userID := seedUser(t, st, "Zara", "zara@example.com")

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	tracked := writeUploadFile(t, uploads, "2026/07/tracked.jpg")

	_, err = st.CreateDumpItem(ctx, issueID, userID, "photo", nil, tracked, nil)
	require.NoError(t, err)

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.True(t, report.Healthy(), "clean directory must report healthy")
	assert.Equal(t, 1, report.Referenced)
	assert.Equal(t, 1, report.OnDisk)
	assert.Zero(t, report.Orphaned)
	assert.Zero(t, report.Missing)
}

// TestCheckUploadIntegrityFindsOrphans is the production signal that went
// unread for three weeks: files on disk whose rows vanished. Deleting a
// block or dump item unlinks its file, so an unreferenced file means the
// row disappeared without going through the app.
func TestCheckUploadIntegrityFindsOrphans(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()
	userID := seedUser(t, st, "Zara", "zara@example.com")

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	tracked := writeUploadFile(t, uploads, "2026/07/tracked.jpg")
	_, err = st.CreateDumpItem(ctx, issueID, userID, "photo", nil, tracked, nil)
	require.NoError(t, err)

	// Two files nobody references — the shape of rows lost to a rollback.
	writeUploadFile(t, uploads, "2026/07/orphan-a.jpg")
	writeUploadFile(t, uploads, "2026/07/orphan-b.mp4")

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.False(t, report.Healthy())
	assert.Equal(t, 2, report.Orphaned)
	assert.Zero(t, report.Missing)
	require.Len(t, report.OrphanSample, 2)
	assert.Contains(t, report.OrphanSample[0], "orphan-a.jpg")
	assert.Contains(t, report.OrphanSample[1], "orphan-b.mp4")
}

// TestCheckUploadIntegrityFindsMissingFiles covers the opposite failure: a
// lost or partially copied uploads volume.
func TestCheckUploadIntegrityFindsMissingFiles(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()
	userID := seedUser(t, st, "Zara", "zara@example.com")

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	gone := filepath.Join(uploads, "2026", "07", "gone.jpg")

	_, err = st.CreateDumpItem(ctx, issueID, userID, "photo", nil, gone, nil)
	require.NoError(t, err)

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.False(t, report.Healthy())
	assert.Equal(t, 1, report.Missing)
	assert.Zero(t, report.Orphaned)
	require.Len(t, report.MissingSample, 1)
	assert.Contains(t, report.MissingSample[0], "gone.jpg")
}

// TestCheckUploadIntegrityMissingDirectory keeps a fresh install from
// failing its first boot before anything has been uploaded.
func TestCheckUploadIntegrityMissingDirectory(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	report, err := st.CheckUploadIntegrity(ctx, filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)

	assert.True(t, report.Healthy())
	assert.Zero(t, report.OnDisk)
}

// writeUploadFile creates a file under root and returns its absolute path.
func writeUploadFile(t *testing.T, root, rel string) string {
	t.Helper()

	path := filepath.Join(root, rel)

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	return path
}
