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

// Healthy reports whether disk and database agree exactly.
func (r *UploadIntegrityReport) Healthy() bool {
	return r.Orphaned == 0 && r.Missing == 0
}

// ReferencedFilePaths returns every upload path any row points at, across
// answers, the photo dump, private rambles, and notebook spreads.
func (s *Store) ReferencedFilePaths(ctx context.Context) (map[string]struct{}, error) {
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

// CheckUploadIntegrity walks uploadPath and compares it against the
// referenced set. Read-only: it never deletes, it only reports. A missing
// upload directory yields an empty report rather than an error, so a fresh
// install does not fail its first boot.
func (s *Store) CheckUploadIntegrity(
	ctx context.Context, uploadPath string,
) (*UploadIntegrityReport, error) {
	referenced, err := s.ReferencedFilePaths(ctx)
	if err != nil {
		return nil, err
	}

	base := filepath.Clean(uploadPath)

	onDisk := make(map[string]struct{}, len(referenced))

	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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

	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, fmt.Errorf("walking upload directory %s: %w", base, walkErr)
	}

	report := &UploadIntegrityReport{
		Referenced: len(referenced),
		OnDisk:     len(onDisk),
	}

	orphans := make([]string, 0, integritySampleLimit)

	for path := range onDisk {
		if _, ok := referenced[path]; ok {
			continue
		}

		report.Orphaned++

		if len(orphans) < integritySampleLimit {
			orphans = append(orphans, path)
		}
	}

	missing := make([]string, 0, integritySampleLimit)

	for path := range referenced {
		// Only judge paths that live under the configured upload root;
		// legacy rows may carry absolute paths from an older layout.
		if _, ok := onDisk[path]; ok {
			continue
		}

		if _, statErr := os.Stat(path); statErr == nil {
			continue
		}

		report.Missing++

		if len(missing) < integritySampleLimit {
			missing = append(missing, path)
		}
	}

	// Deterministic samples: map iteration order would make log lines and
	// tests flap.
	sort.Strings(orphans)
	sort.Strings(missing)

	report.OrphanSample = orphans
	report.MissingSample = missing

	return report, nil
}
