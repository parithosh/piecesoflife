package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// integritySampleLimit caps how many example paths a report carries. Enough
// to identify what went wrong in a log line, not enough to flood it.
const integritySampleLimit = 10

// UploadIntegrityReport reconciles the media files on disk against the rows
// that reference them.
//
// Orphaned is the interesting number. The app unlinks a file when its
// owning block or dump item is deleted, so files nobody references are not
// normal housekeeping residue — they are the visible remains of rows that
// vanished. Twenty-one of them sat unnoticed in production for three weeks
// after a stale-snapshot restore.
//
// Missing is the opposite failure: rows pointing under the upload root at
// files that are gone, i.e. a lost or partially copied uploads volume.
//
// OutsideRoot counts rows whose path resolves nowhere near the configured
// root. Those are a stored-format oddity rather than data loss, so they are
// reported separately and do not make the report unhealthy — treating them
// as Missing would mean a permanent, unactionable error on every boot.
type UploadIntegrityReport struct {
	Referenced    int
	OnDisk        int
	Orphaned      int
	Missing       int
	OutsideRoot   int
	OrphanSample  []string
	MissingSample []string
}

// Healthy reports whether disk and database agree on everything under the
// upload root.
func (r *UploadIntegrityReport) Healthy() bool {
	return r.Orphaned == 0 && r.Missing == 0
}

// ReferencedFilePaths returns every upload path any row points at, across
// answers, the photo dump, private rambles, and notebook spreads.
func (s *Store) ReferencedFilePaths(ctx context.Context) ([]string, error) {
	const query = `
		SELECT file_path FROM response_blocks WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM dump_items WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM ramble_blocks WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM diary_blocks WHERE file_path IS NOT NULL
	`

	rows, err := s.read.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying referenced file paths: %w", err)
	}

	defer rows.Close()

	// Capacity hint: a mature Loop holds a few thousand media rows.
	paths := make([]string, 0, 512)

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scanning referenced file path: %w", err)
		}

		paths = append(paths, path)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating referenced file paths: %w", err)
	}

	return paths, nil
}

// CheckUploadIntegrity walks uploadPath and compares it against the paths
// rows reference. Read-only: it never deletes, it only reports. A missing
// upload directory yields an empty report rather than an error, so a fresh
// install does not fail its first boot.
//
// Matching tolerates two stored forms, because the app has used both: an
// absolute path under the upload root, and a root-relative one such as
// /2026/07/photo.jpg. Insisting on a single form would report every file of
// a legacy install as orphaned AND its every row as missing.
func (s *Store) CheckUploadIntegrity(
	ctx context.Context, uploadPath string,
) (*UploadIntegrityReport, error) {
	referenced, err := s.ReferencedFilePaths(ctx)
	if err != nil {
		return nil, err
	}

	base := filepath.Clean(uploadPath)

	onDisk, err := walkUploads(ctx, base)
	if err != nil {
		return nil, err
	}

	report := &UploadIntegrityReport{
		Referenced: len(referenced),
		OnDisk:     len(onDisk),
	}

	// Every disk path a row accounts for. Whatever is left over is orphaned.
	matched := make(map[string]struct{}, len(onDisk))
	missing := make([]string, 0, len(referenced))

	for _, raw := range referenced {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reconciling upload paths: %w", err)
		}

		resolved, ok := resolveUploadPath(base, raw, onDisk)
		if ok {
			matched[resolved] = struct{}{}

			continue
		}

		if withinRoot(base, filepath.Clean(raw)) {
			report.Missing++
			missing = append(missing, filepath.Clean(raw))

			continue
		}

		report.OutsideRoot++
	}

	orphans := make([]string, 0, len(onDisk))

	for path := range onDisk {
		if _, ok := matched[path]; ok {
			continue
		}

		report.Orphaned++
		orphans = append(orphans, path)
	}

	// Sort the full sets before truncating: sampling straight out of map
	// iteration would change which paths appear from run to run, so the
	// daily log line would churn for exactly the production case of 21
	// orphans.
	sort.Strings(orphans)
	sort.Strings(missing)

	report.OrphanSample = truncateSample(orphans)
	report.MissingSample = truncateSample(missing)

	return report, nil
}

// walkUploads collects every file under base. A missing base is not an
// error — a fresh install has uploaded nothing yet.
func walkUploads(ctx context.Context, base string) (map[string]struct{}, error) {
	onDisk := make(map[string]struct{}, 512)

	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The root not existing is reported to the caller below. A
			// descendant vanishing mid-walk is a concurrent delete by
			// removeUploadIfUnreferenced: skip it and keep walking, rather
			// than abandoning the traversal and reporting a partial set of
			// files as if it were complete.
			if os.IsNotExist(err) && path != base {
				return nil
			}

			return err
		}

		// Cancellation matters: this walk runs on the startup path.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if d.IsDir() {
			return nil
		}

		onDisk[filepath.Clean(path)] = struct{}{}

		return nil
	})

	if walkErr != nil {
		if os.IsNotExist(walkErr) {
			return onDisk, nil
		}

		return nil, fmt.Errorf("walking upload directory %s: %w", base, walkErr)
	}

	return onDisk, nil
}

// resolveUploadPath maps a stored path onto a file the walk actually found,
// accepting either the absolute or the root-relative stored form.
func resolveUploadPath(
	base, raw string, onDisk map[string]struct{},
) (string, bool) {
	direct := filepath.Clean(raw)

	if _, ok := onDisk[direct]; ok {
		return direct, true
	}

	// Legacy form: the path was stored relative to the upload root, with or
	// without a leading separator.
	joined := filepath.Join(base, strings.TrimPrefix(direct, string(filepath.Separator)))

	if _, ok := onDisk[joined]; ok {
		return joined, true
	}

	return "", false
}

// withinRoot reports whether path sits under base, so only rows that claim
// to live in the upload volume are judged as missing files.
func withinRoot(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// truncateSample caps an already-sorted slice at integritySampleLimit.
func truncateSample(paths []string) []string {
	if len(paths) > integritySampleLimit {
		return paths[:integritySampleLimit]
	}

	return paths
}
