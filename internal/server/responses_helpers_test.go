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

	assert.False(t, isAllowedImageType("image/gif"))
	assert.False(t, isAllowedImageType("image/svg+xml"))
	assert.False(t, isAllowedImageType("application/pdf"))
	assert.False(t, isAllowedImageType(""))
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
