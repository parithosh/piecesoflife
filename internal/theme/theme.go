// Package theme turns a circle's appearance choice (a fabric preset plus up
// to three colour overrides) into the full Pallu token set.
//
// Only six seed colours are ever chosen: main (--rani), highlight
// (--marigold), second (--peacock), accent (--sindoor), paper (--ivory) and
// ink. Every other token is derived by carrying over its perceptual
// relationship to its seed in the original Rani palette: a token that is
// "rani, 0.1 darker, a little less saturated" in Rani stays exactly that
// relative to whatever main colour a circle picks. Rani seeds therefore
// reproduce pallu.css's defaults byte for byte.
//
// Readability is enforced afterwards: the three admin-pickable colours are
// nudged (and the nudge reported) until text on and against them passes
// WCAG contrast; derived text tokens are corrected silently.
package theme

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Knob names the three colours a circle admin may override.
const (
	KnobMain      = "main"
	KnobHighlight = "highlight"
	KnobSecond    = "second"
)

// DefaultFabric is today's look; choosing it with no overrides or photos is
// the house look and renders no theme at all.
const DefaultFabric = "rani"

// Fabric is a curated preset: six seeds, everything else derived.
type Fabric struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Vibe      string `json:"vibe"`
	Main      string `json:"main"`
	Highlight string `json:"highlight"`
	Second    string `json:"second"`
	Accent    string `json:"accent"`
	Paper     string `json:"paper"`
	Ink       string `json:"ink"`
}

// Fabrics lists the presets in display order; the first is the house look.
var Fabrics = []Fabric{
	{ID: "rani", Name: "Rani Ikat", Vibe: "The original: rani silk, marigold thread, peacock on ivory.",
		Main: "#7a0f38", Highlight: "#e6b23c", Second: "#0e6b6b", Accent: "#c3362b", Paper: "#fbf4e3", Ink: "#2a0e15"},
	{ID: "indigo", Name: "Indigo Quilt", Vibe: "Deep and literary, like a lamp-lit evening.",
		Main: "#243b6b", Highlight: "#d4a843", Second: "#5b4a7a", Accent: "#b24f37", Paper: "#f6efdf", Ink: "#1c2233"},
	{ID: "coastal", Name: "Coastal Weave", Vibe: "Airy navy, sea glass and coral on sand.",
		Main: "#17435b", Highlight: "#e3b865", Second: "#2f6f6c", Accent: "#b44733", Paper: "#f4ead8", Ink: "#13222b"},
	{ID: "nordic", Name: "Nordic Linen", Vibe: "Quiet slate and flax with a rust thread.",
		Main: "#2e4549", Highlight: "#d4b676", Second: "#56694f", Accent: "#a45942", Paper: "#f5f1e8", Ink: "#1f2628"},
	{ID: "meadow", Name: "Meadow Tapestry", Vibe: "Forest green, ochre and heather: soft and outdoorsy.",
		Main: "#2c4a38", Highlight: "#dcb44e", Second: "#6b4f6e", Accent: "#af4e36", Paper: "#f5eedc", Ink: "#1d2a20"},
	{ID: "kilim", Name: "Autumn Kilim", Vibe: "Warm oxblood, amber and terracotta on oat.",
		Main: "#5f2a25", Highlight: "#dea83e", Second: "#3f5f5a", Accent: "#a14d2b", Paper: "#f1e2c9", Ink: "#2b1a14"},
}

// FabricByID looks up a preset.
func FabricByID(id string) (Fabric, bool) {
	for _, f := range Fabrics {
		if f.ID == id {
			return f, true
		}
	}

	return Fabric{}, false
}

// Config is what a circle stores: a fabric, optional colour overrides, and
// optional photos. Empty override = the fabric's own colour.
type Config struct {
	Version   int    `json:"v"`
	Fabric    string `json:"fabric"`
	Main      string `json:"main,omitempty"`
	Highlight string `json:"highlight,omitempty"`
	Second    string `json:"second,omitempty"`
	Photo     *Photo `json:"photo,omitempty"`
	Banner    *Photo `json:"banner,omitempty"`
}

