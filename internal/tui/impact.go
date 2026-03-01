package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type impactResultMsg struct {
	result *intel.ImpactResult
	err    error
}

// ── Model ─────────────────────────────────────────────────────────────────────

type ImpactModel struct {
	db       *graph.DB
	input    textinput.Model
	viewport viewport.Model
	loading  bool
	err      error
	ready    bool
	width    int
	height   int
}

func NewImpactModel(db *graph.DB) ImpactModel {
	ti := textinput.New()
	ti.Placeholder = "file path or symbol name…"
	ti.Focus()
	ti.CharLimit = 300
	ti.Width = 55

	vp := viewport.New(80, 20)

	return ImpactModel{
		db:       db,
		input:    ti,
		viewport: vp,
	}
}

func (m *ImpactModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if w > 8 {
		m.input.Width = w - 8
	}
	m.viewport.Width = w
	m.viewport.Height = h - 8
	m.ready = true
}

func (m ImpactModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m ImpactModel) runImpact() tea.Cmd {
	target := strings.TrimSpace(m.input.Value())
	if target == "" {
		return nil
	}
	db := m.db
	return func() tea.Msg {
		q := graph.NewQuerier(db)
		result, err := intel.Impact(q, target, 5)
		return impactResultMsg{result: result, err: err}
	}
}

func (m ImpactModel) Update(msg tea.Msg) (ImpactModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case impactResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.result != nil {
			m.viewport.SetContent(renderImpactResult(msg.result))
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if m.input.Focused() {
				m.loading = true
				m.err = nil
				m.viewport.SetContent("")
				cmds = append(cmds, m.runImpact())
				return m, tea.Batch(cmds...)
			}
		case "esc":
			if !m.input.Focused() {
				m.input.Focus()
				return m, tea.Batch(cmds...)
			}
		case "down", "j", "up", "k":
			if m.input.Focused() {
				m.input.Blur()
			}
		}
	}

	if m.input.Focused() {
		var tiCmd tea.Cmd
		m.input, tiCmd = m.input.Update(msg)
		cmds = append(cmds, tiCmd)
	} else {
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

func (m ImpactModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	prompt := lipgloss.NewStyle().Foreground(colorCopper).Bold(true).Render("  ❯  ")
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  ⚡  Impact Analysis"),
		sep,
		prompt+m.input.View(),
		sep,
		StyleMuted.Render("  enter ↵  ·  ↓↑ scroll results  ·  esc refocus"),
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("\n  Analysing blast radius…")
	case m.err != nil:
		body = StyleError.Render("\n  ✗  "+m.err.Error()) +
			"\n" + StyleMuted.Render("  Run 'mantis init' first.")
	case m.viewport.TotalLineCount() == 0:
		body = StyleMuted.Render("\n  Enter a file path or symbol name and press enter.")
	default:
		body = m.viewport.View()
		if m.viewport.TotalLineCount() > m.viewport.Height {
			body += "\n" + StyleMuted.Render(fmt.Sprintf(
				"  %d/%d lines", m.viewport.YOffset+m.viewport.Height, m.viewport.TotalLineCount()))
		}
	}

	return header + body
}

// renderImpactResult formats the impact result as coloured text.
func renderImpactResult(r *intel.ImpactResult) string {
	var sb strings.Builder

	sb.WriteString(StyleHighlight.Render("⚡ Impact: "+r.Target) + "\n")
	sb.WriteString(StyleMuted.Render(strings.Repeat("─", 60)) + "\n\n")

	if r.TotalFiles == 0 {
		sb.WriteString(StyleSuccess.Render("  ✓ No dependents found — safe to change.") + "\n")
		return sb.String()
	}

	sb.WriteString(StyleLabel.Render(fmt.Sprintf("Total affected: %d file(s)\n\n", r.TotalFiles)))

	// Sort depths
	depths := make([]int, 0, len(r.ByDepth))
	for d := range r.ByDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)

	for _, d := range depths {
		nodes := r.ByDepth[d]
		label := "Indirect"
		if d == 1 {
			label = "Direct  "
		}
		sb.WriteString(StyleLabel.Render(fmt.Sprintf("  %s (depth %d): %d file(s)\n", label, d, len(nodes))))
		for _, n := range nodes {
			score := r.RiskScores[n.ID]
			risk := StyleSuccess.Render("LOW")
			if score >= 8 {
				risk = StyleError.Render("HIGH")
			} else if score >= 5 {
				risk = StyleWarning.Render("MED")
			}
			sb.WriteString(fmt.Sprintf("    %s  %s\n", risk, StyleMuted.Render(n.FilePath)))
		}
	}

	return sb.String()
}
