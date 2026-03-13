package pipeline

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// Copper palette (matches TUI).
const (
	selCopper   = "\033[38;5;173m" // copper accent
	selBold     = "\033[1m"
	selDim      = "\033[38;5;244m"
	selWhite    = "\033[38;5;255m"
	selGreen    = "\033[38;5;114m"
	selReset    = "\033[0m"
)

// RunSelector launches an interactive Bubbletea selector for the user to answer
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
		confirmed: make([]bool, len(questions)),
	}
	// Set initial cursors to defaults.
	for i, q := range questions {
		m.cursors[i] = q.Default
	}

	// Inline mode — no alt-screen, so it renders in-place like Claude Code.
	p := tea.NewProgram(m)
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
	current   int    // which question we're on
	cursors   []int  // cursor position per question
	answers   []int  // confirmed answer per question
	confirmed []bool // whether each question has been answered
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

		case "enter", " ":
			m.answers[m.current] = m.cursors[m.current]
			m.confirmed[m.current] = true
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
				// Number key: select AND confirm immediately.
				m.cursors[m.current] = idx
				m.answers[m.current] = idx
				m.confirmed[m.current] = true
				if m.current < len(m.questions)-1 {
					m.current++
				} else {
					m.done = true
					return m, tea.Quit
				}
			}

		case "left", "h":
			// Go back to previous question.
			if m.current > 0 {
				m.current--
			}
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	if m.done {
		// Show final summary of choices.
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("\n  %s◈ Choices confirmed%s\n", selCopper, selReset))
		for i, q := range m.questions {
			ans := m.answers[i]
			sb.WriteString(fmt.Sprintf("  %s%d.%s %s %s→ %s%s\n",
				selDim, i+1, selReset,
				q.Question,
				selCopper, q.Options[ans], selReset))
		}
		sb.WriteString("\n")
		return sb.String()
	}

	var sb strings.Builder

	// Header with progress.
	sb.WriteString(fmt.Sprintf("\n  %s%s◈ Question %d of %d%s\n",
		selCopper, selBold, m.current+1, len(m.questions), selReset))

	// Show already-answered questions as compact summary.
	for i := 0; i < m.current; i++ {
		ans := m.answers[i]
		sb.WriteString(fmt.Sprintf("  %s✓ %s → %s%s\n",
			selGreen, m.questions[i].Question, m.questions[i].Options[ans], selReset))
	}
	if m.current > 0 {
		sb.WriteString("\n")
	}

	// Current question.
	q := m.questions[m.current]
	sb.WriteString(fmt.Sprintf("  %s%s%s\n\n", selWhite, q.Question, selReset))

	// Options with radio-button style.
	for i, opt := range q.Options {
		if i == m.cursors[m.current] {
			sb.WriteString(fmt.Sprintf("  %s● %d  %s%s%s\n", selCopper, i+1, selBold, opt, selReset))
		} else {
			sb.WriteString(fmt.Sprintf("  %s○ %d  %s%s\n", selDim, i+1, opt, selReset))
		}
	}

	// Footer hints.
	sb.WriteString(fmt.Sprintf("\n  %s↑↓ move · 1-5 quick select · enter confirm · esc skip%s\n", selDim, selReset))

	return sb.String()
}
