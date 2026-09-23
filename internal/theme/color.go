package theme

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// rgb is a gamma-encoded sRGB colour with channels in [0, 1].
type rgb struct{ r, g, b float64 }

// lch is an OKLCH colour: perceptual lightness (0..1), chroma, hue in degrees.
type lch struct{ l, c, h float64 }

// ValidHex reports whether s is a #RGB or #RRGGBB colour.
func ValidHex(s string) bool {
	_, ok := parseHex(s)
	return ok
}

func parseHex(s string) (rgb, bool) {
	if !strings.HasPrefix(s, "#") {
		return rgb{}, false
	}
	h := s[1:]
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return rgb{}, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return rgb{}, false
	}

	return rgb{
		r: float64(v>>16&0xff) / 255,
		g: float64(v>>8&0xff) / 255,
		b: float64(v&0xff) / 255,
	}, true
}

func mustHex(s string) rgb {
	c, ok := parseHex(s)
	if !ok {
		panic("theme: bad hex " + s)
	}

	return c
}

func channel8(v float64) int {
	return int(math.Round(math.Max(0, math.Min(1, v)) * 255))
}

func (c rgb) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", channel8(c.r), channel8(c.g), channel8(c.b))
}

// triple renders "r, g, b" for use as rgba(var(--x-rgb), α).
func (c rgb) triple() string {
	return fmt.Sprintf("%d, %d, %d", channel8(c.r), channel8(c.g), channel8(c.b))
}

func toLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}

	return math.Pow((v+0.055)/1.055, 2.4)
}

func toGamma(v float64) float64 {
	if v <= 0.0031308 {
		return v * 12.92
	}

	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

// luminance is the WCAG 2 relative luminance.
func (c rgb) luminance() float64 {
	return 0.2126*toLinear(c.r) + 0.7152*toLinear(c.g) + 0.0722*toLinear(c.b)
}

// contrast is the WCAG 2 contrast ratio between two colours (1..21).
func contrast(a, b rgb) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}

	return (la + 0.05) / (lb + 0.05)
}

func (c rgb) oklch() lch {
	r, g, b := toLinear(c.r), toLinear(c.g), toLinear(c.b)

	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)

	L := 0.2104542553*l + 0.7936177850*m - 0.0040720468*s
	A := 1.9779984951*l - 2.4285922050*m + 0.4505937099*s
	B := 0.0259040371*l + 0.7827717662*m - 0.8086757660*s

	h := math.Atan2(B, A) * 180 / math.Pi
	if h < 0 {
		h += 360
	}

	return lch{l: L, c: math.Hypot(A, B), h: h}
}

// unclamped converts to sRGB without gamut mapping; channels may leave [0, 1].
func (p lch) unclamped() rgb {
	rad := p.h * math.Pi / 180
	A, B := p.c*math.Cos(rad), p.c*math.Sin(rad)

	l := p.l + 0.3963377774*A + 0.2158037573*B
	m := p.l - 0.1055613458*A - 0.0638541728*B
	s := p.l - 0.0894841775*A - 1.2914855480*B
	l, m, s = l*l*l, m*m*m, s*s*s

	return rgb{
		r: toGamma(+4.0767416621*l - 3.3077115913*m + 0.2309699292*s),
		g: toGamma(-1.2684380046*l + 2.6097574011*m - 0.3413193965*s),
		b: toGamma(-0.0041960863*l - 0.7034186147*m + 1.7076147010*s),
	}
}

func inGamut(c rgb) bool {
	const eps = 0.5 / 255
	return c.r >= -eps && c.r <= 1+eps &&
		c.g >= -eps && c.g <= 1+eps &&
		c.b >= -eps && c.b <= 1+eps
}

// rgb maps the colour into sRGB by reducing chroma (hue and lightness kept),
// the standard perceptual gamut-mapping move.
func (p lch) rgb() rgb {
	p.l = math.Max(0, math.Min(1, p.l))
	if c := p.unclamped(); inGamut(c) {
		return c
	}

	lo, hi := 0.0, p.c
	for range 24 {
		mid := (lo + hi) / 2
		if inGamut(lch{p.l, mid, p.h}.unclamped()) {
			lo = mid
		} else {
			hi = mid
		}
	}

	return lch{p.l, lo, p.h}.unclamped()
}

// ensureContrast moves c's lightness (darker or lighter) until it reaches
// min contrast against bg, keeping hue and as much chroma as fits. It
// returns c unchanged when it already passes. Contrast is judged on the
// 8-bit colour that will actually be emitted, so hex rounding can never
// tip a passing pair below the floor.
func ensureContrast(c, bg rgb, min float64, darker bool) rgb {
	c = quantize(c)
	if contrast(c, bg) >= min {
		return c
	}

	p := c.oklch()
	for range 200 {
		if darker {
			p.l -= 0.005
		} else {
			p.l += 0.005
		}
		if p.l <= 0 || p.l >= 1 {
			break
		}
		if out := quantize(p.rgb()); contrast(out, bg) >= min {
			return out
		}
	}

	if darker {
		return rgb{}
	}

	return rgb{1, 1, 1}
}

// quantize rounds c to the 8-bit sRGB colour its hex form encodes.
func quantize(c rgb) rgb {
	return mustHex(c.hex())
}
