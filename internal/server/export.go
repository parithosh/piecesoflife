package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// exportRoot is the top-level JSON shape for /api/admin/export.
type exportRoot struct {
	Version     int                     `json:"version"`
	ExportedAt  time.Time               `json:"exported_at"`
	LoopName    string                  `json:"loop_name"`
	Settings    *store.Settings         `json:"settings"`
	Users       []store.GroupMember     `json:"users"`
	Preferences []exportPrefs           `json:"notification_preferences"`
	Issues      []exportIssue           `json:"issues"`
	Questions   []store.Question        `json:"questions"`
	Responses   []exportResponse        `json:"responses"`
	Comments    []store.CommentWithUser `json:"comments"`
	// DiarySections are the notebook spreads members attached to this
	// Loop's issues — snapshot copies only, never the private journals.
	DiarySections []exportDiarySection `json:"diary_sections"`
}

type exportPrefs struct {
	*store.NotificationPreferences
}

type exportIssue struct {
	store.Issue

	PhotoURLs []string `json:"photo_urls,omitempty"`
}

type exportResponse struct {
	store.Response

	Blocks []exportBlock `json:"blocks"`
}

type exportBlock struct {
	store.ResponseBlock

	// URL is the browser-accessible path for photo blocks. Useful when the
	// export is consumed by something that wants to fetch the images too.
	URL string `json:"url,omitempty"`
}

type exportDiarySection struct {
	store.DiarySection

	UserName string           `json:"user_name"`
	Days     []exportDiaryDay `json:"days"`
}

type exportDiaryDay struct {
	store.DiaryDay

	Blocks []exportDiaryBlock `json:"blocks"`
}

type exportDiaryBlock struct {
	store.DiaryBlock

	URL string `json:"url,omitempty"`
}

// handleExport produces a JSON snapshot of the entire loop. Admin-only.
// GET /api/admin/export
//
// The response is streamed as application/json with an attachment disposition
// so browsers download it rather than render. Intentionally not paginated:
// this is meant for small-group newsletters where a single file is fine.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	payload, err := s.buildExportPayload(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Export: build payload failed",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to build export")

		return
	}

	filename := fmt.Sprintf("piecesoflife-export-%s.json",
		time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filename))

	s.logger.InfoContext(ctx, "Export generated",
		slog.Int("issues", len(payload.Issues)),
		slog.Int("responses", len(payload.Responses)),
		slog.Int("comments", len(payload.Comments)),
	)

	writeJSON(w, http.StatusOK, payload)
}

// handleExportZip streams a zip archive of the entire loop. Admin-only.
// GET /api/admin/export.zip
//
// The archive contains README.txt, export.json (identical payload to
// /api/admin/export), and an uploads/ directory with every file referenced
// by a response block that still exists on disk, keyed by its path relative
// to the configured UPLOAD_PATH. Missing or out-of-tree files are skipped
// (and counted) rather than failing the whole export.
func (s *Server) handleExportZip(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	payload, err := s.buildExportPayload(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Zip export: build payload failed",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to build export")

		return
	}

	filename := fmt.Sprintf("piecesoflife-export-%s.zip",
		time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)

	// From here on the response is committed: mid-stream failures can only
	// be logged, not turned into an error status.
	zw := zip.NewWriter(w)

	if err := writeZipReadme(zw, payload.ExportedAt); err != nil {
		s.logger.ErrorContext(ctx, "Zip export: write README failed",
			slog.String("error", err.Error()))

		return
	}

	if err := writeZipExportJSON(zw, payload); err != nil {
		s.logger.ErrorContext(ctx, "Zip export: write export.json failed",
			slog.String("error", err.Error()))

		return
	}

	included, skipped := s.writeZipUploads(ctx, zw, payload)

	if err := zw.Close(); err != nil {
		s.logger.ErrorContext(ctx, "Zip export: finalize archive failed",
			slog.String("error", err.Error()))

		return
	}

	s.logger.InfoContext(ctx, "Zip export generated",
		slog.Int("issues", len(payload.Issues)),
		slog.Int("responses", len(payload.Responses)),
		slog.Int("comments", len(payload.Comments)),
		slog.Int("files_included", included),
		slog.Int("files_skipped", skipped),
	)
}

