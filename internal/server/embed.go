package server

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

// linkEmbed returns an HTML snippet embedding the given URL as a player or
// preview card when we recognise the provider. For unknown URLs it returns
// an empty string so the caller can fall through to rendering a plain link.
//
// Only a small allowlist of providers is supported. Each emitted iframe
// points at the provider's official embed endpoint, so the iframe sandbox
// only executes trusted third-party code.
func linkEmbed(raw string) template.HTML {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}

	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")

	switch {
	case host == "youtube.com" || host == "m.youtube.com" || host == "youtu.be":
		return youtubeEmbed(u, host)
	case host == "open.spotify.com":
		return spotifyEmbed(u)
	case host == "music.apple.com" || host == "embed.music.apple.com":
		return appleMusicEmbed(u)
	case host == "soundcloud.com":
		return soundcloudEmbed(u)
	}

	return ""
}

var youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,16}$`)

func youtubeEmbed(u *url.URL, host string) template.HTML {
	var id string

	if host == "youtu.be" {
		id = strings.TrimPrefix(u.Path, "/")
	} else {
		id = u.Query().Get("v")
	}

	if !youtubeIDRe.MatchString(id) {
		return ""
	}

	src := "https://www.youtube-nocookie.com/embed/" + url.PathEscape(id)
	return iframe(src, 315, `allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture" allowfullscreen`)
}

// spotifyPathRe captures /{track|album|playlist|episode|show}/{id}.
var spotifyPathRe = regexp.MustCompile(`^/(track|album|playlist|episode|show)/([A-Za-z0-9]+)`)

func spotifyEmbed(u *url.URL) template.HTML {
	m := spotifyPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return ""
	}

	src := fmt.Sprintf("https://open.spotify.com/embed/%s/%s",
		url.PathEscape(m[1]), url.PathEscape(m[2]))

	height := 152
	if m[1] == "album" || m[1] == "playlist" || m[1] == "show" {
		height = 352
	}

	return iframe(src, height, `allow="autoplay; clipboard-write; encrypted-media; fullscreen; picture-in-picture"`)
}

// applePathRe matches /{country}/{album|song|playlist}/{slug}/{id} paths.
var applePathRe = regexp.MustCompile(`^/[a-z]{2}/(album|song|playlist)/`)

func appleMusicEmbed(u *url.URL) template.HTML {
	if !applePathRe.MatchString(u.Path) {
		return ""
	}

	src := "https://embed.music.apple.com" + u.Path
	if u.RawQuery != "" {
		src += "?" + u.RawQuery
	}

	return iframe(src, 175, `allow="autoplay *; encrypted-media *; fullscreen *; clipboard-write"`)
}

func soundcloudEmbed(u *url.URL) template.HTML {
	// SoundCloud requires URL-encoded passthrough to w.soundcloud.com.
	if u.Path == "" || u.Path == "/" {
		return ""
	}

	src := "https://w.soundcloud.com/player/?url=" + url.QueryEscape(u.String()) +
		"&color=%232d5016&inverse=false&auto_play=false&show_user=true"

	return iframe(src, 166, `allow="autoplay"`)
}

// iframe builds a sandboxed iframe tag. Caller guarantees src points at a
// known provider's embed endpoint.
func iframe(src string, height int, extraAttrs string) template.HTML {
	safeSrc := html.EscapeString(src)
	return template.HTML(fmt.Sprintf(
		`<div class="link-embed"><iframe src="%s" height="%d" `+
			`style="width:100%%;height:%dpx;border:0;border-radius:6px;" `+
			`loading="lazy" referrerpolicy="strict-origin-when-cross-origin" `+
			`%s></iframe></div>`,
		safeSrc, height, height, extraAttrs,
	))
}
