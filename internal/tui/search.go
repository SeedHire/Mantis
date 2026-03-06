package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type searchResultMsg struct {
	symbol    string
	importers []*graph.Node
	err       error
}

// ── List item adapter ─────────────────────────────────────────────────────────

type searchItem struct {
	node *graph.Node
}

func (i searchItem) Title() string { return i.node.Name }
func (i searchItem) Description() string {
	return fmt.Sprintf("%s:%d", i.node.FilePath, i.node.LineStart)
}
func (i searchItem) FilterValue() string { return i.node.Name + " " + i.node.FilePath }

// ── Model ─────────────────────────────────────────────────────────────────────

type SearchModel struct {
	db      *graph.DB
	input   textinput.Model
	list    list.Model
	loading bool
	err     error
	width   int
	height  int
}

func NewSearchModel(db *graph.DB) SearchModel {
	ti := textinput.New()
	ti.Placeholder = "symbol or file name…"
	ti.Focus()
	ti.CharLimit = 200
	ti.Width = 50

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(colorFg).
		BorderLeftForeground(colorPrimary)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(colorMuted).
		BorderLeftForeground(colorPrimary)

	l := list.New(nil, delegate, 0, 0)
	l.Title = "Importers"
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = StyleLabel

	return SearchModel{
		db:    db,
		input: ti,
		list:  l,
	}
}

func (m SearchModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m *SearchModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if w > 8 {
		m.input.Width = w - 8
	}
	m.list.SetSize(w, h-8)
}

func (m SearchModel) runSearch() tea.Cmd {
	query := strings.TrimSpace(m.input.Value())
	if query == "" {
		return nil
	}
	db := m.db
	return func() tea.Msg {
		q := graph.NewQuerier(db)
		nodes, err := q.FindNodeByName(query)
		if err != nil {
			return searchResultMsg{symbol: query, err: err}
		}
		// Collect unique importers across all matching nodes
		seen := map[string]bool{}
		var importers []*graph.Node
		for _, n := range nodes {
			imps, err := q.GetImporters(n.ID)
			if err != nil {
				continue
			}
			for _, imp := range imps {
				if !seen[imp.ID] {
					seen[imp.ID] = true
					importers = append(importers, imp)
				}
			}
		}
		return searchResultMsg{symbol: query, importers: importers}
	}
}

func (m SearchModel) Update(msg tea.Msg) (SearchModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case searchResultMsg:
		m.loading = false
		m.err = msg.err
		items := make([]list.Item, len(msg.importers))
		for i, n := range msg.importers {
			items[i] = searchItem{node: n}
		}
		cmd := m.list.SetItems(items)
		m.list.Title = fmt.Sprintf("Importers of %q — %d found", msg.symbol, len(msg.importers))
		cmds = append(cmds, cmd)

	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if m.input.Focused() {
				m.loading = true
				m.err = nil
				cmds = append(cmds, m.runSearch())
				return m, tea.Batch(cmds...)
			}
		case "esc":
			if !m.input.Focused() {
				m.input.Focus()
				return m, tea.Batch(cmds...)
			}
		case "down", "j":
			if m.input.Focused() && len(m.list.Items()) > 0 {
				m.input.Blur()
			}
		}
	}

	if m.input.Focused() {
		var tiCmd tea.Cmd
		m.input, tiCmd = m.input.Update(msg)
		cmds = append(cmds, tiCmd)
	} else {
		var lCmd tea.Cmd
		m.list, lCmd = m.list.Update(msg)
		cmds = append(cmds, lCmd)
	}

	return m, tea.Batch(cmds...)
}

func (m SearchModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	prompt := lipgloss.NewStyle().Foreground(colorCopper).Bold(true).Render("  ❯  ")
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  ◈  Symbol Search"),
		sep,
		prompt+m.input.View(),
		sep,
		StyleMuted.Render("  enter ↵  ·  ↓ browse results  ·  esc refocus"),
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("\n  Searching…")
	case m.err != nil:
		body = StyleError.Render("\n  ✗  "+m.err.Error()) +
			"\n" + StyleMuted.Render("  Run 'mantis init' first.")
	case len(m.list.Items()) == 0 && !m.loading:
		body = StyleMuted.Render("\n  Type a symbol name above and press enter.")
	default:
		body = m.list.View()
	}

	return header + body
}
