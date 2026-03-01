package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type deadResultMsg struct {
	result *intel.DeadResult
	err    error
}

// ── List item adapter ─────────────────────────────────────────────────────────

type deadItem struct {
	node *graph.Node
}

func (i deadItem) Title() string {
	return fmt.Sprintf("[%s]  %s", strings.ToUpper(string(i.node.Type)), i.node.Name)
}

func (i deadItem) Description() string {
	return fmt.Sprintf("%s:%d", i.node.FilePath, i.node.LineStart)
}

func (i deadItem) FilterValue() string {
	return i.node.Name + " " + i.node.FilePath + " " + string(i.node.Type)
}

// ── Model ─────────────────────────────────────────────────────────────────────

type DeadModel struct {
	db      *graph.DB
	list    list.Model
	loading bool
	err     error
	total   int
	width   int
	height  int
}

func NewDeadModel(db *graph.DB) DeadModel {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(colorFg).
		BorderLeftForeground(colorPrimary)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(colorMuted).
		BorderLeftForeground(colorPrimary)

	l := list.New(nil, delegate, 0, 0)
	l.Title = "Unused Exported Symbols"
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = StyleLabel

	return DeadModel{
		list:    l,
		loading: true,
	}
}

func (m *DeadModel) SetDB(db *graph.DB) { m.db = db }

func (m *DeadModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.list.SetSize(w, h-6)
}

func (m DeadModel) Init() tea.Cmd {
	return m.load()
}

func (m DeadModel) load() tea.Cmd {
	db := m.db
	if db == nil {
		return nil
	}
	return func() tea.Msg {
		q := graph.NewQuerier(db)
		result, err := intel.FindDead(q, "")
		return deadResultMsg{result: result, err: err}
	}
}

func (m DeadModel) Update(msg tea.Msg) (DeadModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case deadResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.result != nil {
			m.total = msg.result.Total
			items := make([]list.Item, len(msg.result.Symbols))
			for i, n := range msg.result.Symbols {
				items[i] = deadItem{node: n}
			}
			cmds = append(cmds, m.list.SetItems(items))
			m.list.Title = fmt.Sprintf("Unused Exported Symbols — %d found", m.total)
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.err = nil
			cmds = append(cmds, m.load())
		}
	}

	var lCmd tea.Cmd
	m.list, lCmd = m.list.Update(msg)
	cmds = append(cmds, lCmd)

	return m, tea.Batch(cmds...)
}

func (m DeadModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  ◌  Dead Code"),
		sep,
		StyleMuted.Render("  exported symbols with zero references  ·  r refresh  ·  / filter"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Scanning for unused exports…")
	case m.err != nil:
		body = StyleError.Render("  ✗  "+m.err.Error()) +
			"\n" + StyleMuted.Render("  Run 'mantis init' first.")
	case m.total == 0:
		body = StyleSuccess.Render("  ✓  No dead code found — all exported symbols are referenced.")
	default:
		body = m.list.View()
	}

	return header + body
}