// buildExportPayload assembles the full export snapshot shared by the JSON
// and zip export handlers. Per-issue failures are logged and skipped so one
// bad row can't sink the whole export; only top-level queries are fatal.
func (s *Server) buildExportPayload(
	ctx context.Context, groupID int64,
) (*exportRoot, error) {
	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}

	users, err := s.store.ListGroupMembers(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}

	prefs := make([]exportPrefs, 0, len(users))

	for _, u := range users {
		p, err := s.store.GetNotificationPreferences(ctx, u.ID)
		if err != nil {
			continue
		}

		prefs = append(prefs, exportPrefs{NotificationPreferences: p})
	}

	issues, err := s.store.ListIssues(ctx, groupID, nil)
	if err != nil {
		return nil, fmt.Errorf("listing issues: %w", err)
	}

	questions := make([]store.Question, 0)
	responses := make([]exportResponse, 0)
	comments := make([]store.CommentWithUser, 0)
	diarySections := make([]exportDiarySection, 0, 8)
	exportIssues := make([]exportIssue, 0, len(issues))

	for _, iss := range issues {
		qs, err := s.store.ListQuestionsByIssue(ctx, iss.ID)
		if err != nil {
			s.logger.WarnContext(ctx, "Export: list questions failed",
				slog.Int64("issue_id", iss.ID),
				slog.String("error", err.Error()))

			continue
		}

		questions = append(questions, qs...)

		photoURLs := []string{}

		photos, err := s.store.ListPhotosForIssue(ctx, iss.ID, 100)
		if err == nil {
			for _, p := range photos {
				photoURLs = append(photoURLs, s.uploadURL(p))
			}
		}

		exportIssues = append(exportIssues, exportIssue{
			Issue:     iss,
			PhotoURLs: photoURLs,
		})

		rs, err := s.store.ListResponsesByIssue(ctx, iss.ID, false)
		if err != nil {
			s.logger.WarnContext(ctx, "Export: list responses failed",
				slog.Int64("issue_id", iss.ID),
				slog.String("error", err.Error()))

			continue
		}

		for _, resp := range rs {
			blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
			if err != nil {
				continue
			}

			exBlocks := make([]exportBlock, 0, len(blocks))

			for _, b := range blocks {
				url := ""
				if b.Type == "photo" && b.FilePath != nil {
					url = s.uploadURL(*b.FilePath)
				}

				exBlocks = append(exBlocks, exportBlock{
					ResponseBlock: b,
					URL:           url,
				})
			}

			responses = append(responses, exportResponse{
				Response: resp,
				Blocks:   exBlocks,
			})

			// Comments for this response.
			cs, err := s.store.ListCommentsByResponse(ctx, resp.ID)
			if err == nil {
				comments = append(comments, cs...)
			}
		}

		// Notebook spreads attached to this issue, with their day comments.
		groups, err := s.store.ListDiarySectionsByIssue(ctx, iss.ID)
		if err != nil {
			s.logger.WarnContext(ctx, "Export: list diary sections failed",
				slog.Int64("issue_id", iss.ID),
				slog.String("error", err.Error()))

			continue
		}

		for _, g := range groups {
			days := make([]exportDiaryDay, 0, len(g.Days))

			for _, d := range g.Days {
				blocks := make([]exportDiaryBlock, 0, len(d.Blocks))

				for _, b := range d.Blocks {
					url := ""
					if b.FilePath != nil && b.Type != "text" {
						url = s.uploadURL(*b.FilePath)
					}

					blocks = append(blocks, exportDiaryBlock{DiaryBlock: b, URL: url})
				}

				days = append(days, exportDiaryDay{DiaryDay: d.DiaryDay, Blocks: blocks})

				cs, cErr := s.store.ListCommentsByDiaryDay(ctx, d.DiaryDay.ID)
				if cErr == nil {
					comments = append(comments, cs...)
				}
			}

			diarySections = append(diarySections, exportDiarySection{
				DiarySection: g.Section,
				UserName:     g.UserName,
				Days:         days,
			})
		}
	}

	return &exportRoot{
		Version:       1,
		ExportedAt:    time.Now().UTC(),
		LoopName:      settings.LoopName,
		Settings:      settings,
		Users:         users,
		Preferences:   prefs,
		Issues:        exportIssues,
		Questions:     questions,
		Responses:     responses,
		Comments:      comments,
		DiarySections: diarySections,
	}, nil
}

