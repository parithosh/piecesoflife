package store

import (
	"context"
	"database/sql"
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

// Circle photos live in settings.theme (and its restore point), not in a
// media row; they must count as referenced or every circle photo would be
// reported as an orphan.
func TestCheckUploadIntegrityCountsCirclePhotos(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	uploads := t.TempDir()

	photo := writeUploadFile(t, uploads, "branding/1/0123456789abcdef.jpg")
	oldBanner := writeUploadFile(t, uploads, "branding/1/fedcba9876543210.jpg")
	writeUploadFile(t, uploads, "branding/1/aaaaaaaaaaaaaaaa.jpg")

	current := fmt.Sprintf(`{"v":1,"fabric":"coastal","photo":{"path":%q,"x":50,"y":50}}`, photo)
	previous := fmt.Sprintf(`{"v":1,"fabric":"rani","banner":{"path":%q,"x":50,"y":50}}`, oldBanner)
	require.NoError(t, st.PublishTheme(ctx, 1, nil, []byte(previous)))
	require.NoError(t, st.PublishTheme(ctx, 1, []byte(previous), []byte(current)))

	report, err := st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)

	assert.Equal(t, 2, report.Referenced)
	assert.Equal(t, 1, report.Orphaned)
	assert.Zero(t, report.Missing)

	// A corrupt restore point must not take the whole report down.
	_, err = st.write.ExecContext(ctx, `UPDATE settings SET theme_previous = '{broken' WHERE group_id = 1`)
	require.NoError(t, err)

	report, err = st.CheckUploadIntegrity(ctx, uploads)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Referenced, "the published photo is still counted")
}

// Publishing is compare-and-swap and restore is an atomic swap, so two
// racing edits can never record a restore point that wasn't actually
// replaced.
func TestPublishThemeIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b, c := []byte(`{"fabric":"indigo"}`), []byte(`{"fabric":"kilim"}`), []byte(`{"fabric":"nordic"}`)

	require.ErrorIs(t, st.RestoreTheme(ctx, 1), ErrNoRestorePoint)
	require.NoError(t, st.PublishTheme(ctx, 1, nil, a))
	require.NoError(t, st.PublishTheme(ctx, 1, a, b))

	// A writer that read "a" loses: the look is already "b".
	require.ErrorIs(t, st.PublishTheme(ctx, 1, a, c), ErrThemeChanged)

	got, err := st.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.JSONEq(t, string(b), string(got.Theme))
	assert.JSONEq(t, string(a), string(got.ThemePrevious))

	// Re-publishing the current look keeps the restore point.
	require.NoError(t, st.PublishTheme(ctx, 1, b, b))
	require.NoError(t, st.RestoreTheme(ctx, 1))
	got, err = st.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(got.Theme))
	assert.JSONEq(t, string(b), string(got.ThemePrevious))

	// Publishing the house look over a custom one keeps it restorable.
	require.NoError(t, st.PublishTheme(ctx, 1, a, nil))
	require.NoError(t, st.RestoreTheme(ctx, 1))
	got, err = st.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(got.Theme))
	require.NoError(t, st.RestoreTheme(ctx, 1))
	got, err = st.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.Nil(t, got.Theme, "restoring back to the house look")

	assert.Error(t, st.PublishTheme(ctx, 1, nil, []byte(`{broken`)), "invalid JSON is rejected")
	require.ErrorIs(t, st.PublishTheme(ctx, 99, nil, a), sql.ErrNoRows)
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
