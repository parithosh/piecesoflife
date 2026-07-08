package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// normalizeWebM rewrites a WebM file in place with ffmpeg's stream copy.
//
// Browser MediaRecorder output is "streaming" WebM: it has no SeekHead, no
// Cues, and an unknown duration. Chromium's duration probe on such files can
// fail outright ("FFmpegDemuxer: demuxer seek failed"), leaving a dead black
// tile, and seeking never works. A remux (-c copy — no re-encode, so it runs
// in well under a second for typical recordings) writes the missing index
// and duration.
//
// Best-effort by design: if ffmpeg is not installed or the remux fails, the
// original file is kept untouched and the problem is logged — an imperfect
// video beats a failed upload.
func (s *Server) normalizeWebM(ctx context.Context, path string) {
	if !strings.HasSuffix(strings.ToLower(path), ".webm") {
		return
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		s.logger.DebugContext(ctx, "ffmpeg not available — skipping WebM remux",
			slog.String("path", path))
		return
	}

	tmp := path + ".remux.webm"

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-i", path, "-c", "copy", "-y", tmp,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		s.logger.WarnContext(ctx, "WebM remux failed — keeping original upload",
			slog.String("path", path),
			slog.String("error", err.Error()),
			slog.String("ffmpeg", strings.TrimSpace(string(out))))
		_ = os.Remove(tmp)

		return
	}

	if err := os.Rename(tmp, path); err != nil {
		s.logger.WarnContext(ctx, "Failed to swap remuxed WebM into place",
			slog.String("path", path),
			slog.String("error", err.Error()))
		_ = os.Remove(tmp)

		return
	}

	s.logger.InfoContext(ctx, "Remuxed WebM upload",
		slog.String("path", path))
}

// transcodeHEIC converts a HEIF-family photo upload (HEIC or AVIF) to JPEG
// with libheif's heif-convert, removes the original file, and returns the
// JPEG path. heif-convert applies HEIF rotation/mirror properties to the
// pixels and resets the copied EXIF orientation tag, so converted photos
// display upright.
//
// Android and iPhone cameras default to HEIC, but Chrome and Firefox cannot
// render it, so a stored .heic would be a broken image for most viewers.
// Unlike normalizeWebM this is therefore not best-effort: on any failure the
// caller must reject the upload.
func (s *Server) transcodeHEIC(ctx context.Context, path string) (string, error) {
	converter, err := exec.LookPath("heif-convert")
	if err != nil {
		return "", fmt.Errorf("looking up heif-convert (is libheif-tools installed?): %w", err)
	}

	out := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, converter, "-q", "90", path, out)
	if cmdOut, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("converting HEIC: %w: %s", err, strings.TrimSpace(string(cmdOut)))
	}

	// Multi-image HEIC (e.g. bursts): heif-convert writes name-1.jpg,
	// name-2.jpg, … instead of name.jpg. Keep the first image as the
	// upload and drop the rest.
	if _, err := os.Stat(out); err != nil {
		extras, _ := filepath.Glob(strings.TrimSuffix(out, ".jpg") + "-*.jpg")
		if len(extras) == 0 {
			return "", fmt.Errorf("converting HEIC: heif-convert produced no JPEG")
		}
		if err := os.Rename(extras[0], out); err != nil {
			return "", fmt.Errorf("renaming converted JPEG: %w", err)
		}
		for _, extra := range extras[1:] {
			_ = os.Remove(extra)
		}
	}

	if err := os.Remove(path); err != nil {
		s.logger.WarnContext(ctx, "Failed to remove original HEIC after conversion",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}

	s.logger.InfoContext(ctx, "Transcoded HEIC upload to JPEG",
		slog.String("path", out))

	return out, nil
}
