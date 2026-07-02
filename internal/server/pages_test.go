package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/store"
)

func TestReadJSONRejectsOversizedBody(t *testing.T) {
	body := `{"value":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Value string `json:"value"`
	}

	err := readJSON(req, &dst)

	var maxErr *http.MaxBytesError
	assert.True(t, errors.As(err, &maxErr), "expected MaxBytesError, got %v", err)
}

func TestFormatRelativePastAndFuture(t *testing.T) {
	assert.Equal(t, "2 hours ago", formatPastRelative(2*time.Hour+5*time.Minute))
	assert.Equal(t, "in 2 hours", formatFutureRelative(2*time.Hour+5*time.Minute))
	assert.Equal(t, "1 day ago", formatPastRelative(25*time.Hour))
	assert.Equal(t, "in 1 day", formatFutureRelative(25*time.Hour))
}

func TestUploadURL(t *testing.T) {
	s := &Server{
		config: &config.Config{UploadPath: "/data/uploads"},
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"disk path is rewritten", "/data/uploads/2026/04/abc.jpg", "/uploads/2026/04/abc.jpg"},
		{"already browser URL passes through", "/uploads/2026/04/abc.jpg", "/uploads/2026/04/abc.jpg"},
		{"external http URL passes through", "https://cdn.example.com/a.jpg", "https://cdn.example.com/a.jpg"},
		{"empty returns empty", "", ""},
		{"unrelated path left alone", "/something/else.jpg", "/something/else.jpg"},
		// Trailing-slash variant: filepath.Clean on "/data/uploads" keeps it
		// as "/data/uploads", so a path with exactly that prefix works.
		{"path at base root", "/data/uploads/a.jpg", "/uploads/a.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, s.uploadURL(tt.in))
		})
	}
}

// Exercise the sibling-prefix defence indirectly — an attacker-controlled
// caller can't pass an on-disk path that happens to share a prefix with
// the upload dir and have it rewritten as a valid URL.
func TestUploadURL_SiblingPrefix(t *testing.T) {
	s := &Server{
		config: &config.Config{UploadPath: "/data/uploads"},
	}
	// "/data/uploads2/x" is NOT inside "/data/uploads" — it should be
	// returned unchanged (the template then renders it literally and the
	// browser 404s, which is the correct failure mode).
	got := s.uploadURL("/data/uploads2/x.jpg")
	assert.Equal(t, "/data/uploads2/x.jpg", got,
		"sibling-prefix path must not be rewritten into /uploads/")
}

func TestTruncateWords(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short string unchanged", "hello world", 100, "hello world"},
		{"exact length unchanged", "hello", 5, "hello"},
		// cut[:13] = "one two three"; LastIndex(" ") = 7 > max/2 = 6, so trim
		// back to "one two" before appending "…".
		{"cuts at word boundary", "one two three four five", 13, "one two…"},
		{"falls back to hard cut when no good word boundary", "aaaaaaaaaaaaaaaa", 5, "aaaaa…"},
		{"trims trailing whitespace before ellipsis", "one two    three", 8, "one two…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncateWords(tt.in, tt.max))
		})
	}
}

func TestSummarizeBlocks(t *testing.T) {
	txt := func(s string) *string { return &s }

	t.Run("empty", func(t *testing.T) {
		assert.Equal(t, "", summarizeBlocks(nil, 100))
	})

	t.Run("first text block wins", func(t *testing.T) {
		blocks := []store.ResponseBlock{
			{Type: "photo"},
			{Type: "text", Content: txt("first text")},
			{Type: "text", Content: txt("second text")},
		}
		assert.Equal(t, "first text", summarizeBlocks(blocks, 100))
	})

	t.Run("skips text blocks with nil content", func(t *testing.T) {
		blocks := []store.ResponseBlock{
			{Type: "text", Content: nil},
			{Type: "text", Content: txt("actual")},
		}
		// The nil-content block is skipped by the Content != nil check,
		// so the summary comes from the next usable text block.
		assert.Equal(t, "actual", summarizeBlocks(blocks, 100))
	})

	t.Run("truncates long text", func(t *testing.T) {
		long := "one two three four five six seven eight nine ten"
		blocks := []store.ResponseBlock{{Type: "text", Content: &long}}
		got := summarizeBlocks(blocks, 20)
		assert.LessOrEqual(t, len(got), len(long))
		assert.Contains(t, got, "…")
	})
}
