package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/seedhire/mantis/internal/brain"
	"github.com/seedhire/mantis/internal/config"
	appcontext "github.com/seedhire/mantis/internal/context"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
	"github.com/seedhire/mantis/internal/linter"
	"github.com/seedhire/mantis/internal/parser"
	"github.com/seedhire/mantis/internal/repl"
	"github.com/seedhire/mantis/internal/tui"
	"github.com/seedhire/mantis/internal/viz"
)

// version is injected at build time via ldflags.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "mantis [question]",
	Short:   "AI coding assistant — free, local-first",
	Long:    `Mantis is a free AI coding assistant. Run with no args for interactive mode, or pass a question for a one-shot answer.`,
	Version: version,
	// Allow a direct question as argument: mantis "why does X break?"
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := repl.Config{
			ForceTier: replTier,
			Budget:    replBudget,
			ImagePath: replImage,
		}
		if len(args) > 0 {
			cfg.InitialQuery = strings.Join(args, " ")
		}
		return repl.Run(cfg)
	},
}

var replTier   string
var replBudget int
var replImage  string

// ── init ──────────────────────────────────────────────────────────────────────

var initLang string
var initWatch bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Index the current project",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		// Create .mantis dir
		if err := os.MkdirAll(fmt.Sprintf("%s/.mantis", root), 0o755); err != nil {
			return err
		}

		// Add .mantis to .gitignore if applicable
		gitignorePath := fmt.Sprintf("%s/.gitignore", root)
		if data, err := os.ReadFile(gitignorePath); err == nil {
			content := string(data)
			if !containsLine(content, ".mantis/") && !containsLine(content, ".mantis") {
				f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.WriteString("\n.mantis/\n")
					f.Close()
				}
			}
		}

		dbPath := config.DefaultDBPath(root)
		db, err := graph.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer db.Close()

		builder := graph.NewBuilder(db, root)
		builder.RegisterParser(&parser.GoParser{})
		builder.RegisterParser(&parser.TypeScriptParser{})
		builder.RegisterParser(&parser.PythonParser{Root: root})
		fileCount, symbolCount, err := builder.BuildFull(nil)
		if err != nil {
			return fmt.Errorf("build: %w", err)
		}
		fmt.Printf("✓ Indexed %d files, %d symbols\n", fileCount, symbolCount)

		_ = db.SetMeta("version", "0.1.0")
		_ = db.SetMeta("last_init", fmt.Sprintf("%d", time.Now().Unix()))

		if initWatch {
			watcher := graph.NewWatcher(builder, root)
			if err := watcher.Start(); err != nil {
				return fmt.Errorf("start watcher: %w", err)
			}
			fmt.Println("👁  Watching for changes... (Ctrl+C to stop)")
			waitForInterrupt()
			watcher.Stop()
		}

		return nil
	},
}

// ── context ───────────────────────────────────────────────────────────────────

var contextDepth int
var contextTokens int
var contextOut string

var contextCmd = &cobra.Command{
	Use:   "context <symbol>",
	Short: "Generate a context bundle for a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		symbol := args[0]
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		bundler := appcontext.NewBundler(db, root)
		bundle, err := bundler.Bundle(symbol, contextDepth, contextTokens)
		if err != nil {
			return fmt.Errorf("could not bundle %q: %w\nHint: run 'mantis init' first", symbol, err)
		}

		md := bundler.RenderMarkdown(bundle)

		if contextOut != "" {
			if err := os.WriteFile(contextOut, []byte(md), 0o644); err != nil {
				return err
			}
			fmt.Printf("✓ Written to %s\n", contextOut)
		} else {
			fmt.Print(md)
		}
		return nil
	},
}

