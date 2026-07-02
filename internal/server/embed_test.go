package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLinkEmbed(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantSrc  string // substring expected in the rendered iframe src
		wantNone bool   // true if we expect an empty HTML result (no embed)
	}{
		// YouTube
		{
			name:    "youtube watch URL",
			url:     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantSrc: "youtube-nocookie.com/embed/dQw4w9WgXcQ",
		},
		{
			name:    "youtu.be short URL",
			url:     "https://youtu.be/dQw4w9WgXcQ",
			wantSrc: "youtube-nocookie.com/embed/dQw4w9WgXcQ",
		},
		{
			name:     "youtube without v param",
			url:      "https://www.youtube.com/",
			wantNone: true,
		},
		{
			name:     "youtube watch with malformed id",
			url:      "https://www.youtube.com/watch?v=$$$$",
			wantNone: true,
		},

		// Spotify
		{
			name:    "spotify track",
			url:     "https://open.spotify.com/track/3n3Ppam7vgaVa1iaRUc9Lp",
			wantSrc: "open.spotify.com/embed/track/3n3Ppam7vgaVa1iaRUc9Lp",
		},
		{
			name:    "spotify album",
			url:     "https://open.spotify.com/album/abc123",
			wantSrc: "open.spotify.com/embed/album/abc123",
		},
		{
			name:    "spotify playlist",
			url:     "https://open.spotify.com/playlist/xyz789",
			wantSrc: "open.spotify.com/embed/playlist/xyz789",
		},
		{
			name:     "spotify homepage",
			url:      "https://open.spotify.com/",
			wantNone: true,
		},
		{
			name:     "spotify unknown path",
			url:      "https://open.spotify.com/weirdthing/abc",
			wantNone: true,
		},

		// Apple Music
		{
			name:    "apple music album",
			url:     "https://music.apple.com/us/album/some-slug/1234567890",
			wantSrc: "embed.music.apple.com/us/album/some-slug/1234567890",
		},
		{
			name:     "apple music without country",
			url:      "https://music.apple.com/album/name",
			wantNone: true,
		},

		// SoundCloud
		{
			name:    "soundcloud track",
			url:     "https://soundcloud.com/some-user/some-track",
			wantSrc: "w.soundcloud.com/player",
		},

		// Unknown providers
		{
			name:     "plain website",
			url:      "https://example.com/something",
			wantNone: true,
		},
		{
			name:     "empty string",
			url:      "",
			wantNone: true,
		},
		{
			name:     "not https",
			url:      "ftp://ftp.example.com/file",
			wantNone: true,
		},
		{
			name:     "whitespace",
			url:      "   ",
			wantNone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := linkEmbed(tt.url)
			gotStr := string(got)

			if tt.wantNone {
				assert.Empty(t, gotStr, "expected no embed for %q, got %q", tt.url, gotStr)
				return
			}

			assert.Contains(t, gotStr, tt.wantSrc,
				"embed src for %q should contain %q", tt.url, tt.wantSrc)
			assert.Contains(t, gotStr, "<iframe",
				"embed should be an iframe")
		})
	}
}

// TestLinkEmbedEscaping ensures the generated iframe src attribute is always
// HTML-escaped so a crafted URL can't break out of the attribute.
func TestLinkEmbedEscaping(t *testing.T) {
	// An id like `"><script>alert(1)</script>` would break out of the
	// attribute if we didn't escape. YouTube IDs are regex-bounded so
	// this URL should just not embed at all — but let's lock that in.
	got := linkEmbed(`https://www.youtube.com/watch?v="><img src=x>`)
	assert.NotContains(t, string(got), `"><img`,
		"raw quote must never end up in the rendered iframe")
}

// Smoke: the iframe we emit always contains a loading="lazy" hint and a
// referrerpolicy so we don't leak referrer URLs into the embed providers.
func TestLinkEmbedLazyAndReferrer(t *testing.T) {
	got := string(linkEmbed("https://www.youtube.com/watch?v=dQw4w9WgXcQ"))
	assert.True(t, strings.Contains(got, `loading="lazy"`), "expected loading=lazy")
	assert.True(t, strings.Contains(got, `referrerpolicy="strict-origin-when-cross-origin"`),
		"expected strict-origin referrer policy")
}
