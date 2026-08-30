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

// TestCheckUploadIntegrityFindsOrphans is the production signal that went
// unread for three weeks: files on disk whose rows vanished. Deleting a
// block or dump item unlinks its file, so an unreferenced file means the
// row disappeared without going through the app. The tracked file present
// alongside them also pins the healthy half of the comparison.
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

	assert.Equal(t, 2, report.Orphaned)
	assert.Zero(t, report.Missing, "the tracked file must match its row")
	assert.Equal(t, 1, report.Referenced)
	assert.Equal(t, 3, report.OnDisk)
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

	assert.Zero(t, report.OnDisk)
	assert.Zero(t, report.Orphaned)
	assert.Zero(t, report.Missing)
}

// TestCheckUploadIntegritySampleIsCappedAndStable guards the
// production-scale case: 21 orphans, sampled at 10. Sampling out of map
// iteration order would change which paths appear from run to run, making
// the daily log churn.
func TestCheckUploadIntegritySampleIsCappedAndStable(t *testing.T) {
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
	assert.Contains(t, first.OrphanSample[0], "orphan-00.jpg")

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
