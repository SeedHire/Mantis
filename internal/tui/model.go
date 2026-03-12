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
	tabHotspots
	tabTraces
	tabWorkspace
	tabBrain
	tabRouter
	tabCount
)

var tabNames = []string{
	"Dashboard", "Search", "Impact", "Lint", "Dead",
	"Hotspots", "Traces", "Workspace", "Brain", "Router",
}

var tabIcons = []string{
	"◈", "🔍", "⚡", "⬡", "◌",
	"🔥", "📊", "🌐", "🧠", "🎯",
}

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
	hotspots  HotspotsModel
	traces    TracesModel
	workspace WorkspaceModel
	brain     BrainModel
	router    RouterModel
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
		hotspots:  NewHotspotsModel(root),
		traces:    NewTracesModel(db),
		workspace: NewWorkspaceModel(root),
		brain:     NewBrainModel(root),
		router:    NewRouterModel(),
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
		m.hotspots.Init(),
		m.traces.Init(),
		m.workspace.Init(),
		m.brain.Init(),
		m.router.Init(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentH := m.height - 4
		m.search.SetSize(m.width, contentH)
		m.impact.SetSize(m.width, contentH)
		m.lint.SetSize(m.width, contentH)
		m.dead.SetSize(m.width, contentH)
		m.hotspots.SetSize(m.width, contentH)
		m.traces.SetSize(m.width, contentH)
		m.workspace.SetSize(m.width, contentH)
		m.brain.SetSize(m.width, contentH)
		m.router.SetSize(m.width, contentH)
		m.dashboard.width = m.width

	case tea.KeyMsg:
		// Check if the active tab has a focused text input — if so, skip
		// global keybindings (number keys, q, r) to allow typing.
		inputFocused := (m.activeTab == tabSearch && m.search.input.Focused()) ||
			(m.activeTab == tabImpact && m.impact.input.Focused())

		if !inputFocused {
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
			case key.Matches(msg, Keys.Tab6):
				m.activeTab = tabHotspots
			case key.Matches(msg, Keys.Tab7):
				m.activeTab = tabTraces
			case key.Matches(msg, Keys.Tab8):
				m.activeTab = tabWorkspace
			case key.Matches(msg, Keys.Tab9):
				m.activeTab = tabBrain
			case key.Matches(msg, Keys.Tab0):
				m.activeTab = tabRouter
			}
		} else if key.Matches(msg, Keys.Quit) && msg.String() == "ctrl+c" {
			// Always allow Ctrl+C to quit even when typing
			return m, tea.Quit
		}
	}

	// Key messages go only to the active tab; other messages (WindowSize,
	// custom background-load results) go to all tabs.
	var c tea.Cmd
	if _, isKey := msg.(tea.KeyMsg); isKey {
		switch m.activeTab {
		case tabDashboard:
			m.dashboard, c = m.dashboard.Update(msg)
		case tabSearch:
			m.search, c = m.search.Update(msg)
		case tabImpact:
			m.impact, c = m.impact.Update(msg)
		case tabLint:
			m.lint, c = m.lint.Update(msg)
		case tabDead:
			m.dead, c = m.dead.Update(msg)
		case tabHotspots:
			m.hotspots, c = m.hotspots.Update(msg)
		case tabTraces:
			m.traces, c = m.traces.Update(msg)
		case tabWorkspace:
			m.workspace, c = m.workspace.Update(msg)
		case tabBrain:
			m.brain, c = m.brain.Update(msg)
		case tabRouter:
			m.router, c = m.router.Update(msg)
		}
		cmds = append(cmds, c)
	} else {
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
		m.hotspots, c = m.hotspots.Update(msg)
		cmds = append(cmds, c)
		m.traces, c = m.traces.Update(msg)
		cmds = append(cmds, c)
		m.workspace, c = m.workspace.Update(msg)
		cmds = append(cmds, c)
		m.brain, c = m.brain.Update(msg)
		cmds = append(cmds, c)
		m.router, c = m.router.Update(msg)
		cmds = append(cmds, c)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	tabBar := m.renderTabBar()
	content := m.renderActiveTab()

	help := StyleHelp.Width(m.width).Render(HelpText())

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
		// Use 1-9 then 0 for tab 10
		numStr := fmt.Sprintf("%d", i+1)
		if i == 9 {
			numStr = "0"
		}
		num := lipgloss.NewStyle().
			Foreground(colorCopperDark).
			Bold(true).
			Render(numStr)

		// Shorter labels for compact tab bar
		shortName := name
		if m.width < 120 {
			shortName = tabShort(i)
		}

		if i == m.activeTab {
			label := lipgloss.NewStyle().
				Bold(true).
				Foreground(colorBG).
				Background(colorCopper).
				Padding(0, 1).
				Render(num + " " + shortName)
			tabs = append(tabs, label)
		} else {
			label := lipgloss.NewStyle().
				Foreground(colorFgMuted).
				Background(colorSurface).
				Padding(0, 1).
				Render(num + " " + shortName)
			tabs = append(tabs, label)
		}
	}

	brand := lipgloss.NewStyle().
		Foreground(colorCopper).
		Background(colorSurface).
		Bold(true).
		Padding(0, 2).
		Render("◈ MANTIS")

	bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
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

func tabShort(idx int) string {
	shorts := []string{"Home", "Find", "Imp", "Lint", "Dead", "Hot", "Trace", "WS", "Brain", "Route"}
	if idx < len(shorts) {
		return shorts[idx]
	}
	return ""
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
	case tabHotspots:
		return m.hotspots.View()
	case tabTraces:
		return m.traces.View()
	case tabWorkspace:
		return m.workspace.View()
	case tabBrain:
		return m.brain.View()
	case tabRouter:
		return m.router.View()
	}
	return ""
}
