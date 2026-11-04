package tui

import (
	"image/color"
	"math"
	"testing"
)

// luminance is the relative luminance of WCAG 2.x.
func luminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		x := float64(v) / 0xffff
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b color.Color) float64 {
	hi, lo := luminance(a), luminance(b)
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

func hex(v uint32) color.Color {
	return color.RGBA{R: uint8(v >> 16 & 0xff), G: uint8(v >> 8 & 0xff), B: uint8(v & 0xff), A: 0xff}
}

// The floors of ~/.claude/design-system.yml: 4.5:1 for text; hairlines are
// decoration, held to the 3:1 of WCAG for graphics.
func TestTheme_MeetsTheContrastFloorOnLightAndDarkTerminals(t *testing.T) {
	backgrounds := map[string][]uint32{
		"light": {0xffffff, 0xf1f5f9}, // white, slate-100
		"dark":  {0x000000, 0x0f172a, 0x1e293b},
	}
	for name, bgs := range backgrounds {
		th := NewTheme(name == "dark")
		styles := map[string]struct {
			fg    color.Color
			floor float64
		}{
			"Title": {th.Title.GetForeground(), 4.5}, "Text": {th.Text.GetForeground(), 4.5},
			"Faint": {th.Faint.GetForeground(), 4.5}, "Accent": {th.Accent.GetForeground(), 4.5},
			"Success": {th.Success.GetForeground(), 4.5}, "Warning": {th.Warning.GetForeground(), 4.5},
			"Danger": {th.Danger.GetForeground(), 4.5}, "Rule": {th.Rule.GetForeground(), 3},
		}
		for style, s := range styles {
			for _, bg := range bgs {
				if got := contrast(s.fg, hex(bg)); got < s.floor {
					t.Errorf("%s %s on #%06x: %.2f:1, floor %.1f:1", name, style, bg, got, s.floor)
				}
			}
		}
	}
}
