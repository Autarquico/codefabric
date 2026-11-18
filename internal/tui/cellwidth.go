package tui

import "github.com/charmbracelet/x/ansi"

// wideSymbols are the glyphs that are one cell wide by the Unicode width table
// (ansi.StringWidth counts them as one) and that have an emoji form, which many
// terminals draw two cells wide: the bullet of Claude (⏺), its spinner (✳),
// ticks, warnings, play buttons (✻, the other spinner, has no emoji form). A row that holds them can be wider than it was
// measured and would wrap, so the grid counts each as two.
var wideSymbols = [][2]rune{
	{0x203C, 0x203C}, {0x2049, 0x2049}, {0x2122, 0x2122}, {0x2139, 0x2139},
	{0x2194, 0x2199}, {0x21A9, 0x21AA}, {0x2328, 0x2328}, {0x23CF, 0x23CF},
	{0x23E9, 0x23F3}, {0x23F8, 0x23FA}, {0x24C2, 0x24C2}, {0x25AA, 0x25AB},
	{0x25B6, 0x25B6}, {0x25C0, 0x25C0}, {0x25FB, 0x25FE}, {0x2600, 0x26FF},
	{0x2702, 0x2702}, {0x2705, 0x2705}, {0x2708, 0x270D}, {0x270F, 0x270F},
	{0x2712, 0x2712}, {0x2714, 0x2714}, {0x2716, 0x2716}, {0x271D, 0x271D},
	{0x2721, 0x2721}, {0x2728, 0x2728}, {0x2733, 0x2734}, {0x2744, 0x2744},
	{0x2747, 0x2747}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2763, 0x2764}, {0x2795, 0x2797}, {0x27A1, 0x27A1},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2934, 0x2935}, {0x2B05, 0x2B07}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50},
	{0x2B55, 0x2B55},
}

// cellWidth is the width of s in a terminal that draws the emoji forms wide: at
// least what ansi.StringWidth says, never less than what is drawn.
func cellWidth(s string) int {
	width := ansi.StringWidth(s)
	for _, r := range ansi.Strip(s) {
		if isWideSymbol(r) {
			width++
		}
	}
	return width
}

func isWideSymbol(r rune) bool {
	for _, span := range wideSymbols {
		if r >= span[0] && r <= span[1] {
			return ansi.StringWidth(string(r)) == 1
		}
	}
	return false
}

// truncateCells cuts s to at most width cells by cellWidth.
func truncateCells(s string, width int) string {
	out := ansi.Truncate(s, width, "")
	for cut := width - 1; cellWidth(out) > width && cut >= 0; cut-- {
		out = ansi.Truncate(s, cut, "")
	}
	return out
}
