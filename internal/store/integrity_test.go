package store

import (
	"context"
	"fmt"
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

// TestCheckUploadIntegrityMatchesRootRelativePaths covers the stored form
// the app has also used: a path relative to the upload root, such as
// /2026/07/photo.jpg. Insisting on the absolute form would report every
// file of such an install as orphaned AND its every row as missing — a
// permanent, unactionable error on every boot.
func TestCheckUploadIntegrityMatchesRootRelativePaths(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()
	userID := seedUser(t, st, "Zara", "zara@example.com")

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	writeUploadFile(t, uploads, "2026/07/legacy.jpg")

	// Stored root-relative, the way older rows look.
	_, err = st.CreateDumpItem(
		ctx, issueID, userID, "photo", nil, "/2026/07/legacy.jpg", nil,
	)
	require.NoError(t, err)

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.True(t, report.Healthy(),
		"a root-relative row must match its file, not double-count as orphan+missing")
	assert.Zero(t, report.Orphaned)
	assert.Zero(t, report.Missing)
	assert.Zero(t, report.OutsideRoot)
}

// TestCheckUploadIntegrityIgnoresPathsOutsideRoot keeps a row pointing
// somewhere else entirely out of the Missing count: it is a stored-format
// oddity, not evidence of a lost uploads volume.
func TestCheckUploadIntegrityIgnoresPathsOutsideRoot(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()
	elsewhere := t.TempDir()
	userID := seedUser(t, st, "Zara", "zara@example.com")

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	_, err = st.CreateDumpItem(ctx, issueID, userID, "photo", nil,
		filepath.Join(elsewhere, "stray.jpg"), nil)
	require.NoError(t, err)

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.True(t, report.Healthy(), "an out-of-root row is not data loss")
	assert.Equal(t, 1, report.OutsideRoot)
	assert.Zero(t, report.Missing)
	assert.Empty(t, report.MissingSample)
}

// TestCheckUploadIntegritySampleIsDeterministic guards the production-scale
// case: 21 orphans, capped at 10. Sampling out of map iteration order would
// change which paths appear from run to run, making the daily log churn.
func TestCheckUploadIntegritySampleIsDeterministic(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	uploads := t.TempDir()

	for i := range 21 {
		writeUploadFile(t, uploads, fmt.Sprintf("2026/07/orphan-%02d.jpg", i))
	}

	first, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	require.Equal(t, 21, first.Orphaned)
	require.Len(t, first.OrphanSample, integritySampleLimit)

	// Lowest-sorting paths, every time.
	assert.Contains(t, first.OrphanSample[0], "orphan-00.jpg")
	assert.Contains(t, first.OrphanSample[integritySampleLimit-1], "orphan-09.jpg")

	second, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)
	assert.Equal(t, first.OrphanSample, second.OrphanSample,
		"the sample must not depend on map iteration order")
}

// writeUploadFile creates a file under root and returns its absolute path.
func writeUploadFile(t *testing.T, root, rel string) string {
	t.Helper()

	path := filepath.Join(root, rel)

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	return path
}
