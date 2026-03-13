package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type workspaceResultMsg struct {
	repos []string
	stats map[string]wsRepoStats
	err   error
}

type wsRepoStats struct {
	files   int
	symbols int
	edges   int
}

// ── Model ─────────────────────────────────────────────────────────────────────

type WorkspaceModel struct {
	root     string
	viewport viewport.Model
	loading  bool
	err      error
	noConfig bool
	ready    bool
	width    int
	height   int
}

func NewWorkspaceModel(root string) WorkspaceModel {
	vp := viewport.New(80, 20)
	return WorkspaceModel{root: root, viewport: vp, loading: true}
}

func (m *WorkspaceModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = h - 6
	m.ready = true
}

func (m WorkspaceModel) Init() tea.Cmd {
	return m.load()
}

func (m WorkspaceModel) load() tea.Cmd {
	root := m.root
	return func() tea.Msg {
		cfg, err := graph.LoadWorkspace(root)
		if err != nil || cfg == nil {
			return workspaceResultMsg{err: fmt.Errorf("no workspace config")}
		}
		repos := make([]string, 0, len(cfg.Repos))
		stats := make(map[string]wsRepoStats)
		for _, r := range cfg.Repos {
			name := r.Alias
			if name == "" {
				name = r.Path
			}
			repos = append(repos, name)
			// Try to open each repo's graph for stats
			db, err := graph.Open(r.Path + "/.mantis/graph.db")
			if err != nil {
				stats[name] = wsRepoStats{}
				continue
			}
			q := graph.NewQuerier(db)
			files, _ := q.GetAllFiles()
			nodes, _ := q.FindAllNodes("")
			edges, _ := q.GetAllEdges()
			stats[name] = wsRepoStats{
				files:   len(files),
				symbols: len(nodes) - len(files),
				edges:   len(edges),
			}
			db.Close()
		}
		return workspaceResultMsg{repos: repos, stats: stats}
	}
}

func (m WorkspaceModel) Update(msg tea.Msg) (WorkspaceModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case workspaceResultMsg:
		m.loading = false
		if msg.err != nil {
			m.noConfig = true
			m.err = msg.err
		} else {
			m.noConfig = false
			m.viewport.SetContent(renderWorkspace(msg.repos, msg.stats, m.width))
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.noConfig = false
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

func (m WorkspaceModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  🌐  Cross-Repo Workspace"),
		sep,
		StyleMuted.Render("  multi-repo graph intelligence  ·  r refresh  ·  ↓↑ scroll"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Loading workspace configuration…")
	case m.noConfig:
		body = noWorkspaceView()
	default:
		body = m.viewport.View()
	}

	return header + body
}

func noWorkspaceView() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(StyleWarning.Render("  ◈ No workspace configured") + "\n\n")
	sb.WriteString(StyleMuted.Render("  Create a multi-repo workspace:") + "\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(colorCopperLight).Render("    mantis workspace init ~/code/auth ~/code/api ~/code/shared") + "\n\n")
	sb.WriteString(StyleMuted.Render("  This creates ") +
		lipgloss.NewStyle().Foreground(colorCopperLight).Render("mantis.workspace.yml") +
		StyleMuted.Render(" for cross-repo analysis.") + "\n\n")
	sb.WriteString(StyleMuted.Render("  Then use:") + "\n")
	sb.WriteString(StyleLabel.Render("    workspace find <sym>    ") + StyleMuted.Render("— search across all repos") + "\n")
	sb.WriteString(StyleLabel.Render("    workspace impact <sym>  ") + StyleMuted.Render("— cross-repo blast radius") + "\n")
	sb.WriteString(StyleLabel.Render("    workspace stats         ") + StyleMuted.Render("— aggregate statistics") + "\n")
	return sb.String()
}

func renderWorkspace(repos []string, stats map[string]wsRepoStats, width int) string {
	var sb strings.Builder
	sepLen := width - 4
	if sepLen < 40 {
		sepLen = 40
	}

	// Summary
	totalFiles, totalSyms, totalEdges := 0, 0, 0
	for _, s := range stats {
		totalFiles += s.files
		totalSyms += s.symbols
		totalEdges += s.edges
	}

	cards := lipgloss.JoinHorizontal(
		lipgloss.Top,
		statCard("Repositories", fmt.Sprintf("%d", len(repos)), "🌐"),
		"  ",
		statCard("Total Files", fmt.Sprintf("%d", totalFiles), "▦"),
		"  ",
		statCard("Total Symbols", fmt.Sprintf("%d", totalSyms), "◈"),
	)
	sb.WriteString(cards + "\n\n")

	// Per-repo breakdown
	sb.WriteString(StyleHighlight.Render("  Repository Breakdown") + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")

	for _, name := range repos {
		s := stats[name]
		repoName := lipgloss.NewStyle().Foreground(colorCopper).Bold(true).Width(20).Render(name)

		var statusBadge string
		if s.files == 0 {
			statusBadge = StyleWarning.Render("NOT INDEXED")
		} else {
			statusBadge = StyleSuccess.Render("INDEXED")
		}

		detail := StyleMuted.Render(fmt.Sprintf("%d files · %d symbols · %d edges", s.files, s.symbols, s.edges))
		sb.WriteString(fmt.Sprintf("  %s  %s  %s\n", repoName, statusBadge, detail))
	}

	// Cross-repo graph info
	sb.WriteString("\n")
	sb.WriteString(StyleLabel.Render("  Cross-Repo Commands") + "\n")
	sb.WriteString(StyleDivider.Render("  "+strings.Repeat("─", sepLen)) + "\n")
	sb.WriteString(StyleMuted.Render("  ") +
		lipgloss.NewStyle().Foreground(colorCopperLight).Render("mantis workspace find <symbol>") +
		StyleMuted.Render("   — search across all repos") + "\n")
	sb.WriteString(StyleMuted.Render("  ") +
		lipgloss.NewStyle().Foreground(colorCopperLight).Render("mantis workspace impact <symbol>") +
		StyleMuted.Render(" — cross-repo blast radius") + "\n")

	return sb.String()
}