// writeZipUploads copies every on-disk file referenced by the payload's
// response and diary blocks into the archive under uploads/, preserving each
// file's path relative to the configured upload directory. Returns how many
// files were included and how many were skipped (missing on disk,
// duplicate-safe, or resolving outside the upload directory).
func (s *Server) writeZipUploads(
	ctx context.Context, zw *zip.Writer, payload *exportRoot,
) (included, skipped int) {
	base := filepath.Clean(s.config.UploadPath)
	seen := make(map[string]struct{}, len(payload.Responses))

	for _, resp := range payload.Responses {
		for _, b := range resp.Blocks {
			s.zipOneUpload(ctx, zw, base, seen, b.ID, b.FilePath,
				&included, &skipped)
		}
	}

	for _, section := range payload.DiarySections {
		for _, d := range section.Days {
			for _, b := range d.Blocks {
				s.zipOneUpload(ctx, zw, base, seen, b.ID, b.FilePath,
					&included, &skipped)
			}
		}
	}

	return included, skipped
}

// zipOneUpload streams a single referenced upload into the archive, applying
// the dedupe and path-escape defenses shared by every block source.
func (s *Server) zipOneUpload(
	ctx context.Context, zw *zip.Writer,
	base string, seen map[string]struct{},
	blockID int64, filePath *string,
	included, skipped *int,
) {
	if filePath == nil || *filePath == "" {
		return
	}

	cleanPath := filepath.Clean(*filePath)
	if _, dup := seen[cleanPath]; dup {
		return
	}

	seen[cleanPath] = struct{}{}

	// Reject anything that could resolve outside the upload directory —
	// same defense as handleUploadServe and handleMementoFile.
	rel, err := filepath.Rel(base, cleanPath)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		s.logger.WarnContext(ctx, "Zip export: path outside upload dir skipped",
			slog.Int64("block_id", blockID))

		(*skipped)++

		return
	}

	if err := addFileToZip(zw, cleanPath, rel); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.WarnContext(ctx, "Zip export: upload copy failed",
				slog.Int64("block_id", blockID),
				slog.String("error", err.Error()))
		}

		(*skipped)++

		return
	}

	(*included)++
}

// writeZipExportJSON adds export.json (the same payload as the plain JSON
// export, pretty-printed) to the archive.
func writeZipExportJSON(zw *zip.Writer, payload *exportRoot) error {
	entry, err := zw.CreateHeader(&zip.FileHeader{
		Name:     "export.json",
		Method:   zip.Deflate,
		Modified: payload.ExportedAt,
	})
	if err != nil {
		return fmt.Errorf("creating export.json entry: %w", err)
	}

	enc := json.NewEncoder(entry)
	enc.SetIndent("", "  ")

	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encoding export.json: %w", err)
	}

	return nil
}

// writeZipReadme adds a README.txt describing the archive layout.
func writeZipReadme(zw *zip.Writer, exportedAt time.Time) error {
	entry, err := zw.CreateHeader(&zip.FileHeader{
		Name:     "README.txt",
		Method:   zip.Deflate,
		Modified: exportedAt,
	})
	if err != nil {
		return fmt.Errorf("creating README.txt entry: %w", err)
	}

	readme := fmt.Sprintf(`Pieces of Life — data export
Exported: %s

Contents
--------
export.json   Full JSON snapshot of the loop: settings, members,
              notification preferences, issues, questions, responses
              (with their content blocks), comments, and the notebook
              (diary) spreads members attached to issues. Identical to
              the payload served by GET /api/admin/export.
uploads/      Every uploaded file (photos, audio, video) referenced by
              a response or notebook block, laid out with the same
              relative paths the app stores under its upload directory.
              Files that no longer exist on disk are skipped.

Note
----
This archive is a portable snapshot for reading and safekeeping. The
SQLite database file on the server remains the canonical backup — copy
that file directly if you want a byte-for-byte restorable backup.
`, exportedAt.Format(time.RFC3339))

	if _, err := io.WriteString(entry, readme); err != nil {
		return fmt.Errorf("writing README.txt: %w", err)
	}

	return nil
}

// addFileToZip streams one on-disk upload into the archive as
// uploads/<relPath>. Uploads are already-compressed media, so they are
// stored rather than re-deflated.
func addFileToZip(zw *zip.Writer, absPath, relPath string) error {
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("opening upload: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stating upload: %w", err)
	}

	if info.IsDir() {
		return fmt.Errorf("upload path %q is a directory", relPath)
	}

	entry, err := zw.CreateHeader(&zip.FileHeader{
		Name:     "uploads/" + filepath.ToSlash(relPath),
		Method:   zip.Store,
		Modified: info.ModTime(),
	})
	if err != nil {
		return fmt.Errorf("creating zip entry: %w", err)
	}

	if _, err := io.Copy(entry, f); err != nil {
		return fmt.Errorf("copying upload into zip: %w", err)
	}

	return nil
}
