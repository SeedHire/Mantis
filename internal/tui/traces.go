package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ── Messages ──────────────────────────────────────────────────────────────────

type tracesResultMsg struct {
	hot        []intel.TraceStats
	cold       []intel.TraceStats
	totalCalls int
	unique     int
	err        error
}

// ── Model ─────────────────────────────────────────────────────────────────────

type TracesModel struct {
	db       *graph.DB
	viewport viewport.Model
	loading  bool
	err      error
	ready    bool
	noData   bool
	width    int
	height   int
}

func NewTracesModel(db *graph.DB) TracesModel {
	vp := viewport.New(80, 20)
	return TracesModel{db: db, viewport: vp, loading: true}
}

func (m *TracesModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = h - 6
	m.ready = true
}

func (m TracesModel) Init() tea.Cmd {
	return m.load()
}

func (m TracesModel) load() tea.Cmd {
	db := m.db
	return func() tea.Msg {
		conn := db.Conn()
		totalCalls, unique, err := intel.TraceSummary(conn)
		if err != nil {
			return tracesResultMsg{err: err}
		}
		if totalCalls == 0 {
			return tracesResultMsg{totalCalls: 0, unique: 0}
		}
		hot, _ := intel.Hotpaths(conn, 15)
		cold, _ := intel.ColdPaths(conn, 15)
		return tracesResultMsg{
			hot:        hot,
			cold:       cold,
			totalCalls: totalCalls,
			unique:     unique,
		}
	}
}

func (m TracesModel) Update(msg tea.Msg) (TracesModel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case tracesResultMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil && msg.totalCalls == 0 {
			m.noData = true
		} else if msg.err == nil {
			m.noData = false
			m.viewport.SetContent(renderTraces(msg.hot, msg.cold, msg.totalCalls, msg.unique, m.width))
		}

	case tea.KeyMsg:
		if msg.String() == "r" {
			m.loading = true
			m.err = nil
			m.noData = false
			cmds = append(cmds, m.load())
			return m, tea.Batch(cmds...)
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m TracesModel) View() string {
	w := m.width
	if w < 10 {
		w = 80
	}
	sep := StyleDivider.Render(strings.Repeat("─", w))
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		StyleTitle.Render("  ⚡  Runtime Traces"),
		sep,
		StyleMuted.Render("  OTLP · pprof · custom traces  ·  r refresh  ·  ↓↑ scroll"),
		sep,
		"",
	)

	var body string
	switch {
	case m.loading:
		body = StyleMuted.Render("  Loading trace data…")
	case m.err != nil:
		body = StyleError.Render("  ✗  "+m.err.Error())
	case m.noData:
		body = noTraceDataView()
	default:
		body = m.viewport.View()
		if m.viewport.TotalLineCount() > m.viewport.Height {
			body += "\n" + StyleMuted.Render(fmt.Sprintf(
				"  %d/%d lines", m.viewport.YOffset+m.viewport.Height, m.viewport.TotalLineCount()))
		}
	}

	return header + body
}

func noTraceDataView() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(StyleWarning.Render("  ◈ No trace data ingested yet") + "\n\n")
	sb.WriteString(StyleMuted.Render("  Ingest runtime traces to see hotpaths and cold code:") + "\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(colorGold).Render("    mantis trace ingest traces.json") + "\n\n")
	sb.WriteString(StyleMuted.Render("  Supported formats:") + "\n")
	sb.WriteString(StyleLabel.Render("    OTLP JSON  ") + StyleMuted.Render("— OpenTelemetry export (Jaeger, otel-cli)") + "\n")
	sb.WriteString(StyleLabel.Render("    Go pprof   ") + StyleMuted.Render("— go tool pprof -text output") + "\n")
	sb.WriteString(StyleLabel.Render("    Custom     ") + StyleMuted.Render("— [{function, calls, duration_ms}]") + "\n")
	return sb.String()
}

func renderTraces(hot, cold []intel.TraceStats, totalCalls, unique, width int) string {
	var sb strings.Builder
	sepLen := width - 4
	if sepLen < 40 {
		sepLen = 40
	}

	// Summary cards
	cards := lipgloss.JoinHorizontal(
		lipgloss.Top,
		statCard("Total Calls", fmt.Sprintf("%d", totalCalls), "⚡"),
		"  ",
		statCard("Unique Funcs", fmt.Sprintf("%d", unique), "◈"),
	)
	sb.WriteString(cards + "\n\n")

	// ── Hot paths ──
	sb.WriteString(StyleError.Render("  🔥 Hottest Paths") + StyleMuted.Render("  (most runtime calls)") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	if len(hot) == 0 {
		sb.WriteString(StyleMuted.Render("  No hot paths found.\n"))
	}
	maxCalls := 1
	if len(hot) > 0 {
		maxCalls = hot[0].CallCount
	}
	for i, t := range hot {
		rank := lipgloss.NewStyle().Foreground(colorError).Bold(true).Width(4).Render(fmt.Sprintf("#%d", i+1))
		pct := float64(t.CallCount) / float64(maxCalls) * 100
		bar := renderBar(pct, 15)
		calls := lipgloss.NewStyle().Foreground(colorGold).Bold(true).Width(8).Render(fmt.Sprintf("%dx", t.CallCount))
		dur := lipgloss.NewStyle().Foreground(colorCopperLight).Width(10).Render(fmtDuration(t.AvgDuration))
		name := StyleMuted.Render(t.Name)
		sb.WriteString(fmt.Sprintf("  %s %s %s %s  %s\n", rank, bar, calls, dur, name))
	}

	// ── Cold paths ──
	sb.WriteString("\n")
	sb.WriteString(StyleSuccess.Render("  ❄ Cold Code") + StyleMuted.Render("  (structurally connected, rarely called)") + "\n")
	sb.WriteString(StyleDivider.Render("  " + strings.Repeat("─", sepLen)) + "\n")

	if len(cold) == 0 {
		sb.WriteString(StyleMuted.Render("  No cold paths found.\n"))
	}
	for i, t := range cold {
		rank := lipgloss.NewStyle().Foreground(colorSuccess).Width(4).Render(fmt.Sprintf("#%d", i+1))
		calls := lipgloss.NewStyle().Foreground(colorFgMuted).Width(8).Render(fmt.Sprintf("%dx", t.CallCount))
		name := StyleMuted.Render(t.Name)
		path := lipgloss.NewStyle().Foreground(colorBorderHi).Render(truncPath(t.FilePath, 30))
		sb.WriteString(fmt.Sprintf("  %s %s  %s  %s\n", rank, calls, name, path))
	}

	return sb.String()
}

func fmtDuration(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	if ms >= 1 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.0fµs", ms*1000)
}
