package tui

import "charm.land/lipgloss/v2"

// Palette says how the theme is drawn: on a dark or a light background, and
// with colour or without (NO_COLOR keeps the weight and drops the hue).
type Palette struct {
	Dark  bool
	Color bool
}

// Theme is the visual signature of the design system in a terminal: cold
// neutrals, one accent, hairline separators and the semantic hues. Colours are
// picked per background, so the same theme reads on light and dark terminals.
type Theme struct {
	Title   lipgloss.Style // CODEFABRIC, panel headings
	Text    lipgloss.Style
	Faint   lipgloss.Style // labels, hairlines, disabled keys
	Accent  lipgloss.Style // key letters in the footer
	Success lipgloss.Style
	Warning lipgloss.Style
	Danger  lipgloss.Style
}

// NewTheme builds the theme of p.
func NewTheme(p Palette) Theme {
	pick := lipgloss.LightDark(p.Dark)
	fg := func(light, dark string) lipgloss.Style {
		s := lipgloss.NewStyle()
		if !p.Color {
			return s
		}
		return s.Foreground(pick(lipgloss.Color(light), lipgloss.Color(dark)))
	}
	return Theme{
		Title:   fg("#0f172a", "#f1f5f9").Bold(true),
		Text:    fg("#334155", "#cbd5e1"),
		Faint:   fg("#94a3b8", "#64748b"),
		Accent:  fg("#0284c7", "#38bdf8").Bold(true),
		Success: fg("#059669", "#34d399"),
		Warning: fg("#d97706", "#fbbf24"),
		Danger:  fg("#e11d48", "#fb7185"),
	}
}