// ── watch ─────────────────────────────────────────────────────────────────────

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch for file changes and update the graph",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		builder := graph.NewBuilder(db, root)
		builder.RegisterParser(&parser.GoParser{})
		builder.RegisterParser(&parser.TypeScriptParser{})
		builder.RegisterParser(&parser.PythonParser{Root: root})

		watcher := graph.NewWatcher(builder, root)
		if err := watcher.Start(); err != nil {
			return fmt.Errorf("start watcher: %w", err)
		}
		fmt.Println("👁  Watching for changes... (Ctrl+C to stop)")
		waitForInterrupt()
		watcher.Stop()
		return nil
	},
}

// ── find ──────────────────────────────────────────────────────────────────────

var findType string
var findFormat string

var findCmd = &cobra.Command{
	Use:   "find <symbol>",
	Short: "Find where a symbol is used",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		q := graph.NewQuerier(db)
		result, err := intel.Find(q, args[0], findType)
		if err != nil {
			return err
		}

		switch findFormat {
		case "json":
			paths := make([]string, len(result.Importers))
			for i, n := range result.Importers {
				paths[i] = n.FilePath
			}
			data, _ := json.MarshalIndent(paths, "", "  ")
			fmt.Println(string(data))
		case "tree":
			fmt.Printf("%q\n", result.Symbol)
			for _, n := range result.Importers {
				fmt.Printf("  └── %s\n", n.FilePath)
			}
		default: // table
			fmt.Printf("Found %d importers of %q\n", len(result.Importers), result.Symbol)
			fmt.Println(strings.Repeat("─", 50))
			fmt.Printf("%-45s %-7s %s\n", "FILE", "LINE", "LANG")
			for _, n := range result.Importers {
				fmt.Printf("%-45s %-7s %s\n", n.FilePath, "-", n.Language)
			}
		}
		return nil
	},
}

// ── impact ────────────────────────────────────────────────────────────────────

var impactDepth int
var impactRisk bool

var impactCmd = &cobra.Command{
	Use:   "impact <target>",
	Short: "Show impact of changing a file or symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		q := graph.NewQuerier(db)
		result, err := intel.Impact(q, args[0], impactDepth)
		if err != nil {
			return fmt.Errorf("impact: %w", err)
		}

		fmt.Printf("⚡ Impact Analysis: %s\n", result.Target)
		fmt.Println(strings.Repeat("─", 50))

		// Print by depth.
		depths := make([]int, 0, len(result.ByDepth))
		for d := range result.ByDepth {
			depths = append(depths, d)
		}
		sort.Ints(depths)
		for _, d := range depths {
			label := "Indirect dependents"
			if d == 1 {
				label = "Direct importers  "
			}
			fmt.Printf("%s (depth %d): %d files\n", label, d, len(result.ByDepth[d]))
		}
		fmt.Printf("Total affected: %d files\n", result.TotalFiles)

		if impactRisk && result.TotalFiles > 0 {
			fmt.Println()
			// Sort files by risk descending.
			type fileRisk struct {
				node  *graph.Node
				score int
			}
			var ranked []fileRisk
			for _, nodes := range result.ByDepth {
				for _, n := range nodes {
					ranked = append(ranked, fileRisk{n, result.RiskScores[n.ID]})
				}
			}
			sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
			for _, fr := range ranked {
				label := "LOW RISK: "
				if fr.score >= 8 {
					label = "HIGH RISK:  "
				} else if fr.score >= 5 {
					label = "MEDIUM RISK:"
				}
				fmt.Printf("%s %-40s (score: %d/10)\n", label, fr.node.FilePath, fr.score)
			}
		}
		return nil
	},
}

// ── dead ──────────────────────────────────────────────────────────────────────

var deadConfirm bool
var deadIgnore string

