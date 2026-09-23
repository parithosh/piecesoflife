package theme

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tokenMap(p Palette) map[string]string {
	m := make(map[string]string, len(p.Tokens))
	for _, t := range p.Tokens {
		m[t.Name] = t.Value
	}

	return m
}

// The house palette must equal pallu.css's :root defaults, or a circle that
// merely picks a Rani override would shift every derived colour.
func TestRaniMatchesPalluDefaults(t *testing.T) {
	css, err := os.ReadFile("../../static/pallu.css")
	require.NoError(t, err)

	root := regexp.MustCompile(`(?s):root\s*\{\s*--rani:.*?\}`).Find(css)
	require.NotNil(t, root, "pallu.css :root token block")
	decl := regexp.MustCompile(`--([a-z0-9-]+):\s*([^;]+);`)

	defaults := map[string]string{}
	for _, m := range decl.FindAllSubmatch(root, -1) {
		defaults[string(m[1])] = strings.TrimSpace(string(m[2]))
	}

	p, err := Resolve(nil)
	require.NoError(t, err)
	for _, tok := range p.Tokens {
		want, ok := defaults[tok.Name]
		require.True(t, ok, "token --%s missing from pallu.css :root", tok.Name)
		assert.Equal(t, strings.ReplaceAll(want, "var(--rani-rgb)", "122, 15, 56"), tok.Value, "--%s", tok.Name)
	}
	assert.Empty(t, p.Adjustments)
}

// Presets are hand-tuned: the readability guard must not have to touch
// their own seeds, and every guarded pair must hold.
func TestFabricsPassUnadjusted(t *testing.T) {
	for _, f := range Fabrics {
		t.Run(f.ID, func(t *testing.T) {
			p, err := Resolve(&Config{Fabric: f.ID})
			require.NoError(t, err)
			m := tokenMap(p)
			assert.Equal(t, f.Main, m["rani"])
			assert.Equal(t, f.Highlight, m["marigold"])
			assert.Equal(t, f.Second, m["peacock"])
			assert.Equal(t, f.Accent, m["sindoor"])
			assertReadable(t, m)
		})
	}
}

func assertReadable(t *testing.T, m map[string]string) {
	t.Helper()
	pairs := []struct {
		fg, bg string
		min    float64
	}{
		{"rani", "ivory", minText},
		{"on-rani", "rani", minText},
		{"marigold", "rani", minText},
		{"ink", "marigold", minText},
		{"peacock", "ivory", minText},
		{"nav-muted", "rani", minText},
		{"on-peacock", "peacock", minText},
		{"ledger-fg", "peacock", minLedgerFg},
		{"peacock-ink", "peacock-wash", minText},
		{"sindoor-ink", "sindoor-wash", minText},
		{"ink-soft", "ivory", minSmallMeta},
		{"marigold", "peacock", minQuietTag},
		{"sindoor", "ivory", minText},
	}
	for _, pr := range pairs {
		got := contrast(mustHex(m[pr.fg]), mustHex(m[pr.bg]))
		assert.GreaterOrEqual(t, got, pr.min, "%s on %s", pr.fg, pr.bg)
	}
	white := rgb{1, 1, 1}
	assert.GreaterOrEqual(t, contrast(white, mustHex(m["peacock"])), minText, "white on peacock")
	assert.GreaterOrEqual(t, contrast(white, mustHex(m["sindoor"])), minText, "white on sindoor")
}

// Adversarial pick combinations that each satisfy some guards by being on
// the "wrong side" of a ground must still come out readable everywhere.
func TestExtremePicksStayReadable(t *testing.T) {
	for _, c := range []Config{
		{Fabric: "rani", Main: "#000000", Highlight: "#8f8f8f", Second: "#555555"},
		{Fabric: "nordic", Main: "#6a6a6a", Highlight: "#000000"},
		{Fabric: "kilim", Main: "#ffffff", Highlight: "#ffffff", Second: "#ffffff"},
		{Fabric: "coastal", Main: "#000000", Highlight: "#000000", Second: "#000000"},
	} {
		p, err := Resolve(&c)
		require.NoError(t, err)
		assertReadable(t, tokenMap(p))
	}
}

// Picks that would be unreadable are corrected, reported, and the result
// passes; readable picks are kept verbatim.
func TestUnreadablePicksAreAdjustedAndReported(t *testing.T) {
	p, err := Resolve(&Config{Fabric: "coastal", Main: "#ffe066", Highlight: "#301020", Second: "#f0f0f0"})
	require.NoError(t, err)

	got := map[string]Adjustment{}
	for _, a := range p.Adjustments {
		got[a.Knob] = a
	}
	require.Len(t, got, 3)
	assert.Equal(t, "#ffe066", got[KnobMain].Picked)
	assert.Equal(t, tokenMap(p)["rani"], got[KnobMain].Used)
	assert.Equal(t, p.Main, got[KnobMain].Used)
	assertReadable(t, tokenMap(p))

	ok, err := Resolve(&Config{Fabric: "rani", Main: "#1b4d3e"})
	require.NoError(t, err)
	assert.Empty(t, ok.Adjustments)
	assert.Equal(t, "#1b4d3e", ok.Main)
	assertReadable(t, tokenMap(ok))
}

func TestNormalize(t *testing.T) {
	house, err := Config{Fabric: "rani", Main: "#7A0F38"}.Normalize()
	require.NoError(t, err)
	assert.Nil(t, house, "Rani with its own colour is the house look")

	c, err := Config{Fabric: "indigo", Main: " #ABC ", Highlight: "#d4a843",
		Photo: &Photo{Path: "p.jpg", X: 140, Y: -3}}.Normalize()
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "#aabbcc", c.Main)
	assert.Empty(t, c.Highlight, "override equal to the fabric colour is dropped")
	assert.Equal(t, Photo{Path: "p.jpg", X: 100, Y: 0}, *c.Photo)
	assert.Equal(t, ConfigVersion, c.Version)

	_, err = Config{Fabric: "tartan"}.Normalize()
	assert.Error(t, err)
	_, err = Config{Fabric: "rani", Second: "red"}.Normalize()
	assert.Error(t, err)
}

func TestValidHex(t *testing.T) {
	for in, want := range map[string]bool{
		"#2d5016": true, "#FFF": true, "#abcdef": true, "#AABBCC": true,
		"2d5016": false, "#2d501": false, "#2d50166": false, "#2d501g": false,
		"": false, "#": false, "#GGG": false, "#2d5016 ": false, "#+ab": false,
	} {
		assert.Equal(t, want, ValidHex(in), "%q", in)
	}
}
