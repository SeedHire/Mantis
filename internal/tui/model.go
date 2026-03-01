package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
)

// ── Tab indices ───────────────────────────────────────────────────────────────

const (
	tabDashboard = iota
	tabSearch
	tabImpact
	tabLint
	tabDead
	tabCount
)

var tabNames = []string{"Dashboard", "Search", "Impact", "Lint", "Dead Code"}

// ── Root model ────────────────────────────────────────────────────────────────

// Model is the top-level Bubble Tea model.
type Model struct {
	db        *graph.DB
	activeTab int
	width     int
	height    int

	dashboard DashboardModel
	search    SearchModel
	impact    ImpactModel
	lint      LintModel
	dead      DeadModel
}

// New creates the root TUI model.
func New(db *graph.DB, root string) Model {
	m := Model{
		db:        db,
		activeTab: tabDashboard,
		dashboard: NewDashboardModel(db),
		search:    NewSearchModel(db),
		impact:    NewImpactModel(db),
		lint:      NewLintModel(db, root),
		dead:      NewDeadModel(db),
	}
	m.dead.SetDB(db)
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.dashboard.Init(),
		m.search.Init(),
		m.impact.Init(),
		m.lint.Init(),
		m.dead.Init(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentH := m.height - 4 // subtract tab bar + help line
		m.search.SetSize(m.width, contentH)
		m.impact.SetSize(m.width, contentH)
		m.lint.SetSize(m.width, contentH)
		m.dead.SetSize(m.width, contentH)
		m.dashboard.width = m.width

	case tea.KeyMsg:
		// Global navigation — don't intercept if a text input has focus
		switch {
		case key.Matches(msg, Keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, Keys.Tab):
			m.activeTab = (m.activeTab + 1) % tabCount
		case key.Matches(msg, Keys.ShiftTab):
			m.activeTab = (m.activeTab - 1 + tabCount) % tabCount
		case key.Matches(msg, Keys.Tab1):
			m.activeTab = tabDashboard
		case key.Matches(msg, Keys.Tab2):
			m.activeTab = tabSearch
		case key.Matches(msg, Keys.Tab3):
			m.activeTab = tabImpact
		case key.Matches(msg, Keys.Tab4):
			m.activeTab = tabLint
		case key.Matches(msg, Keys.Tab5):
			m.activeTab = tabDead
		}
	}

	// Route messages to the active screen (and all screens for window resize)
	switch msg.(type) {
	case tea.WindowSizeMsg:
		var c tea.Cmd
		m.dashboard, c = m.dashboard.Update(msg)
		cmds = append(cmds, c)
		m.search, c = m.search.Update(msg)
		cmds = append(cmds, c)
		m.impact, c = m.impact.Update(msg)
		cmds = append(cmds, c)
		m.lint, c = m.lint.Update(msg)
		cmds = append(cmds, c)
		m.dead, c = m.dead.Update(msg)
		cmds = append(cmds, c)

	default:
		// Non-window messages go to ALL screens (so background loads work)
		var c tea.Cmd
		m.dashboard, c = m.dashboard.Update(msg)
		cmds = append(cmds, c)
		m.search, c = m.search.Update(msg)
		cmds = append(cmds, c)
		m.impact, c = m.impact.Update(msg)
		cmds = append(cmds, c)
		m.lint, c = m.lint.Update(msg)
		cmds = append(cmds, c)
		m.dead, c = m.dead.Update(msg)
		cmds = append(cmds, c)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	tabBar := m.renderTabBar()
	content := m.renderActiveTab()

	// Bottom help bar stretched to terminal width
	help := StyleHelp.Width(m.width).Render(HelpText())

	// Pad content to fill terminal height
	contentLines := strings.Count(content, "\n") + 1
	tabBarLines := 2
	helpLines := 1
	remaining := m.height - tabBarLines - contentLines - helpLines
	if remaining > 0 {
		content += strings.Repeat("\n", remaining)
	}

	return tabBar + content + "\n" + help
}

func (m Model) renderTabBar() string {
	var tabs []string
	for i, name := range tabNames {
		num := lipgloss.NewStyle().
			Foreground(colorCopperDark).
			Bold(true).
			Render(fmt.Sprintf("%d", i+1))
		if i == m.activeTab {
			label := lipgloss.NewStyle().
				Bold(true).
				Foreground(colorBG).
				Background(colorCopper).
				Padding(0, 3).
				Render(num + " " + name)
			tabs = append(tabs, label)
		} else {
			label := lipgloss.NewStyle().
				Foreground(colorFgMuted).
				Background(colorSurface).
				Padding(0, 3).
				Render(num + " " + name)
			tabs = append(tabs, label)
		}
	}
	// Right-side branding
	brand := lipgloss.NewStyle().
		Foreground(colorCopper).
		Background(colorSurface).
		Bold(true).
		Padding(0, 2).
		Render("◈ MANTIS")

	bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	// Pad between tabs and brand
	barWidth := lipgloss.Width(bar)
	brandWidth := lipgloss.Width(brand)
	pad := m.width - barWidth - brandWidth
	if pad < 0 {
		pad = 0
	}
	padding := lipgloss.NewStyle().
		Background(colorSurface).
		Render(strings.Repeat(" ", pad))

	full := lipgloss.NewStyle().Background(colorSurface).Render(bar) + padding + brand
	return full + "\n"
}

func (m Model) renderActiveTab() string {
	switch m.activeTab {
	case tabDashboard:
		return m.dashboard.View()
	case tabSearch:
		return m.search.View()
	case tabImpact:
		return m.impact.View()
	case tabLint:
		return m.lint.View()
	case tabDead:
		return m.dead.View()
	}
	return ""
}