var deadCmd = &cobra.Command{
	Use:   "dead",
	Short: "Find dead/unused exported symbols",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		q := graph.NewQuerier(db)
		result, err := intel.FindDead(q, deadIgnore)
		if err != nil {
			return err
		}

		fmt.Println("🔍 Dead Code Analysis")
		fmt.Println(strings.Repeat("─", 50))
		fmt.Printf("Found %d unused exported symbols\n\n", result.Total)
		for _, sym := range result.Symbols {
			fmt.Printf("%-12s %s:%d    %s\n", strings.ToUpper(string(sym.Type)), sym.FilePath, sym.LineStart, sym.Name)
		}

		if deadConfirm {
			if result.Total == 0 {
				return nil
			}
			fmt.Print("\nDelete these symbols? [y/N] ")
			var answer string
			fmt.Scanln(&answer)
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Aborted.")
				return nil
			}
			fmt.Println("Use your editor or refactoring tool to remove them.")
			fmt.Println("Tip: re-run 'mantis dead' after cleanup to verify.")
		}
		return nil
	},
}

// ── circular ──────────────────────────────────────────────────────────────────

var circularHTML bool
var circularOut string

var circularCmd = &cobra.Command{
	Use:   "circular",
	Short: "Detect circular dependencies",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		q := graph.NewQuerier(db)
		result, err := intel.FindCircular(q)
		if err != nil {
			return err
		}

		fmt.Println("🔄 Circular Dependencies")
		fmt.Println(strings.Repeat("─", 50))
		fmt.Printf("Found %d circular import chains\n\n", result.Total)
		for i, cycle := range result.Cycles {
			fmt.Printf("CYCLE %d (length %d):\n", i+1, cycle.Length)
			for j, fp := range cycle.Nodes {
				if j == 0 {
					fmt.Printf("  %s\n", fp)
				} else {
					fmt.Printf("  → %s\n", fp)
				}
			}
			// Close the cycle.
			if len(cycle.Nodes) > 0 {
				fmt.Printf("  → %s\n", cycle.Nodes[0])
			}
			fmt.Println()
		}

		if circularHTML {
			outPath := circularOut
			if outPath == "" {
				outPath = "mantis-graph.html"
			}
			allFiles, err := q.GetAllFiles()
			if err != nil {
				return err
			}
			allEdges, err := q.GetAllEdges()
			if err != nil {
				return err
			}
			html := viz.GenerateHTML(allFiles, allEdges, "", 0)
			if err := os.WriteFile(outPath, []byte(html), 0o644); err != nil {
				return err
			}
			fmt.Printf("✓ Graph saved to %s\n", outPath)
		}
		return nil
	},
}

// ── graph ─────────────────────────────────────────────────────────────────────

var graphModule string
var graphDepth int
var graphOut string

var graphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Generate interactive dependency graph (D3)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return err
		}
		defer db.Close()

		q := graph.NewQuerier(db)
		allFiles, err := q.GetAllFiles()
		if err != nil {
			return err
		}
		allEdges, err := q.GetAllEdges()
		if err != nil {
			return err
		}

		html := viz.GenerateHTML(allFiles, allEdges, graphModule, graphDepth)
		outPath := graphOut
		if outPath == "" {
			outPath = "mantis-graph.html"
		}
		if err := os.WriteFile(outPath, []byte(html), 0o644); err != nil {
			return err
		}
		fmt.Printf("✓ Graph saved to %s — open in browser\n", outPath)
		return nil
	},
}

var lintStrict bool
var lintCI bool
var lintConfig string

var lintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Lint the codebase with architecture rules",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		db, err := openDB(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\nRun 'mantis init' first.\n", err)
			os.Exit(1)
		}
		defer db.Close()

		cfg, err := config.Load(root)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		if len(cfg.Rules) == 0 {
			fmt.Println("No lint rules configured.")
			fmt.Println()
			fmt.Println("Create a .mantisrc.yml in your project root. Example:")
			fmt.Println()
			fmt.Println("  version: 1")
			fmt.Println("  rules:")
			fmt.Println("    - name: no-circular-dependencies")
			fmt.Println("      type: built_in")
			fmt.Println("      severity: error")
			fmt.Println("    - name: no-controller-db-access")
			fmt.Println("      from: 'src/controllers/**'")
			fmt.Println("      disallow_import: 'src/db/**'")
			fmt.Println("      severity: error")
			return nil
		}

		q := graph.NewQuerier(db)
		runner := linter.NewRunner(q, root)
		violations, err := runner.Run(cfg)
		if err != nil {
			return fmt.Errorf("lint: %w", err)
		}

		allFiles, err := q.GetAllFiles()
		if err != nil {
			return err
		}

		fmt.Println("🔍 Architecture Lint")
		fmt.Println(strings.Repeat("─", 50))
		fmt.Printf("Checking %d rules against %d files...\n\n", len(cfg.Rules), len(allFiles))

		if len(violations) == 0 {
			fmt.Println("✓ No violations found")
			return nil
		}

		errCount, warnCount := 0, 0
		for _, v := range violations {
			label := strings.ToUpper(v.Severity)
			location := v.File
			if v.Line > 0 {
				location = fmt.Sprintf("%s:%d", v.File, v.Line)
			}
			fmt.Printf("%-7s %s\n", label, location)
			fmt.Printf("        %s\n\n", v.Message)
			if v.Severity == "error" {
				errCount++
			} else {
				warnCount++
			}
		}

		fmt.Println(strings.Repeat("─", 50))
		fmt.Printf("%d error", errCount)
		if errCount != 1 {
			fmt.Print("s")
		}
		fmt.Printf(", %d warning", warnCount)
		if warnCount != 1 {
			fmt.Print("s")
		}
		fmt.Println()

		if lintCI {
			if errCount > 0 || (lintStrict && warnCount > 0) {
				os.Exit(1)
			}
		}
		return nil
	},
}

// ── tui ───────────────────────────────────────────────────────────────────────

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return fmt.Errorf("%w\nRun 'mantis init' first", err)
		}
		defer db.Close()

		m := tui.New(db, root)
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
		_, err = p.Run()
		return err
	},
}

// ── handoff command ──────────────────────────────────────────────────────────

var handoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Generate a handoff brief for another developer or AI session",
	Long:  "Reads BRAIN.md, DECISIONS.log, REJECTED.md, CONVENTIONS.md and recent git changes to produce a structured HANDOFF.md.",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		b := brain.New(root)
		if !b.Exists() {
			return fmt.Errorf("no .mantis/ directory — run 'mantis init' first")
		}

		md := buildHandoff(b, root)
		outPath := filepath.Join(root, ".mantis", "HANDOFF.md")
		if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
			return fmt.Errorf("write handoff: %w", err)
		}

		fmt.Printf("✓ Handoff written to %s\n", outPath)
		return nil
	},
}

func buildHandoff(b *brain.Brain, root string) string {
	var sb strings.Builder
	now := time.Now().Format("2006-01-02 15:04")

	sb.WriteString("# Handoff Brief\n")
	sb.WriteString(fmt.Sprintf("> Generated: %s\n\n", now))

	// Current state from BRAIN.md.
	if brainMD := b.ReadFile("BRAIN.md"); brainMD != "" {
		sb.WriteString("## Current State\n\n")
		sb.WriteString(brainMD)
		sb.WriteString("\n\n")
	}

	// Key decisions (last 20 entries from DECISIONS.log).
	if decisions := b.ReadFile("DECISIONS.log"); decisions != "" {
		sb.WriteString("## Key Decisions\n\n")
		lines := strings.Split(decisions, "\n")
		start := 0
		if len(lines) > 20 {
			start = len(lines) - 20
		}
		sb.WriteString(strings.Join(lines[start:], "\n"))
		sb.WriteString("\n\n")
	}

	// Don't touch — rejected approaches.
	if rejected := b.ReadFile("REJECTED.md"); rejected != "" {
		sb.WriteString("## Don't Touch — Rejected Approaches\n\n")
		sb.WriteString(rejected)
		sb.WriteString("\n\n")
	}

	// Architecture rules.
	if conventions := b.ReadFile("CONVENTIONS.md"); conventions != "" {
		sb.WriteString("## Architecture Rules\n\n")
		sb.WriteString(conventions)
		sb.WriteString("\n\n")
	}

	// Hot files — recently changed files from git.
	sb.WriteString("## Hot Files (recently changed)\n\n")
	hotFiles := getRecentGitChanges(root)
	if len(hotFiles) > 0 {
		for _, f := range hotFiles {
			sb.WriteString(fmt.Sprintf("- %s\n", f))
		}
	} else {
		sb.WriteString("_No recent git changes detected._\n")
	}
	sb.WriteString("\n")

	return sb.String()
}

