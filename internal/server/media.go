package server

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
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
