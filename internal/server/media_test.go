package server

import (
	"context"
	"image"
	"image/color"
	"image/png"
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

// TestTranscodeHEIC verifies a real HEIC (encoded with libheif's own
// heif-enc) is converted to JPEG, the original removed, and garbage input
// rejected. Skipped when the libheif tools are absent.
func TestTranscodeHEIC(t *testing.T) {
	for _, tool := range []string{"heif-enc", "heif-convert"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	s := &Server{
		config: &config.Config{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	dir := t.TempDir()

	// A tiny PNG, encoded to HEIC with heif-enc.
	pngPath := filepath.Join(dir, "src.png")
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.Set(3, 3, color.RGBA{R: 0xff, A: 0xff})

	pngFile, err := os.Create(pngPath)
	require.NoError(t, err)
	require.NoError(t, png.Encode(pngFile, img))
	require.NoError(t, pngFile.Close())

	heicPath := filepath.Join(dir, "photo.heic")
	gen := exec.Command("heif-enc", pngPath, "-o", heicPath)
	require.NoError(t, gen.Run(), "generate test heic")

	// The generated file must trip our sniffer, like a phone upload would.
	sniff, err := os.ReadFile(heicPath)
	require.NoError(t, err)
	require.True(t, isHEIF(sniff[:min(512, len(sniff))]), "heif-enc output sniffs as HEIF")

	jpegPath, err := s.transcodeHEIC(ctx, heicPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "photo.jpg"), jpegPath)

	jpegBytes, err := os.ReadFile(jpegPath)
	require.NoError(t, err)
	require.Greater(t, len(jpegBytes), 2)
	require.Equal(t, []byte{0xff, 0xd8}, jpegBytes[:2], "output is a JPEG")

	_, err = os.Stat(heicPath)
	require.True(t, os.IsNotExist(err), "original HEIC removed after conversion")

	// Garbage input: error returned, nothing written.
	bad := filepath.Join(dir, "junk.heic")
	require.NoError(t, os.WriteFile(bad, []byte("not a heic"), 0o644))
	_, err = s.transcodeHEIC(ctx, bad)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "junk.jpg"))
	require.True(t, os.IsNotExist(statErr), "no JPEG written for garbage input")

	// Multi-image HEIC: heif-convert writes name-1.jpg/name-2.jpg with no
	// name.jpg — the first image must be kept, the extras removed.
	multiPath := filepath.Join(dir, "burst.heic")
	gen = exec.Command("heif-enc", pngPath, pngPath, "-o", multiPath)
	require.NoError(t, gen.Run(), "generate multi-image heic")

	jpegPath, err = s.transcodeHEIC(ctx, multiPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "burst.jpg"), jpegPath)
	_, err = os.Stat(jpegPath)
	require.NoError(t, err, "first burst image kept")
	extras, err := filepath.Glob(filepath.Join(dir, "burst-*.jpg"))
	require.NoError(t, err)
	require.Empty(t, extras, "extra burst images removed")

	// AVIF goes through the same converter. Not all libheif builds ship an
	// AV1 encoder, so skip (not fail) if the sample can't be generated.
	avifPath := filepath.Join(dir, "photo.avif")
	if err := exec.Command("heif-enc", "-A", pngPath, "-o", avifPath).Run(); err != nil {
		t.Skipf("heif-enc cannot encode AVIF: %v", err)
	}

	sniff, err = os.ReadFile(avifPath)
	require.NoError(t, err)
	require.True(t, isHEIF(sniff[:min(512, len(sniff))]), "AVIF sniffs as HEIF-family")

	jpegPath, err = s.transcodeHEIC(ctx, avifPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "photo.jpg"), jpegPath)
}
