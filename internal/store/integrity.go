package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// integritySampleLimit caps how many example paths a report carries. Enough
// to name the files in a log line, not enough to flood it.
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
// Missing is the opposite failure: rows pointing at files that are gone,
// i.e. a lost or partially copied uploads volume.
type UploadIntegrityReport struct {
	Referenced    int
	OnDisk        int
	Orphaned      int
	Missing       int
	OrphanSample  []string
	MissingSample []string
}

// CheckUploadIntegrity walks uploadPath and compares it against the paths
// rows reference. Read-only: it never deletes, it only reports. A missing
// upload directory yields an empty report rather than an error, so a fresh
// install does not fail its first boot.
//
// Every upload the app has ever written is an absolute path under the
// configured root (see receiveMediaUpload), so plain set comparison is
// enough — no path interpretation, and a row pointing anywhere else counts
// as missing, which is what it is.
func (s *Store) CheckUploadIntegrity(
	ctx context.Context, uploadPath string,
) (*UploadIntegrityReport, error) {
	referenced, err := s.referencedFilePaths(ctx)
	if err != nil {
		return nil, err
	}

	onDisk, err := walkUploads(ctx, filepath.Clean(uploadPath))
	if err != nil {
		return nil, err
	}

	report := &UploadIntegrityReport{
		Referenced: len(referenced),
		OnDisk:     len(onDisk),
	}

	missing := make([]string, 0, len(referenced))

	for path := range referenced {
		if _, ok := onDisk[path]; !ok {
			missing = append(missing, path)
		}
	}

	orphans := make([]string, 0, len(onDisk))

	for path := range onDisk {
		if _, ok := referenced[path]; !ok {
			orphans = append(orphans, path)
		}
	}

	report.Missing = len(missing)
	report.Orphaned = len(orphans)

	// Sort the full sets before truncating: sampling straight out of map
	// iteration would change which paths appear from run to run, so the
	// daily log line would churn for exactly the production case of 21
	// orphans.
	sort.Strings(orphans)
	sort.Strings(missing)

	report.OrphanSample = orphans[:min(len(orphans), integritySampleLimit)]
	report.MissingSample = missing[:min(len(missing), integritySampleLimit)]

	return report, nil
}

// referencedFilePaths returns every upload path any row points at, across
// answers, the photo dump, private rambles, notebook spreads, and circle
// photos/banners (published look and its restore point).
func (s *Store) referencedFilePaths(ctx context.Context) (map[string]struct{}, error) {
	// A malformed theme value must not abort the whole report: only
	// well-formed JSON objects are inspected for photo paths.
	const query = `
		WITH themes(t) AS (
			SELECT theme FROM settings
				WHERE CASE WHEN json_valid(theme) THEN json_type(theme) = 'object' ELSE 0 END
			UNION ALL SELECT theme_previous FROM settings
				WHERE CASE WHEN json_valid(theme_previous) THEN json_type(theme_previous) = 'object' ELSE 0 END
		)
		SELECT file_path FROM response_blocks WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM dump_items WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM ramble_blocks WHERE file_path IS NOT NULL
		UNION SELECT file_path FROM diary_blocks WHERE file_path IS NOT NULL
		UNION SELECT p FROM (
			SELECT json_extract(t, '$.photo.path') AS p FROM themes
			UNION SELECT json_extract(t, '$.banner.path') FROM themes
		) WHERE p IS NOT NULL
	`

	rows, err := s.read.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying referenced file paths: %w", err)
	}

	defer rows.Close()

	// Capacity hint: a mature Loop holds a few thousand media rows.
	referenced := make(map[string]struct{}, 512)

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scanning referenced file path: %w", err)
		}

		referenced[filepath.Clean(path)] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating referenced file paths: %w", err)
	}

	return referenced, nil
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
