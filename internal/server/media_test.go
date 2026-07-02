package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/config"
)

// TestNormalizeWebM verifies the remux rewrites a WebM in place and leaves
// non-WebM and garbage inputs untouched. Skipped when ffmpeg is absent.
func TestNormalizeWebM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	s := &Server{
		config: &config.Config{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	dir := t.TempDir()

	// A real (tiny) WebM produced by ffmpeg.
	src := filepath.Join(dir, "clip.webm")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=5:duration=1",
		"-c:v", "libvpx", "-y", src)
	require.NoError(t, gen.Run(), "generate test webm")

	before, err := os.Stat(src)
	require.NoError(t, err)

	s.normalizeWebM(ctx, src)

	after, err := os.Stat(src)
	require.NoError(t, err)
	require.Greater(t, after.Size(), int64(0))
	require.NotEqual(t, before.ModTime(), after.ModTime(), "file was rewritten")

	// Garbage input: original preserved, no temp file left behind.
	bad := filepath.Join(dir, "junk.webm")
	require.NoError(t, os.WriteFile(bad, []byte("not a webm"), 0o644))
	s.normalizeWebM(ctx, bad)

	content, err := os.ReadFile(bad)
	require.NoError(t, err)
	require.Equal(t, "not a webm", string(content), "failed remux keeps original")

	leftovers, err := filepath.Glob(filepath.Join(dir, "*.remux.webm"))
	require.NoError(t, err)
	require.Empty(t, leftovers, "no temp files left behind")

	// Non-webm path: untouched.
	mp4 := filepath.Join(dir, "clip.mp4")
	require.NoError(t, os.WriteFile(mp4, []byte("mp4-bytes"), 0o644))
	s.normalizeWebM(ctx, mp4)
	content, err = os.ReadFile(mp4)
	require.NoError(t, err)
	require.Equal(t, "mp4-bytes", string(content))
}
