package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/brain"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type brainResultMsg struct {
	brainContent string
	decisions    string
	rejected     string
	conventions  string
	hasBrain     bool
	err          error
}

// ── Model ─────────────────────────────────────────────────────────────────────

type BrainModel struct {
	root     string
	viewport viewport.Model
	loading  bool
	err      error
	noBrain  bool
	ready    bool
	width    int
	height   int
}

func NewBrainModel(root string) BrainModel {
	vp := viewport.New(80, 20)
	return BrainModel{root: root, viewport: vp, loading: true}
}

func (m *BrainModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = h - 6
	m.ready = true
}

func (m BrainModel) Init() tea.Cmd {
	return m.load()
}

func (m BrainModel) load() tea.Cmd {
	root := m.root
	return func() tea.Msg {
		b := brain.New(root)
		if !b.Exists() {
			return brainResultMsg{hasBrain: false}
		}
		return brainResultMsg{
			hasBrain:     true,
			brainContent: b.ReadBrain(),
			decisions:    b.ReadFile("DECISIONS.log"),
			rejected:     b.ReadFile("REJECTED.md"),
			conventions:  b.ReadFile("CONVENTIONS.md"),
		}
	}
}

func (m BrainModel) Update(msg tea.Msg) (BrainModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case brainResultMsg:
		m.loading = false
		m.err = msg.err
		if !msg.hasBrain {
			m.noBrain = true
		} else {
			m.noBrain = false
			m.viewport.SetContent(renderBrain(msg, m.width))
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.noBrain = false
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

func (m BrainModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  🧠  Project Brain"),
		sep,
		StyleMuted.Render("  BRAIN.md · DECISIONS.log · REJECTED.md · CONVENTIONS.md  ·  r refresh"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Loading project brain…")
	case m.err != nil:
		body = StyleError.Render("  ✗  " + m.err.Error())
	case m.noBrain:
		body = noBrainView()
	default:
		body = m.viewport.View()
		if m.viewport.TotalLineCount() > m.viewport.Height {
			body += "\n" + StyleMuted.Render(fmt.Sprintf(
				"  %d/%d lines", m.viewport.YOffset+m.viewport.Height, m.viewport.TotalLineCount()))
		}
	}

	return header + body
}

func noBrainView() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(StyleWarning.Render("  ◈ No project brain found") + "\n\n")
	sb.WriteString(StyleMuted.Render("  Start a Mantis REPL session to initialize the brain:") + "\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(colorCopperLight).Render("    mantis \"explain this project\"") + "\n\n")
	sb.WriteString(StyleMuted.Render("  The brain accumulates across sessions:") + "\n")
	sb.WriteString(StyleLabel.Render("    BRAIN.md         ") + StyleMuted.Render("— rolling project summary") + "\n")
	sb.WriteString(StyleLabel.Render("    DECISIONS.log    ") + StyleMuted.Render("— timestamped architectural choices") + "\n")
	sb.WriteString(StyleLabel.Render("    REJECTED.md      ") + StyleMuted.Render("— approaches tried and failed") + "\n")
	sb.WriteString(StyleLabel.Render("    CONVENTIONS.md   ") + StyleMuted.Render("— code style & architecture rules") + "\n")
	return sb.String()
}

func renderBrain(data brainResultMsg, width int) string {
	var sb strings.Builder
	sepLen := width - 4
	if sepLen < 40 {
		sepLen = 40
	}

	// Stat cards
	brainLines := strings.Count(data.brainContent, "\n")
	decisionLines := strings.Count(data.decisions, "\n")
	rejectedLines := strings.Count(data.rejected, "\n")
	convLines := strings.Count(data.conventions, "\n")

	cards := lipgloss.JoinHorizontal(
		lipgloss.Top,
		statCard("Brain", fmt.Sprintf("%d lines", brainLines), "🧠"),
		"  ",
		statCard("Decisions", fmt.Sprintf("%d entries", decisionLines), "📋"),
		"  ",
		statCard("Rejected", fmt.Sprintf("%d entries", rejectedLines), "🚫"),
	)
	sb.WriteString(cards + "\n\n")

	// ── BRAIN.md ──
	sb.WriteString(StyleHighlight.Render("  🧠 BRAIN.md") + StyleMuted.Render("  (project memory)") + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")
	if data.brainContent == "" {
		sb.WriteString(StyleMuted.Render("  (empty)\n"))
	} else {
		for _, line := range strings.Split(truncContent(data.brainContent, 20), "\n") {
			sb.WriteString("  " + StyleMuted.Render(line) + "\n")
		}
	}

	// ── CONVENTIONS.md ──
	sb.WriteString("\n")
	sb.WriteString(StyleHighlight.Render("  📐 CONVENTIONS.md") + StyleMuted.Render(fmt.Sprintf("  (%d lines)", convLines)) + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")
	if data.conventions == "" {
		sb.WriteString(StyleMuted.Render("  (empty)\n"))
	} else {
		for _, line := range strings.Split(truncContent(data.conventions, 15), "\n") {
			sb.WriteString("  " + StyleMuted.Render(line) + "\n")
		}
	}

	// ── DECISIONS.log ──
	sb.WriteString("\n")
	sb.WriteString(StyleHighlight.Render("  📋 DECISIONS.log") + StyleMuted.Render("  (recent)") + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")
	if data.decisions == "" {
		sb.WriteString(StyleMuted.Render("  No decisions logged yet. Use /decision in REPL.\n"))
	} else {
		lines := strings.Split(data.decisions, "\n")
		// Show last 10 decisions
		start := len(lines) - 10
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:] {
			if strings.TrimSpace(line) != "" {
				sb.WriteString("  " + StyleMuted.Render(line) + "\n")
			}
		}
	}

	// ── REJECTED.md ──
	sb.WriteString("\n")
	sb.WriteString(StyleError.Render("  🚫 REJECTED.md") + StyleMuted.Render("  (never suggest again)") + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")
	if data.rejected == "" {
		sb.WriteString(StyleMuted.Render("  No rejected approaches. Use /reject in REPL.\n"))
	} else {
		for _, line := range strings.Split(truncContent(data.rejected, 15), "\n") {
			if strings.TrimSpace(line) != "" {
				sb.WriteString("  " + StyleMuted.Render(line) + "\n")
			}
		}
	}

	return sb.String()
}

func truncContent(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n  …(" + fmt.Sprintf("%d", len(lines)-maxLines) + " more lines)"
}
