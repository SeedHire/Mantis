package tui

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
)

// ── Copper enterprise palette ─────────────────────────────────────────────────
//
//   Primary   — fresh copper     #C87941
//   Light     — polished copper  #E8A96A
//   Dark      — deep copper      #7A3E10
//   Gold      — warm gold accent #F0C040
//   BG        — obsidian black   #0C0E14
//   Surface   — dark surface     #12151E
//   Surface2  — lifted surface   #1A1E2A
//   Border    — subtle border    #272B38
//   BorderHi  — bright border    #3A3F52
//   Fg        — off-white text   #E8EAF2
//   FgMuted   — muted text       #6E7491
//   Success   — mint green       #2DD4A0
//   Warning   — amber            #F0A030
//   Error     — crimson          #F05060

const (
	colorCopper      = lipgloss.Color("#C87941")
	colorCopperLight = lipgloss.Color("#E8A96A")
	colorCopperDark  = lipgloss.Color("#7A3E10")
	colorGold        = lipgloss.Color("#FFAF00")
	colorBG          = lipgloss.Color("#0C0E14")
	colorSurface     = lipgloss.Color("#12151E")
	colorSurface2    = lipgloss.Color("#1A1E2A")
	colorBorder      = lipgloss.Color("#272B38")
	colorBorderHi    = lipgloss.Color("#3A3F52")
	colorFg          = lipgloss.Color("#E8EAF2")
	colorFgMuted     = lipgloss.Color("#6E7491")
	colorSuccess     = lipgloss.Color("#2DD4A0")
	colorWarning     = lipgloss.Color("#F0A030")
	colorError       = lipgloss.Color("#F05060")

	// Aliases used across screens
	colorPrimary = colorCopper
	colorAccent  = colorCopperLight
	colorMuted   = colorFgMuted
)

// ── Shared styles ─────────────────────────────────────────────────────────────

var (
	StyleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorCopper)

	StyleSubtitle = lipgloss.NewStyle().
			Foreground(colorFgMuted).
			Italic(true)

	StyleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	StyleTabBar = lipgloss.NewStyle().
			Background(colorSurface).
			PaddingTop(0).
			PaddingBottom(0)

	StyleTabActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorBG).
			Background(colorCopper).
			Padding(0, 3)

	StyleTabInactive = lipgloss.NewStyle().
				Foreground(colorFgMuted).
				Background(colorSurface).
				Padding(0, 3)

	StyleTabSep = lipgloss.NewStyle().
			Foreground(colorBorder).
			Background(colorSurface)

	StyleLabel = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorCopperLight)

	StyleValue = lipgloss.NewStyle().
			Foreground(colorFg)

	StyleSuccess = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	StyleWarning = lipgloss.NewStyle().
			Foreground(colorWarning).
			Bold(true)

	StyleError = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	StyleMuted = lipgloss.NewStyle().
			Foreground(colorFgMuted)

	StyleHelp = lipgloss.NewStyle().
			Foreground(colorFgMuted).
			Background(colorSurface).
			Padding(0, 1)

	StyleHighlight = lipgloss.NewStyle().
			Foreground(colorCopperLight).
			Bold(true)

	StyleSelected = lipgloss.NewStyle().
			Foreground(colorFg).
			Background(colorSurface2).
			Bold(true)

	StyleStatCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Background(colorSurface).
			Padding(1, 3).
			Width(24)

	StyleStatNumber = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorCopperLight)

	StyleStatLabel = lipgloss.NewStyle().
			Foreground(colorFgMuted)

	StyleLogoBox = lipgloss.NewStyle().
			Foreground(colorCopper).
			Bold(true)

	StyleLogoTagline = lipgloss.NewStyle().
				Foreground(colorFgMuted).
				Italic(true)

	StyleDivider = lipgloss.NewStyle().
			Foreground(colorBorder)

	StyleInputBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorderHi).
			Padding(0, 1)

	StyleSectionHeader = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorCopper).
				BorderBottom(true).
				BorderForeground(colorBorder).
				MarginBottom(1)
)

// ── Key bindings ──────────────────────────────────────────────────────────────

type GlobalKeys struct {
	Quit     key.Binding
	Tab      key.Binding
	ShiftTab key.Binding
	Tab1     key.Binding
	Tab2     key.Binding
	Tab3     key.Binding
	Tab4     key.Binding
	Tab5     key.Binding
	Tab6     key.Binding
	Tab7     key.Binding
	Tab8     key.Binding
	Tab9     key.Binding
	Tab0     key.Binding
}

var Keys = GlobalKeys{
	Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Tab:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next")),
	ShiftTab: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "prev")),
	Tab1:     key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "dashboard")),
	Tab2:     key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "search")),
	Tab3:     key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "impact")),
	Tab4:     key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "lint")),
	Tab5:     key.NewBinding(key.WithKeys("5"), key.WithHelp("5", "dead code")),
	Tab6:     key.NewBinding(key.WithKeys("6"), key.WithHelp("6", "hotspots")),
	Tab7:     key.NewBinding(key.WithKeys("7"), key.WithHelp("7", "traces")),
	Tab8:     key.NewBinding(key.WithKeys("8"), key.WithHelp("8", "workspace")),
	Tab9:     key.NewBinding(key.WithKeys("9"), key.WithHelp("9", "brain")),
	Tab0:     key.NewBinding(key.WithKeys("0"), key.WithHelp("0", "router")),
}

// HelpText renders the bottom help bar.
func HelpText(extra ...string) string {
	hints := append(extra, "tab / shift+tab  navigate", "1-9,0  jump", "q  quit")
	out := ""
	for i, h := range hints {
		if i > 0 {
			out += StyleTabSep.Render("   │   ")
		}
		out += StyleMuted.Render(h)
	}
	return out
}
