package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/intel"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type hotspotsResultMsg struct {
	hotspots []intel.FileChurn
	risky    []intel.FileChurn
	coupling []intel.CoupledFile
	err      error
}

// ── Model ─────────────────────────────────────────────────────────────────────

type HotspotsModel struct {
	root     string
	viewport viewport.Model
	loading  bool
	err      error
	ready    bool
	width    int
	height   int
}

func NewHotspotsModel(root string) HotspotsModel {
	vp := viewport.New(80, 20)
	return HotspotsModel{root: root, viewport: vp, loading: true}
}

func (m *HotspotsModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = h - 6
	m.ready = true
}

func (m HotspotsModel) Init() tea.Cmd {
	return m.load()
}

func (m HotspotsModel) load() tea.Cmd {
	root := m.root
	return func() tea.Msg {
		stats, err := intel.Temporal(root, 90)
		if err != nil {
			return hotspotsResultMsg{err: err}
		}
		return hotspotsResultMsg{
			hotspots: intel.Hotspots(stats, 15),
			risky:    intel.Risky(stats, 15),
			coupling: stats.Coupling,
		}
	}
}

func (m HotspotsModel) Update(msg tea.Msg) (HotspotsModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case hotspotsResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.viewport.SetContent(renderHotspots(msg.hotspots, msg.risky, msg.coupling, m.width))
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.err = nil
			cmds = append(cmds, m.load())
			return m, tea.Batch(cmds...)
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m HotspotsModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  🔥  Temporal Intelligence"),
		sep,
		StyleMuted.Render("  git churn · risk scores · co-change coupling  ·  r refresh  ·  ↓↑ scroll"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Analyzing git history (90 days)…")
	case m.err != nil:
		body = StyleError.Render("  ✗  "+m.err.Error()) +
			"\n" + StyleMuted.Render("  Requires a git repository.")
	default:
		body = m.viewport.View()
		if m.viewport.TotalLineCount() > m.viewport.Height {
			body += "\n" + StyleMuted.Render(fmt.Sprintf(
				"  %d/%d lines", m.viewport.YOffset+m.viewport.Height, m.viewport.TotalLineCount()))
		}
	}

	return header + body
}

func renderHotspots(hotspots, risky []intel.FileChurn, coupling []intel.CoupledFile, width int) string {
	var sb strings.Builder
	sepLen := width - 4
	if sepLen < 40 {
		sepLen = 40
	}

	// ── Hotspots ──
	sb.WriteString(StyleHighlight.Render("  🔥 Churn Hotspots") + StyleMuted.Render("  (most changes in 90 days)") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	if len(hotspots) == 0 {
		sb.WriteString(StyleMuted.Render("  No file changes found.\n"))
	}
	for i, f := range hotspots {
		rank := lipgloss.NewStyle().Foreground(colorGold).Bold(true).Width(4).Render(fmt.Sprintf("#%d", i+1))
		churnBar := renderBar(f.ChurnScore, 20)
		path := StyleMuted.Render(truncPath(f.Path, 40))
		commits := lipgloss.NewStyle().Foreground(colorCopperLight).Render(fmt.Sprintf("%dc", f.Commits))
		authors := lipgloss.NewStyle().Foreground(colorFgMuted).Render(fmt.Sprintf("%da", f.Authors))
		sb.WriteString(fmt.Sprintf("  %s %s  %s  %s %s\n", rank, churnBar, path, commits, authors))
	}

	// ── Risky ──
	sb.WriteString("\n")
	sb.WriteString(StyleError.Render("  ⚠ Risk Zones") + StyleMuted.Render("  (high changes × many authors)") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	if len(risky) == 0 {
		sb.WriteString(StyleMuted.Render("  No risky files detected.\n"))
	}
	for i, f := range risky {
		rank := lipgloss.NewStyle().Foreground(colorError).Bold(true).Width(4).Render(fmt.Sprintf("#%d", i+1))
		risk := riskBadge(f.Commits * f.Authors)
		path := StyleMuted.Render(truncPath(f.Path, 40))
		detail := lipgloss.NewStyle().Foreground(colorFgMuted).Render(
			fmt.Sprintf("%d commits · %d authors · %dd ago", f.Commits, f.Authors, f.DaysSinceChange))
		sb.WriteString(fmt.Sprintf("  %s %s  %s  %s\n", rank, risk, path, detail))
	}

	// ── Coupling ──
	sb.WriteString("\n")
	sb.WriteString(StyleWarning.Render("  🔗 Co-Change Coupling") + StyleMuted.Render("  (files that always change together)") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	shown := coupling
	if len(shown) > 15 {
		shown = shown[:15]
	}
	if len(shown) == 0 {
		sb.WriteString(StyleMuted.Render("  No coupling patterns detected.\n"))
	}
	for _, c := range shown {
		score := lipgloss.NewStyle().Foreground(colorGold).Bold(true).Render(fmt.Sprintf("%.0f%%", c.Coupling*100))
		a := StyleMuted.Render(truncPath(c.FileA, 30))
		b := StyleMuted.Render(truncPath(c.FileB, 30))
		arrow := lipgloss.NewStyle().Foreground(colorCopper).Render(" ⟷ ")
		sb.WriteString(fmt.Sprintf("  %s  %s%s%s  (%dx)\n", score, a, arrow, b, c.CoChanges))
	}

	return sb.String()
}

func renderBar(score float64, maxWidth int) string {
	filled := int(score * float64(maxWidth) / 100)
	if filled > maxWidth {
		filled = maxWidth
	}
	if filled < 1 && score > 0 {
		filled = 1
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", maxWidth-filled)
	return lipgloss.NewStyle().Foreground(colorCopper).Render(bar)
}

func riskBadge(score int) string {
	if score >= 20 {
		return StyleError.Render("CRIT")
	}
	if score >= 10 {
		return StyleWarning.Render("HIGH")
	}
	if score >= 5 {
		return lipgloss.NewStyle().Foreground(colorGold).Render(" MED")
	}
	return StyleSuccess.Render(" LOW")
}

func truncPath(p string, max int) string {
	if len(p) <= max {
		return p
	}
	return "…" + p[len(p)-max+1:]
}
