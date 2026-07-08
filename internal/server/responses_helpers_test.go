package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAllowedImageType(t *testing.T) {
	assert.True(t, isAllowedImageType("image/jpeg"))
	assert.True(t, isAllowedImageType("image/png"))
	assert.True(t, isAllowedImageType("image/webp"))
	assert.True(t, isAllowedImageType("image/heic"))

	assert.False(t, isAllowedImageType("image/gif"))
	assert.False(t, isAllowedImageType("image/svg+xml"))
	assert.False(t, isAllowedImageType("application/pdf"))
	assert.False(t, isAllowedImageType(""))
}

// ftypHeader builds an ISO-BMFF ftyp box: size, "ftyp", major brand,
// minor version, then compatible brands.
func ftypHeader(major string, compatible ...string) []byte {
	size := 16 + 4*len(compatible)
	buf := make([]byte, 0, size)
	buf = append(buf, byte(size>>24), byte(size>>16), byte(size>>8), byte(size))
	buf = append(buf, "ftyp"...)
	buf = append(buf, major...)
	buf = append(buf, 0, 0, 0, 0) // minor version
	for _, brand := range compatible {
		buf = append(buf, brand...)
	}

	return buf
}

func TestIsHEIF(t *testing.T) {
	tests := []struct {
		name  string
		sniff []byte
		want  bool
	}{
		// First 32 bytes of a real Apple-produced HEIC (sips output).
		{"real apple heic", []byte("\x00\x00\x00\x20ftypheic\x00\x00\x00\x00mif1miafMiHBheic"), true},
		{"major brand heic", ftypHeader("heic"), true},
		{"major brand heix", ftypHeader("heix"), true},
		{"generic heif mif1", ftypHeader("mif1", "miaf"), true},
		{"heic only in compatible brands", ftypHeader("XXXX", "isom", "heic"), true},
		{"mp4 video", ftypHeader("isom", "iso2", "avc1", "mp41"), false},
		{"quicktime", ftypHeader("qt  "), false},
		// Real AVIF brand layout (heif-enc output: avif + mif1/miaf compat).
		{"avif", ftypHeader("avif", "mif1", "avif", "miaf"), true},
		{"avif bare", ftypHeader("avif", "av01"), true},
		{"jpeg bytes", []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), false},
		{"too short", []byte("\x00\x00\x00\x20ftyp"), false},
		{"empty", nil, false},
		// Box size field larger than the sniff buffer must not panic.
		{"truncated box", append([]byte("\x00\x00\x02\x00ftypXXXX\x00\x00\x00\x00"), "heic"...), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isHEIF(tt.sniff))
		})
	}
}

func TestNormalizedUploadContentType_Photo(t *testing.T) {
	// Photos are classified by sniffing alone; the declared header is
	// ignored (it is attacker-controlled).
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	assert.Equal(t, "image/jpeg",
		normalizedUploadContentType(photoBlockType, "image/png", jpeg))

	heic := ftypHeader("heic", "mif1")
	assert.Equal(t, "image/heic",
		normalizedUploadContentType(photoBlockType, "application/octet-stream", heic))

	// Unrecognised binary stays octet-stream and gets rejected downstream.
	assert.Equal(t, "application/octet-stream",
		normalizedUploadContentType(photoBlockType, "image/jpeg", []byte{0x01, 0x02, 0xfe, 0xff, 0x00}))
}

func TestIsAllowedRecorderTypes(t *testing.T) {
	assert.True(t, isAllowedAudioType("audio/webm"))
	assert.True(t, isAllowedAudioType("audio/ogg"))
	assert.True(t, isAllowedAudioType("audio/mp4"))
	assert.True(t, isAllowedAudioType("audio/wav"))
	assert.False(t, isAllowedAudioType("video/webm"))
	assert.False(t, isAllowedAudioType("application/octet-stream"))

	assert.True(t, isAllowedVideoType("video/webm"))
	assert.True(t, isAllowedVideoType("video/mp4"))
	assert.True(t, isAllowedVideoType("video/quicktime"))
	assert.False(t, isAllowedVideoType("audio/webm"))
	assert.False(t, isAllowedVideoType("application/octet-stream"))
}

func TestExtensionFromContentType(t *testing.T) {
	tests := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/webp": ".webp",
		"image/heic": ".heic",
		"audio/webm": ".webm",
		"audio/ogg":  ".ogg",
		"audio/mpeg": ".mp3",
		"audio/mp4":  ".m4a",
		"audio/wav":  ".wav",
		"video/webm": ".webm",
		"video/mp4":  ".mp4",

		// Unknown types fall through to ".bin" so we never write a
		// recognised extension for an unrecognised file.
		"image/gif":       ".bin",
		"application/pdf": ".bin",
		"":                ".bin",
	}

	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, extensionFromContentType(in))
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"photo.jpg", "photojpg"}, // dot removed
		{"my-pic_01", "my-pic_01"},
		{"../../etc/passwd", "etcpasswd"},
		{"hello world", "helloworld"},
		{"日本語", "日本語"},  // unicode letters kept
		{"   ", "file"}, // whitespace only → fallback
		{"", "file"},    // empty → fallback
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeFilename(tt.in))
		})
	}
}

func TestRenderCommentBody_StripsRawHTML(t *testing.T) {
	// goldmark's default config drops inline raw HTML. A pasted
	// <script> must not survive the render step.
	got := string(renderCommentBody("hello <script>alert(1)</script> world"))
	assert.NotContains(t, got, "<script>",
		"markdown renderer must drop raw <script> tags")
	assert.Contains(t, got, "hello")
}

func TestRenderCommentBody_MarkdownBasics(t *testing.T) {
	got := string(renderCommentBody("**bold** and _italic_"))
	// We don't hard-assert exact HTML since goldmark's output may evolve,
	// but we lock in the two transformations actually in use.
	assert.Contains(t, strings.ToLower(got), "<strong>bold</strong>",
		"markdown bold should render")
	assert.Contains(t, strings.ToLower(got), "<em>italic</em>",
		"markdown italic should render")
}
