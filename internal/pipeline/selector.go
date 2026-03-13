package pipeline

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// RunSelector launches an interactive Bubbletea program for the user to answer
// clarifying questions. Returns nil if the user pressed Esc to skip.
func RunSelector(questions []ClarifyQuestion) *ClarifyResult {
	// Non-terminal: auto-select defaults.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		answers := make([]int, len(questions))
		for i, q := range questions {
			answers[i] = q.Default
		}
		return &ClarifyResult{Questions: questions, Answers: answers}
	}

	m := selectorModel{
		questions: questions,
		cursors:   make([]int, len(questions)),
		answers:   make([]int, len(questions)),
	}
	// Set initial cursors to defaults.
	for i, q := range questions {
		m.cursors[i] = q.Default
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return nil
	}
	fm := final.(selectorModel)
	if fm.escaped {
		return nil
	}
	return &ClarifyResult{Questions: questions, Answers: fm.answers}
}

type selectorModel struct {
	questions []ClarifyQuestion
	current   int   // which question we're on
	cursors   []int // cursor position per question
	answers   []int // confirmed answer per question
	done      bool
	escaped   bool
}

func (m selectorModel) Init() tea.Cmd {
	return nil
}

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.escaped = true
			return m, tea.Quit

		case "up", "k":
			if m.cursors[m.current] > 0 {
				m.cursors[m.current]--
			}

		case "down", "j":
			max := len(m.questions[m.current].Options) - 1
			if m.cursors[m.current] < max {
				m.cursors[m.current]++
			}

		case "enter":
			m.answers[m.current] = m.cursors[m.current]
			if m.current < len(m.questions)-1 {
				m.current++
			} else {
				m.done = true
				return m, tea.Quit
			}

		case "1", "2", "3", "4", "5":
			idx := int(msg.String()[0]-'0') - 1
			opts := m.questions[m.current].Options
			if idx >= 0 && idx < len(opts) {
				m.cursors[m.current] = idx
			}
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	if m.done {
		return ""
	}

	q := m.questions[m.current]
	var sb strings.Builder

	header := fmt.Sprintf("Clarifying Questions (%d/%d)", m.current+1, len(m.questions))
	sb.WriteString(fmt.Sprintf("\n  \033[38;5;220m╭─ %s ─╮\033[0m\n\n", header))
	sb.WriteString(fmt.Sprintf("  %s\n\n", q.Question))

	for i, opt := range q.Options {
		if i == m.cursors[m.current] {
			sb.WriteString(fmt.Sprintf("  \033[38;5;220m> %d. %s\033[0m\n", i+1, opt))
		} else {
			sb.WriteString(fmt.Sprintf("    %d. %s\n", i+1, opt))
		}
	}

	sb.WriteString(fmt.Sprintf("\n  \033[38;5;244m↑↓ navigate · enter select · esc skip\033[0m\n"))
	return sb.String()
}