// Photo is an uploaded circle image plus its focal point (percent, 0..100),
// applied as object-position/background-position so any crop keeps the
// important part in frame.
type Photo struct {
	Path string `json:"path"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
}

// ConfigVersion is the current stored-config schema version.
const ConfigVersion = 1

// ParseConfig decodes a stored config. Empty input means "no theme".
func ParseConfig(raw []byte) (*Config, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decoding theme config: %w", err)
	}

	return &c, nil
}

// Normalize validates c and canonicalises it: lower-case hex, overrides
// equal to the fabric's own colour dropped, focal points clamped. It
// returns nil when the result is the plain house look, so storing it
// renders exactly today's pages.
func (c Config) Normalize() (*Config, error) {
	f, ok := FabricByID(c.Fabric)
	if !ok {
		return nil, fmt.Errorf("unknown fabric %q", c.Fabric)
	}

	c.Version = ConfigVersion
	for _, o := range []struct {
		knob string
		v    *string
		base string
	}{
		{KnobMain, &c.Main, f.Main},
		{KnobHighlight, &c.Highlight, f.Highlight},
		{KnobSecond, &c.Second, f.Second},
	} {
		*o.v = strings.ToLower(strings.TrimSpace(*o.v))
		if *o.v == "" {
			continue
		}
		col, ok := parseHex(*o.v)
		if !ok {
			return nil, fmt.Errorf("%s colour must be a hex colour like #17435b", o.knob)
		}
		*o.v = col.hex()
		if *o.v == o.base {
			*o.v = ""
		}
	}

	for _, p := range []**Photo{&c.Photo, &c.Banner} {
		if *p == nil {
			continue
		}
		cp := **p
		cp.X, cp.Y = clampPct(cp.X), clampPct(cp.Y)
		*p = &cp
	}

	if c.Fabric == DefaultFabric && c.Main == "" && c.Highlight == "" &&
		c.Second == "" && c.Photo == nil && c.Banner == nil {
		return nil, nil
	}

	return &c, nil
}

func clampPct(v int) int {
	return max(0, min(100, v))
}

// Token is one CSS custom property (name without the leading "--").
type Token struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Adjustment reports an admin-picked colour the readability guard changed.
type Adjustment struct {
	Knob   string `json:"knob"`
	Picked string `json:"picked"`
	Used   string `json:"used"`
}

// Palette is a resolved token set.
type Palette struct {
	Tokens      []Token      `json:"tokens"`
	Adjustments []Adjustment `json:"adjustments"`
	// Main is the resolved main colour (nav, mobile browser chrome, the
	// circle switcher dot).
	Main string `json:"main"`
}

// Declarations renders the palette as CSS custom-property declarations.
// Every value is engine-generated hex or an "r, g, b" triple, never user
// text, so the output is safe to embed in a <style> block.
func (p Palette) Declarations() string {
	var b strings.Builder
	for _, t := range p.Tokens {
		b.WriteString("--")
		b.WriteString(t.Name)
		b.WriteByte(':')
		b.WriteString(t.Value)
		b.WriteByte(';')
	}

	return b.String()
}

// seed identifies which of the six seeds a derived token follows.
type seed int

const (
	seedMain seed = iota
	seedHighlight
	seedSecond
	seedAccent
	seedPaper
	seedInk
)

// rani holds the original seeds; derived tokens are defined relative to them.
var rani = Fabrics[0]

func (f Fabric) seedHex(s seed) string {
	return [...]string{f.Main, f.Highlight, f.Second, f.Accent, f.Paper, f.Ink}[s]
}

// derived lists every non-seed colour token with its seed and its Rani
// value (which must match pallu.css :root — enforced by a test).
var derived = []struct {
	name string
	from seed
	rani string
}{
	{"rani-deep", seedMain, "#5c0a2a"},
	{"rani-line", seedMain, "#b06a82"},
	{"nav-muted", seedMain, "#e9c9d4"},

	{"zari", seedHighlight, "#c8962c"},
	{"zari-deep", seedHighlight, "#a8791f"},
	{"zari-ink", seedHighlight, "#8a6a1d"},
	{"emblem-arm", seedHighlight, "#e9bd4c"},
	{"emblem-leaf", seedHighlight, "#e2b143"},
	{"emblem-pale", seedHighlight, "#f2d488"},
	{"emblem-bud", seedHighlight, "#f0cd6e"},
	{"emblem-line", seedHighlight, "#eabf52"},

	{"ledger-fg", seedSecond, "#c3ded9"},
	{"ledger-wait", seedSecond, "#8fc4bd"},
	{"on-peacock", seedSecond, "#eef6f4"},
	{"peacock-wash", seedSecond, "#e7f2f0"},
	{"peacock-line", seedSecond, "#bcd8d3"},
	{"peacock-ink", seedSecond, "#0d4f4f"},

	{"sindoor-wash", seedAccent, "#f9e8e6"},
	{"sindoor-ink", seedAccent, "#7a231c"},
	{"sindoor-deep", seedAccent, "#8f1f16"},

	{"ivory-2", seedPaper, "#fffaf0"},
	{"strip", seedPaper, "#f7edd8"},
	{"line", seedPaper, "#ddc9a6"},
	{"line-2", seedPaper, "#e2cfa8"},
	{"page", seedPaper, "#efe4cb"},
	{"track", seedPaper, "#e0cfae"},
	{"stone", seedPaper, "#b3a587"},
	{"placeholder", seedPaper, "#a98f7a"},
	{"muted", seedPaper, "#a99374"},
	{"ink-soft", seedPaper, "#8a6d55"},
	{"on-rani", seedPaper, "#f6ecd8"},
	{"numeral-off", seedPaper, "#d9bf94"},

	{"read-ink", seedInk, "#2e1820"},
}

// follow re-expresses Rani's (seed → token) relationship on a new seed:
// lightness and hue shift by the same amount, chroma scales by the same
// ratio. With the Rani seed it returns the Rani token itself.
func follow(newSeed, raniSeed, raniTok lch) lch {
	c := raniTok.c
	if raniSeed.c > 1e-4 {
		c = newSeed.c * raniTok.c / raniSeed.c
	}

	return lch{
		l: newSeed.l + (raniTok.l - raniSeed.l),
		c: c,
		h: newSeed.h + (raniTok.h - raniSeed.h),
	}
}

// Readability floors, each set just under the Rani palette's own ratio
// (or at WCAG AA where Rani clears it) so the house look passes untouched.
const (
	minText      = 4.5 // WCAG AA body text
	minLedgerFg  = 4.4
	minSmallMeta = 4.3
	minQuietTag  = 3.0
	// minWashLight keeps alert washes light: at 7:1 against black there is
	// always an ink dark enough to reach AA on them. Rani's are ~18:1.
	minWashLight = 7.0
)

// Resolve computes the full palette for a config (nil = house look).
func Resolve(c *Config) (Palette, error) {
	if c == nil {
		c = &Config{Fabric: DefaultFabric}
	}

	f, ok := FabricByID(c.Fabric)
	if !ok {
		return Palette{}, fmt.Errorf("unknown fabric %q", c.Fabric)
	}

	seeds := f
	var picked [3]string
	for i, o := range []struct {
		v   string
		dst *string
	}{
		{c.Main, &seeds.Main}, {c.Highlight, &seeds.Highlight}, {c.Second, &seeds.Second},
	} {
		if o.v == "" {
			continue
		}
		if !ValidHex(o.v) {
			return Palette{}, fmt.Errorf("invalid colour %q", o.v)
		}
		*o.dst = mustHex(o.v).hex()
		picked[i] = *o.dst
	}

	paper := mustHex(seeds.Paper)
	ink := mustHex(seeds.Ink)
	onRani := follow(paper.oklch(), mustHex(rani.Paper).oklch(), mustHex("#f6ecd8").oklch()).rgb()

	// Knobs first: main and second must be dark enough for headings on
	// paper and for light text on them; the highlight must be light
	// enough to read on main, to carry ink, and as small labels on the
	// second colour's panels.
	main := mustHex(seeds.Main)
	main = ensureContrast(main, paper, minText, true)
	main = ensureContrast(main, onRani, minText, true)

	second := mustHex(seeds.Second)
	second = ensureContrast(second, paper, minText, true)
	second = ensureContrast(second, rgb{1, 1, 1}, minText, true)

	// A very dark pick can satisfy one ground by being darker than it and
	// then be lightened past it for the next; once it is lighter than all
	// three dark grounds every further lightening only raises contrast, so
	// a couple of passes reach a point where all guards hold together.
	highlight := mustHex(seeds.Highlight)
	for range 4 {
		highlight = ensureContrast(highlight, main, minText, false)
		highlight = ensureContrast(highlight, ink, minText, false)
		highlight = ensureContrast(highlight, second, minQuietTag, false)
		if contrast(highlight, main) >= minText && contrast(highlight, ink) >= minText {
			break
		}
	}

	// The accent is fabric-only (not a knob) but carries white button text
	// and error text on paper; guard it like the knobs, silently.
	accent := mustHex(seeds.Accent)
	accent = ensureContrast(accent, rgb{1, 1, 1}, minText, true)
	accent = ensureContrast(accent, paper, minText, true)
	seeds.Accent = accent.hex()

	var adjustments []Adjustment
	for i, got := range []rgb{main, highlight, second} {
		if picked[i] != "" && got.hex() != picked[i] {
			adjustments = append(adjustments, Adjustment{
				Knob:   [...]string{KnobMain, KnobHighlight, KnobSecond}[i],
				Picked: picked[i],
				Used:   got.hex(),
			})
		}
	}

	seeds.Main, seeds.Highlight, seeds.Second = main.hex(), highlight.hex(), second.hex()

	vals := map[string]rgb{
		"rani":     main,
		"marigold": highlight,
		"peacock":  second,
		"sindoor":  accent,
		"ivory":    paper,
		"ink":      ink,
	}
	for _, d := range derived {
		vals[d.name] = follow(
			mustHex(seeds.seedHex(d.from)).oklch(),
			mustHex(rani.seedHex(d.from)).oklch(),
			mustHex(d.rani).oklch(),
		).rgb()
	}

	// Derived text tokens: silently corrected against their grounds.
	fix := func(name string, bg rgb, min float64, darker bool) {
		vals[name] = ensureContrast(vals[name], bg, min, darker)
	}
	fix("nav-muted", main, minText, false)
	fix("on-peacock", second, minText, false)
	fix("ledger-fg", second, minLedgerFg, false)
	fix("ledger-wait", second, minQuietTag, false)
	// Alert washes are light surfaces; a very dark seed would otherwise
	// drag them to mid-grey where no ink can reach AA. Keep them light,
	// then fit the ink.
	fix("peacock-wash", rgb{}, minWashLight, false)
	fix("sindoor-wash", rgb{}, minWashLight, false)
	fix("peacock-ink", vals["peacock-wash"], minText, true)
	fix("sindoor-ink", vals["sindoor-wash"], minText, true)
	fix("zari-ink", vals["strip"], minSmallMeta, true)
	fix("ink-soft", paper, minSmallMeta, true)

	order := []string{"rani", "marigold", "peacock", "sindoor", "ivory", "ink"}
	for _, d := range derived {
		order = append(order, d.name)
	}

	tokens := make([]Token, 0, len(order)+4)
	for _, n := range order {
		tokens = append(tokens, Token{Name: n, Value: vals[n].hex()})
	}
	tokens = append(tokens,
		Token{Name: "rani-rgb", Value: main.triple()},
		Token{Name: "marigold-rgb", Value: highlight.triple()},
		Token{Name: "ink-rgb", Value: ink.triple()},
		Token{Name: "pl-shadow", Value: "14px 14px 0 rgba(" + main.triple() + ", 0.18)"},
	)

	return Palette{Tokens: tokens, Adjustments: adjustments, Main: main.hex()}, nil
}
