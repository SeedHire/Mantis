package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/config"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/linter"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type lintResultMsg struct {
	violations []linter.Violation
	fileCount  int
	ruleCount  int
	err        error
}

type lintRunCmd struct{}

// ── List item adapter ─────────────────────────────────────────────────────────

type lintItem struct {
	v linter.Violation
}

func (i lintItem) Title() string {
	sev := StyleWarning.Render("WARN")
	if i.v.Severity == "error" {
		sev = StyleError.Render("ERR ")
	}
	loc := i.v.File
	if i.v.Line > 0 {
		loc = fmt.Sprintf("%s:%d", i.v.File, i.v.Line)
	}
	return sev + "  " + loc
}
func (i lintItem) Description() string { return i.v.Message }
func (i lintItem) FilterValue() string { return i.v.File + " " + i.v.Message + " " + i.v.Rule }

// ── Model ─────────────────────────────────────────────────────────────────────

type LintModel struct {
	db      *graph.DB
	root    string
	list    list.Model
	loading bool
	err     error
	summary string
	width   int
	height  int
}

func NewLintModel(db *graph.DB, root string) LintModel {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(colorFg).
		BorderLeftForeground(colorPrimary)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(colorMuted).
		BorderLeftForeground(colorPrimary)

	l := list.New(nil, delegate, 0, 0)
	l.Title = "Lint Violations"
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = StyleLabel

	return LintModel{
		db:   db,
		root: root,
		list: l,
	}
}

func (m *LintModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.list.SetSize(w, h-6)
}

func (m LintModel) Init() tea.Cmd {
	return m.runLint()
}

func (m LintModel) runLint() tea.Cmd {
	db := m.db
	root := m.root
	return func() tea.Msg {
		cfg, err := config.Load(root)
		if err != nil {
			return lintResultMsg{err: err}
		}
		if len(cfg.Rules) == 0 {
			return lintResultMsg{
				violations: nil,
				ruleCount:  0,
			}
		}
		q := graph.NewQuerier(db)
		runner := linter.NewRunner(q, root)
		vs, err := runner.Run(cfg)
		if err != nil {
			return lintResultMsg{err: err}
		}
		files, _ := q.GetAllFiles()
		return lintResultMsg{
			violations: vs,
			fileCount:  len(files),
			ruleCount:  len(cfg.Rules),
		}
	}
}

func (m LintModel) Update(msg tea.Msg) (LintModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case lintResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			items := make([]list.Item, len(msg.violations))
			for i, v := range msg.violations {
				items[i] = lintItem{v}
			}
			cmds = append(cmds, m.list.SetItems(items))

			errCount := 0
			warnCount := 0
			for _, v := range msg.violations {
				if v.Severity == "error" {
					errCount++
				} else {
					warnCount++
				}
			}

			if msg.ruleCount == 0 {
				m.summary = StyleMuted.Render("No rules configured in .mantisrc.yml")
			} else if len(msg.violations) == 0 {
				m.summary = StyleSuccess.Render(fmt.Sprintf("✓ No violations — %d rule(s) checked %d files", msg.ruleCount, msg.fileCount))
			} else {
				parts := []string{}
				if errCount > 0 {
					parts = append(parts, StyleError.Render(fmt.Sprintf("%d error(s)", errCount)))
				}
				if warnCount > 0 {
					parts = append(parts, StyleWarning.Render(fmt.Sprintf("%d warning(s)", warnCount)))
				}
				m.summary = strings.Join(parts, StyleMuted.Render("  ·  "))
				m.summary += StyleMuted.Render(fmt.Sprintf("  (%d rules, %d files)", msg.ruleCount, msg.fileCount))
			}
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.summary = ""
			cmds = append(cmds, m.runLint())
		}
	}

	var lCmd tea.Cmd
	m.list, lCmd = m.list.Update(msg)
	cmds = append(cmds, lCmd)

	return m, tea.Batch(cmds...)
}

func (m LintModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  ⬡  Architecture Lint"),
		sep,
		StyleMuted.Render("  r re-run  ·  / filter"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Running lint rules against .mantisrc.yml…")
	case m.err != nil:
		body = StyleError.Render("  ✗  "+m.err.Error()) +
			"\n" + StyleMuted.Render("  Run 'mantis init' first.")
	default:
		body = "  " + m.summary + "\n\n" + m.list.View()
	}

	return header + body
}
