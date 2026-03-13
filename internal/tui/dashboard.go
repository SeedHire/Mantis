package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
)

// ASCII logo — rendered in copper
const logo = `
███╗   ███╗ █████╗ ███╗   ██╗████████╗██╗███████╗
████╗ ████║██╔══██╗████╗  ██║╚══██╔══╝██║██╔════╝
██╔████╔██║███████║██╔██╗ ██║   ██║   ██║███████╗
██║╚██╔╝██║██╔══██║██║╚██╗██║   ██║   ██║╚════██║
██║ ╚═╝ ██║██║  ██║██║ ╚████║   ██║   ██║███████║
╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝   ╚═╝   ╚═╝╚══════╝`

const mascot = `  
    \   /
     \_/
    (o_o)
<===[|||]==>`

// ── Messages ──────────────────────────────────────────────────────────────────

type dashboardLoadedMsg struct {
	files    int
	symbols  int
	edges    int
	lastInit string
}

type dashboardErrMsg struct{ err error }

// ── Model ─────────────────────────────────────────────────────────────────────

type DashboardModel struct {
	db      *graph.DB
	data    *dashboardLoadedMsg
	err     error
	loading bool
	width   int
}

func NewDashboardModel(db *graph.DB) DashboardModel {
	return DashboardModel{db: db, loading: true}
}

func (m DashboardModel) Init() tea.Cmd {
	return m.load()
}

func (m DashboardModel) load() tea.Cmd {
	return func() tea.Msg {
		q := graph.NewQuerier(m.db)
		files, err := q.GetAllFiles()
		if err != nil {
			return dashboardErrMsg{err}
		}
		allNodes, err := q.FindAllNodes("")
		if err != nil {
			return dashboardErrMsg{err}
		}
		edges, err := q.GetAllEdges()
		if err != nil {
			return dashboardErrMsg{err}
		}
		lastInit, _ := m.db.GetMeta("last_init")
		lastInitStr := "not yet indexed"
		if lastInit != "" {
			var ts int64
			if _, err := fmt.Sscan(lastInit, &ts); err == nil {
				lastInitStr = time.Unix(ts, 0).Format("Mon 02 Jan 2006  ·  15:04:05")
			}
		}
		return dashboardLoadedMsg{
			files:    len(files),
			symbols:  len(allNodes) - len(files),
			edges:    len(edges),
			lastInit: lastInitStr,
		}
	}
}

func (m DashboardModel) Update(msg tea.Msg) (DashboardModel, tea.Cmd) {
	switch msg := msg.(type) {
	case dashboardLoadedMsg:
		m.loading = false
		m.data = &msg
	case dashboardErrMsg:
		m.loading = false
		m.err = msg.err
	case tea.WindowSizeMsg:
		m.width = msg.Width
	}
	return m, nil
}

func (m DashboardModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}

	// ── Logo + mascot side-by-side ────────────────────────────────────────────
	logoStyled := StyleLogoBox.Render(logo)
	mascotStyled := lipgloss.NewStyle().
		Foreground(colorCopperLight).
		Bold(true).
		PaddingLeft(4).
		PaddingTop(1).
		Render(mascot)
	header := lipgloss.JoinHorizontal(lipgloss.Top, logoStyled, "    ", mascotStyled)

	// ── Tagline ───────────────────────────────────────────────────────────────
	tagline := StyleLogoTagline.Render(
		"Codebase Intelligence Engine  ·  Local-first  ·  No cloud  ·  No upload",
	)

	// ── Divider ───────────────────────────────────────────────────────────────
	divider := StyleDivider.Render(strings.Repeat("─", clamp(w-2, 10, 72)))

	// ── Body ─────────────────────────────────────────────────────────────────
	var body string
	if m.loading {
		body = StyleMuted.Render("  Loading project stats…")
	} else if m.err != nil {
		body = StyleError.Render("  ✗  "+m.err.Error()) +
			"\n\n" + StyleMuted.Render("  Run  ") +
			lipgloss.NewStyle().Foreground(colorCopperLight).Render("mantis init --lang ts") +
			StyleMuted.Render("  to index your project.")
	} else {
		d := m.data

		cards := lipgloss.JoinHorizontal(
			lipgloss.Top,
			statCard("Files indexed", fmt.Sprintf("%d", d.files), "▦"),
			"  ",
			statCard("Symbols found", fmt.Sprintf("%d", d.symbols), "◈"),
			"  ",
			statCard("Import edges", fmt.Sprintf("%d", d.edges), "⇢"),
		)

		lastLine := StyleMuted.Render("  Last indexed  ") +
			lipgloss.NewStyle().Foreground(colorCopperLight).Render(d.lastInit)

		ref := lipgloss.JoinVertical(
			lipgloss.Left,
			StyleSectionHeader.Render("  Quick Reference"),
			quickRow("2", "Search", "Find all importers of a symbol"),
			quickRow("3", "Impact", "Blast-radius + risk scores before a refactor"),
			quickRow("4", "Lint", "Enforce architecture rules from .mantisrc.yml"),
			quickRow("5", "Dead Code", "Exported symbols with zero references"),
			quickRow("6", "Hotspots", "Git churn, risk zones, co-change coupling"),
			quickRow("7", "Traces", "Runtime hotpaths, cold code, weighted impact"),
			quickRow("8", "Workspace", "Cross-repo graph intelligence"),
			quickRow("9", "Brain", "BRAIN.md, decisions, rejected approaches"),
			quickRow("0", "Router", "7-tier model mapping and classification"),
		)

		body = lipgloss.JoinVertical(
			lipgloss.Left,
			cards,
			"",
			lastLine,
			"",
			divider,
			"",
			ref,
		)
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		tagline,
		"",
		divider,
		"",
		body,
	)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func statCard(label, value, icon string) string {
	top := lipgloss.NewStyle().
		Foreground(colorFgMuted).
		Render(icon + "  " + label)
	num := lipgloss.NewStyle().
		Bold(true).
		Foreground(colorCopperLight).
		Render(value)
	return StyleStatCard.Render(top + "\n" + num)
}

func quickRow(key, name, desc string) string {
	badge := lipgloss.NewStyle().
		Bold(true).
		Foreground(colorBG).
		Background(colorCopper).
		Padding(0, 1).
		Render(key)
	nm := lipgloss.NewStyle().
		Bold(true).
		Foreground(colorCopperLight).
		Width(12).
		Render(name)
	ds := StyleMuted.Render(desc)
	return "  " + badge + "  " + nm + ds + "\n"
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