func getRecentGitChanges(root string) []string {
	out, err := exec.Command("git", "-C", root, "log", "--oneline", "--name-only", "--pretty=format:", "-10").Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	return files
}

// ── temporal intelligence commands ───────────────────────────────────────────

var temporalDays int

var hotspotsCmd = &cobra.Command{
	Use:   "hotspots",
	Short: "Show files with highest churn and change frequency",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		stats, err := intel.Temporal(root, temporalDays)
		if err != nil {
			return err
		}
		hotspots := intel.Hotspots(stats, 15)
		if len(hotspots) == 0 {
			fmt.Println("No file changes found in the last", temporalDays, "days.")
			return nil
		}
		fmt.Printf("%-50s %7s %7s %8s %6s\n", "FILE", "COMMITS", "AUTHORS", "CHURN", "DAYS")
		fmt.Println(strings.Repeat("─", 85))
		for _, f := range hotspots {
			days := fmt.Sprintf("%d", f.DaysSinceChange)
			if f.DaysSinceChange < 0 {
				days = "?"
			}
			fmt.Printf("%-50s %7d %7d %8.1f %6s\n", truncPath(f.Path, 50), f.Commits, f.Authors, f.ChurnScore, days)
		}
		return nil
	},
}

var riskyCmd = &cobra.Command{
	Use:   "risky",
	Short: "Show high-churn files with low bus factor (single author)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		stats, err := intel.Temporal(root, temporalDays)
		if err != nil {
			return err
		}
		risky := intel.Risky(stats, 15)
		if len(risky) == 0 {
			fmt.Println("No risky files found — good bus factor across the board.")
			return nil
		}
		fmt.Printf("%-50s %7s %8s %s\n", "FILE", "COMMITS", "CHURN", "SOLE AUTHOR")
		fmt.Println(strings.Repeat("─", 85))
		for _, f := range risky {
			author := f.LastAuthor
			if len(f.AuthorNames) > 0 {
				author = f.AuthorNames[0]
			}
			fmt.Printf("%-50s %7d %8.1f %s\n", truncPath(f.Path, 50), f.Commits, f.ChurnScore, author)
		}
		return nil
	},
}

var couplingCmd = &cobra.Command{
	Use:   "coupling [path]",
	Short: "Show files that frequently change together",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		stats, err := intel.Temporal(root, temporalDays)
		if err != nil {
			return err
		}

		if len(args) == 1 {
			coupled := intel.CouplingFor(stats, args[0], 15)
			if len(coupled) == 0 {
				fmt.Printf("No coupling data for %s\n", args[0])
				return nil
			}
			fmt.Printf("Files that change with %s:\n\n", args[0])
			fmt.Printf("%-50s %8s %8s\n", "FILE", "CO-CHGS", "COUPLING")
			fmt.Println(strings.Repeat("─", 70))
			for _, c := range coupled {
				other := c.FileB
				if other == args[0] {
					other = c.FileA
				}
				fmt.Printf("%-50s %8d %7.0f%%\n", truncPath(other, 50), c.CoChanges, c.Coupling*100)
			}
		} else {
			if len(stats.Coupling) == 0 {
				fmt.Println("No file coupling detected in the last", temporalDays, "days.")
				return nil
			}
			fmt.Printf("%-40s %-40s %8s\n", "FILE A", "FILE B", "COUPLING")
			fmt.Println(strings.Repeat("─", 92))
			limit := 20
			if len(stats.Coupling) < limit {
				limit = len(stats.Coupling)
			}
			for _, c := range stats.Coupling[:limit] {
				fmt.Printf("%-40s %-40s %7.0f%%\n", truncPath(c.FileA, 40), truncPath(c.FileB, 40), c.Coupling*100)
			}
		}
		return nil
	},
}

