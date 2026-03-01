package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/router"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type routerResultMsg struct {
	summary map[string]string
	err     error
}

// ── Model ─────────────────────────────────────────────────────────────────────

type RouterModel struct {
	viewport viewport.Model
	loading  bool
	err      error
	ready    bool
	width    int
	height   int
}

func NewRouterModel() RouterModel {
	vp := viewport.New(80, 20)
	return RouterModel{viewport: vp, loading: true}
}

func (m *RouterModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = h - 6
	m.ready = true
}

func (m RouterModel) Init() tea.Cmd {
	return m.load()
}

func (m RouterModel) load() tea.Cmd {
	return func() tea.Msg {
		summary := router.ResolvedSummary()
		return routerResultMsg{summary: summary}
	}
}

func (m RouterModel) Update(msg tea.Msg) (RouterModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case routerResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.viewport.SetContent(renderRouter(msg.summary, m.width))
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			cmds = append(cmds, m.load())
			return m, tea.Batch(cmds...)
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m RouterModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  🎯  Model Router"),
		sep,
		StyleMuted.Render("  7-tier intent classification · dynamic model selection  ·  r refresh"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Loading router configuration…")
	case m.err != nil:
		body = StyleError.Render("  ✗  "+m.err.Error())
	default:
		body = m.viewport.View()
	}

	return header + body
}

func renderRouter(summary map[string]string, width int) string {
	var sb strings.Builder
	sepLen := width - 4
	if sepLen < 40 {
		sepLen = 40
	}

	// Tier explanation table
	tierInfo := []struct {
		tier    string
		icon    string
		desc    string
		example string
		color   lipgloss.Color
	}{
		{"Trivial", "💬", "Greetings, simple acknowledgments", "\"hi\", \"thanks\", \"yes\"", colorSuccess},
		{"Fast", "⚡", "Quick explanations, factual questions", "\"explain mutex\", \"what is a goroutine\"", colorCopperLight},
		{"Code", "💻", "Code generation, implementation tasks", "\"implement JWT refresh\", \"write tests\"", colorCopper},
		{"Reason", "🧠", "Analysis, comparison, trade-offs", "\"compare Redis vs Memcached\"", colorGold},
		{"Heavy", "🏗", "Large refactoring, multi-file changes", "\"refactor the auth module\"", colorWarning},
		{"Max", "🔮", "Architecture design, 3-model ensemble", "\"redesign the payment system\"", colorError},
		{"Vision", "👁", "Image analysis, diagram understanding", "(requires image input)", colorFgMuted},
	}

	// Active model mapping
	sb.WriteString(StyleHighlight.Render("  Model Tier Mapping") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n\n")

	assigned := 0
	for _, ti := range tierInfo {
		model, ok := summary[ti.tier]
		tierLabel := lipgloss.NewStyle().
			Foreground(ti.color).
			Bold(true).
			Width(10).
			Render(ti.tier)

		icon := ti.icon + " "

		var modelLabel string
		if ok && model != "" {
			modelLabel = lipgloss.NewStyle().
				Foreground(colorGold).
				Bold(true).
				Render(model)
			assigned++
		} else {
			modelLabel = StyleMuted.Render("(not assigned)")
		}

		desc := StyleMuted.Render(ti.desc)
		sb.WriteString(fmt.Sprintf("  %s%s  →  %s\n", icon, tierLabel, modelLabel))
		sb.WriteString(fmt.Sprintf("            %s\n\n", desc))
	}

	// Summary card
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n\n")

	cards := lipgloss.JoinHorizontal(
		lipgloss.Top,
		statCard("Tiers Active", fmt.Sprintf("%d/7", assigned), "🎯"),
		"  ",
		statCard("Classification", "<1ms", "⚡"),
	)
	sb.WriteString(cards + "\n\n")

	// How it works
	sb.WriteString(StyleLabel.Render("  How Classification Works") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")
	sb.WriteString(StyleMuted.Render("  1. User message analyzed against keyword sets per tier\n"))
	sb.WriteString(StyleMuted.Render("  2. Highest-priority matching tier selected\n"))
	sb.WriteString(StyleMuted.Render("  3. Default: Code tier at 60% confidence (low confidence shown)\n"))
	sb.WriteString(StyleMuted.Render("  4. Reason/Heavy tiers trigger multi-pass reasoning pipeline\n"))
	sb.WriteString(StyleMuted.Render("  5. Max tier runs 3 models in parallel, synthesizes responses\n"))
	sb.WriteString(StyleMuted.Render("  6. Trivial/Fast prefer quantized models (q4/q8) for speed\n"))

	// Example classification
	sb.WriteString("\n")
	sb.WriteString(StyleLabel.Render("  Example Classifications") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	examples := []struct {
		query string
		tier  string
		color lipgloss.Color
	}{
		{"\"what does this function do?\"", "Fast", colorCopperLight},
		{"\"implement a binary search tree\"", "Code", colorCopper},
		{"\"compare microservices vs monolith\"", "Reason", colorGold},
		{"\"redesign the database schema\"", "Max", colorError},
		{"\"thanks!\"", "Trivial", colorSuccess},
	}

	for _, ex := range examples {
		query := StyleMuted.Render(ex.query)
		tier := lipgloss.NewStyle().Foreground(ex.color).Bold(true).Render(ex.tier)
		arrow := lipgloss.NewStyle().Foreground(colorBorderHi).Render(" → ")
		sb.WriteString(fmt.Sprintf("  %s%s%s\n", query, arrow, tier))
	}

	return sb.String()
}
