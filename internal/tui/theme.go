package tui

import "charm.land/lipgloss/v2"

// Theme is the visual signature of the design system in a terminal: cold
// neutrals, one accent, hairline separators and the semantic hues. Colours are
// picked per background, so the same theme reads on light and dark terminals;
// every text style meets the contrast floor of the design system (4.5:1) on
// both. Colour degradation (NO_COLOR, 16 colours) is the program's: bubbletea
// downsamples what the View emits to the profile of the terminal.
type Theme struct {
	Title   lipgloss.Style // CODEFABRIC, panel numbers
	Text    lipgloss.Style
	Faint   lipgloss.Style // labels, timestamps, unavailable keys
	Rule    lipgloss.Style // hairlines: decoration, not text (3:1)
	Accent  lipgloss.Style // key letters in the footer
	Success lipgloss.Style
	Warning lipgloss.Style
	Danger  lipgloss.Style
}

// NewTheme builds the theme for a dark or a light background.
func NewTheme(dark bool) Theme {
	pick := lipgloss.LightDark(dark)
	fg := func(light, dark string) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(pick(lipgloss.Color(light), lipgloss.Color(dark)))
	}
	return Theme{
		Title:   fg("#0f172a", "#f1f5f9").Bold(true),
		Text:    fg("#334155", "#cbd5e1"),
		Faint:   fg("#556377", "#94a3b8"),
		Rule:    fg("#6f7f96", "#64748b"),
		Accent:  fg("#0369a1", "#38bdf8").Bold(true),
		Success: fg("#047857", "#34d399"),
		Warning: fg("#b45309", "#fbbf24"),
		Danger:  fg("#be123c", "#fb7185"),
	}
}