func truncPath(p string, maxLen int) string {
	if len(p) <= maxLen {
		return p
	}
	return "…" + p[len(p)-maxLen+1:]
}

// ── helpers ───────────────────────────────────────────────────────────────────

func openDB(root string) (*graph.DB, error) {
	dbPath := config.DefaultDBPath(root)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("database not found — run 'mantis init' first")
	}
	db, err := graph.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

func containsLine(content, line string) bool {
	for _, l := range splitLines(content) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func waitForInterrupt() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	fmt.Println()
}

// ── main ──────────────────────────────────────────────────────────────────────

func init() {
	initCmd.Flags().StringVar(&initLang, "lang", "ts", "Primary language (ts, py)")
	initCmd.Flags().BoolVar(&initWatch, "watch", false, "Start file watcher after indexing")

	contextCmd.Flags().IntVar(&contextDepth, "depth", 3, "Max import depth to traverse")
	contextCmd.Flags().IntVar(&contextTokens, "tokens", 8000, "Token budget for context")
	contextCmd.Flags().StringVar(&contextOut, "out", "", "Write output to file instead of stdout")

	findCmd.Flags().StringVar(&findType, "type", "importer", "Find type: importer, caller, reference")
	findCmd.Flags().StringVar(&findFormat, "format", "table", "Output format: table, json, tree")

	impactCmd.Flags().IntVar(&impactDepth, "depth", 5, "Max BFS depth for impact analysis")
	impactCmd.Flags().BoolVar(&impactRisk, "risk", false, "Show risk scores for impacted files")

	deadCmd.Flags().BoolVar(&deadConfirm, "confirm", false, "Prompt to delete dead symbols")
	deadCmd.Flags().StringVar(&deadIgnore, "ignore", "", "Comma-separated glob patterns to ignore")

	circularCmd.Flags().BoolVar(&circularHTML, "html", false, "Also generate an HTML dependency graph")
	circularCmd.Flags().StringVar(&circularOut, "out", "", "Output file for HTML graph (default: mantis-graph.html)")

	graphCmd.Flags().StringVar(&graphModule, "module", "", "Focus on a specific module/path")
	graphCmd.Flags().IntVar(&graphDepth, "depth", 3, "Max depth for graph traversal")
	graphCmd.Flags().StringVar(&graphOut, "out", "mantis-graph.html", "Output file path")

	lintCmd.Flags().BoolVar(&lintStrict, "strict", false, "Treat warnings as errors for exit code")
	lintCmd.Flags().BoolVar(&lintCI, "ci", false, "Exit with code 1 on any violation")
	lintCmd.Flags().StringVar(&lintConfig, "config", ".mantisrc.yml", "Path to config file")

	rootCmd.Flags().StringVar(&replTier, "model", "", "Force model tier: fast · smart · heavy · vision")
	rootCmd.Flags().IntVar(&replBudget, "budget", 0, "Max tokens for this session (0 = unlimited)")
	rootCmd.Flags().StringVar(&replImage, "image", "", "Image file path for multimodal input")

	rootCmd.AddCommand(initCmd, contextCmd, watchCmd, findCmd, impactCmd, deadCmd, circularCmd, graphCmd, lintCmd, tuiCmd, handoffCmd, hotspotsCmd, riskyCmd, couplingCmd)

	hotspotsCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
	riskyCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
	couplingCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
