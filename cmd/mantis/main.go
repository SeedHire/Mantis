package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	"github.com/seedhire/mantis/internal/lsp"
	"github.com/seedhire/mantis/internal/mcp"
	"github.com/seedhire/mantis/internal/parser"
	"github.com/seedhire/mantis/internal/repl"
	"github.com/seedhire/mantis/internal/tui"
	"github.com/seedhire/mantis/internal/viz"
)

// version is injected at build time via ldflags.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:          "mantis [question]",
	Short:        "AI coding assistant — free, local-first",
	Long:         `Mantis is a free AI coding assistant. Run with no args for interactive mode, or pass a question for a one-shot answer.`,
	Version:      version,
	SilenceUsage: true, // Don't dump full help text on runtime errors
	// Allow a direct question as argument: mantis "why does X break?"
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// --api-key flags: set as comma-separated env var for REPL to pick up.
		if len(replAPIKeys) > 0 {
			// Set first key as OLLAMA_API_KEY for immediate client creation.
			os.Setenv("OLLAMA_API_KEY", replAPIKeys[0])
			// Set all keys for KeyRing.
			os.Setenv("OLLAMA_API_KEYS", strings.Join(replAPIKeys, ","))
		}
		cfg := repl.Config{
			ForceTier: replTier,
			Budget:    replBudget,
			ImagePath: replImage,
			PlanMode:  replPlan,
			Continue:  replContinue,
			Version:   version,
			Offline:   replOffline,
		}
		if len(args) > 0 {
			cfg.InitialQuery = strings.Join(args, " ")
		}
		return repl.Run(cfg)
	},
}

var replTier string
var replBudget int
var replImage string
var replPlan bool
var replContinue bool
var replOffline bool
var replAPIKeys []string

var evalOffline bool
var evalCompare bool

// ── init ──────────────────────────────────────────────────────────────────────

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

		indexStart := time.Now()
		fmt.Printf("\033[38;5;244m⠋ indexing project…\033[0m")
		fileCount, symbolCount, err := builder.BuildFull(nil)
		fmt.Printf("\r\033[K")
		if err != nil {
			return fmt.Errorf("build: %w", err)
		}
		fmt.Printf("✓ Indexed %d files, %d symbols  \033[38;5;244m(%.1fs)\033[0m\n", fileCount, symbolCount, time.Since(indexStart).Seconds())

		_ = db.SetMeta("version", "0.1.0")
		_ = db.SetMeta("last_init", fmt.Sprintf("%d", time.Now().Unix()))

		// Seed brain files + auto-discover conventions.
		b := brain.New(root)
		_ = b.Init()
		if n := b.DiscoverConventions(); n > 0 {
			fmt.Printf("✓ Discovered %d conventions  \033[38;5;244m(.mantis/CONVENTIONS.md)\033[0m\n", n)
		}

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
		hotspots := intel.Hotspots(stats, 20)
		if len(hotspots) == 0 {
			fmt.Println("No file changes found in the last", temporalDays, "days.")
			return nil
		}

		// Split into refactor candidates (high churn, single/few authors) and watch list (many authors).
		var refactor, watch []intel.FileChurn
		for _, f := range hotspots {
			if f.Authors <= 1 && f.Commits >= 3 {
				refactor = append(refactor, f)
			} else if f.Authors > 1 {
				watch = append(watch, f)
			}
		}

		header := fmt.Sprintf("%-50s %7s %7s %8s %s", "FILE", "COMMITS", "AUTHORS", "CHURN", "TOP AUTHOR")
		sep := strings.Repeat("─", 90)

		if len(refactor) > 0 {
			fmt.Println("\n\033[38;5;197mRefactor Candidates\033[0m  (high churn · single author · bus factor 1)")
			fmt.Println(header)
			fmt.Println(sep)
			for _, f := range refactor {
				author := f.LastAuthor
				if len(author) > 20 {
					author = author[:17] + "…"
				}
				fmt.Printf("%-50s %7d %7d %8.1f %s\n", truncPath(f.Path, 50), f.Commits, f.Authors, f.ChurnScore, author)
			}
		}

		if len(watch) > 0 {
			fmt.Println("\n\033[38;5;220mWatch List\033[0m  (high churn · actively evolving · multiple authors)")
			fmt.Println(header)
			fmt.Println(sep)
			for _, f := range watch {
				author := f.LastAuthor
				if len(author) > 20 {
					author = author[:17] + "…"
				}
				fmt.Printf("%-50s %7d %7d %8.1f %s\n", truncPath(f.Path, 50), f.Commits, f.Authors, f.ChurnScore, author)
			}
		}

		// If nothing fits either category (e.g. all are single commit), show raw list.
		if len(refactor) == 0 && len(watch) == 0 {
			fmt.Println(header)
			fmt.Println(sep)
			for _, f := range hotspots {
				days := fmt.Sprintf("%d", f.DaysSinceChange)
				if f.DaysSinceChange < 0 {
					days = "?"
				}
				fmt.Printf("%-50s %7d %7d %8.1f %6s\n", truncPath(f.Path, 50), f.Commits, f.Authors, f.ChurnScore, days)
			}
		}
		fmt.Println()
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

// ── intent intelligence commands ────────────────────────────────────────────

var intentCmd = &cobra.Command{
	Use:   "intent [path]",
	Short: "Show commit intent history for a file (feat/fix/refactor timeline)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		summary, err := intel.IntentFor(root, args[0])
		if err != nil {
			return err
		}
		if len(summary.Intents) == 0 {
			fmt.Printf("No commit history for %s\n", args[0])
			return nil
		}

		fmt.Printf("Intent history for %s:\n", args[0])
		fmt.Printf("  Features: %d  |  Fixes: %d  |  Refactors: %d  |  Tests: %d\n\n",
			summary.FeatureCount, summary.FixCount, summary.RefactorCount, summary.TestCount)

		fmt.Printf("%-8s %-10s %-12s %s\n", "HASH", "TYPE", "AUTHOR", "SUMMARY")
		fmt.Println(strings.Repeat("─", 80))
		for _, ci := range summary.Intents {
			refs := ""
			if len(ci.IssueRefs) > 0 {
				refs = " " + strings.Join(ci.IssueRefs, ", ")
			}
			fmt.Printf("%-8s %-10s %-12s %s%s\n", ci.Hash, ci.Type, truncPath(ci.Author, 12), ci.Summary, refs)
		}
		return nil
	},
}

var todosCmd = &cobra.Command{
	Use:   "todos",
	Short: "Find all TODO/FIXME/HACK/XXX comments in source code",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		items, err := intel.FindTodos(root)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No TODO/FIXME/HACK/XXX found. Clean codebase!")
			return nil
		}
		fmt.Printf("%-6s %-45s %5s %s\n", "TYPE", "FILE", "LINE", "COMMENT")
		fmt.Println(strings.Repeat("─", 100))
		for _, t := range items {
			fmt.Printf("%-6s %-45s %5d %s\n", t.Type, truncPath(t.File, 45), t.Line, t.Comment)
		}
		fmt.Printf("\nTotal: %d items\n", len(items))
		return nil
	},
}

var specGapsCmd = &cobra.Command{
	Use:   "spec-gaps",
	Short: "Find files where features were added but fixes keep piling up",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		gaps, err := intel.SpecGaps(root, 15)
		if err != nil {
			return err
		}
		if len(gaps) == 0 {
			fmt.Println("No spec gaps detected — features are stable.")
			return nil
		}
		fmt.Printf("%-50s %6s %6s %s\n", "FILE", "FEATS", "FIXES", "SIGNAL")
		fmt.Println(strings.Repeat("─", 80))
		for _, g := range gaps {
			signal := "⚠ unstable"
			if g.FixCount >= g.FeatureCount*2 {
				signal = "🔴 high risk"
			}
			fmt.Printf("%-50s %6d %6d %s\n", truncPath(g.Path, 50), g.FeatureCount, g.FixCount, signal)
		}
		return nil
	},
}

// ── workspace commands ───────────────────────────────────────────────────────

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Cross-repo workspace commands",
	Long:  "Manage and query across multiple repositories defined in mantis.workspace.yml.",
}

var wsInitCmd = &cobra.Command{
	Use:   "init [repo-paths...]",
	Short: "Create mantis.workspace.yml with the given repo paths",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		var repos []graph.RepoEntry
		for _, p := range args {
			alias := filepath.Base(p)
			repos = append(repos, graph.RepoEntry{Path: p, Alias: alias})
		}

		if err := graph.InitWorkspaceConfig(root, repos); err != nil {
			return err
		}
		fmt.Printf("✓ Created mantis.workspace.yml with %d repos\n", len(repos))
		fmt.Println("  Ensure each repo has been indexed with 'mantis init'.")
		return nil
	},
}

var wsFindCmd = &cobra.Command{
	Use:   "find <symbol>",
	Short: "Search for a symbol across all workspace repos",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		ws, err := graph.OpenWorkspace(root)
		if err != nil {
			return err
		}
		defer ws.Close()

		results, err := ws.FindAcrossRepos(args[0])
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Printf("No matches for %q across workspace\n", args[0])
			return nil
		}

		fmt.Printf("%-12s %-10s %-40s %s\n", "REPO", "TYPE", "FILE", "NAME")
		fmt.Println(strings.Repeat("─", 85))
		for _, r := range results {
			fmt.Printf("%-12s %-10s %-40s %s\n",
				r.Repo, string(r.Node.Type), truncPath(r.Node.FilePath, 40), r.Node.Name)
		}
		return nil
	},
}

var wsImpactCmd = &cobra.Command{
	Use:   "impact <symbol>",
	Short: "Trace impact of a symbol change across all repos",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		ws, err := graph.OpenWorkspace(root)
		if err != nil {
			return err
		}
		defer ws.Close()

		results, err := ws.ImpactAcrossRepos(args[0], 5)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Printf("No impact found for %q across workspace\n", args[0])
			return nil
		}

		fmt.Printf("Impact of changing %q:\n\n", args[0])
		fmt.Printf("%-12s %-8s %-10s %s\n", "REPO", "DEPTH", "RELATION", "FILE")
		fmt.Println(strings.Repeat("─", 80))
		for _, r := range results {
			fmt.Printf("%-12s %-8d %-10s %s\n",
				r.Repo, r.Depth, r.Relation, truncPath(r.Node.FilePath, 50))
		}
		return nil
	},
}

var wsStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show statistics for each repo in the workspace",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		ws, err := graph.OpenWorkspace(root)
		if err != nil {
			return err
		}
		defer ws.Close()

		stats := ws.GetStats()
		fmt.Printf("%-15s %8s %8s %8s\n", "REPO", "FILES", "SYMBOLS", "EDGES")
		fmt.Println(strings.Repeat("─", 45))
		for _, s := range stats {
			fmt.Printf("%-15s %8d %8d %8d\n", s.Repo, s.Files, s.Symbols, s.Edges)
		}
		fmt.Printf("\nRepos: %d\n", len(stats))
		return nil
	},
}

// ── trace commands ────────────────────────────────────────────────────────────

var traceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Runtime trace ingestion and analysis",
	Long:  "Ingest OpenTelemetry, pprof, or custom trace data to weight impact analysis by actual runtime behavior.",
}

var traceIngestCmd = &cobra.Command{
	Use:   "ingest <file>",
	Short: "Ingest trace data from OTLP JSON, pprof text, or custom JSON",
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

		entries, err := intel.IngestTraceFile(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("Parsed %d trace entries from %s\n", len(entries), filepath.Base(args[0]))

		matched, unmatched, err := intel.StoreTraces(db.Conn(), entries)
		if err != nil {
			return err
		}
		fmt.Printf("✓ Matched %d entries to graph nodes (%d unmatched)\n", matched, unmatched)
		return nil
	},
}

var traceHotpathsCmd = &cobra.Command{
	Use:   "hotpaths",
	Short: "Show hottest code paths by runtime call frequency",
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

		totalCalls, uniqueNodes, err := intel.TraceSummary(db.Conn())
		if err != nil {
			return err
		}
		if uniqueNodes == 0 {
			fmt.Println("No trace data. Run 'mantis trace ingest <file>' first.")
			return nil
		}
		fmt.Printf("Trace data: %d total calls across %d functions\n\n", totalCalls, uniqueNodes)

		stats, err := intel.Hotpaths(db.Conn(), 20)
		if err != nil {
			return err
		}

		fmt.Printf("%-8s %-10s %-30s %s\n", "CALLS", "AVG(ms)", "FUNCTION", "FILE")
		fmt.Println(strings.Repeat("─", 80))
		for _, s := range stats {
			fmt.Printf("%-8d %-10.2f %-30s %s\n",
				s.CallCount, s.AvgDuration, s.Name, truncPath(s.FilePath, 30))
		}
		return nil
	},
}

var traceColdCmd = &cobra.Command{
	Use:   "cold",
	Short: "Show structurally important but runtime-cold code",
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

		stats, err := intel.ColdPaths(db.Conn(), 20)
		if err != nil {
			return err
		}
		if len(stats) == 0 {
			fmt.Println("No cold paths found (all traced, or no trace data).")
			return nil
		}

		fmt.Printf("Structurally imported but never called at runtime:\n\n")
		fmt.Printf("%-30s %s\n", "FUNCTION", "FILE")
		fmt.Println(strings.Repeat("─", 70))
		for _, s := range stats {
			fmt.Printf("%-30s %s\n", s.Name, truncPath(s.FilePath, 40))
		}
		return nil
	},
}

var traceWeightCmd = &cobra.Command{
	Use:   "weight <symbol>",
	Short: "Runtime-weighted impact analysis for a symbol",
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
		nodes, err := q.FindNodeByName(args[0])
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			return fmt.Errorf("symbol %q not found in graph", args[0])
		}

		// Run BFS from first matching node.
		bfs, err := q.BFSReverse(nodes[0].ID, 5)
		if err != nil {
			return err
		}

		weighted, err := intel.WeightedImpact(db.Conn(), bfs)
		if err != nil {
			return err
		}
		if len(weighted) == 0 {
			fmt.Println("No impact data found.")
			return nil
		}

		fmt.Printf("Runtime-weighted impact of changing %q:\n\n", args[0])
		fmt.Printf("%-10s %-6s %-8s %-25s %s\n", "WEIGHT", "DEPTH", "CALLS", "FUNCTION", "FILE")
		fmt.Println(strings.Repeat("─", 85))
		for _, w := range weighted {
			fmt.Printf("%-10.1f %-6d %-8d %-25s %s\n",
				w.RuntimeWeight, w.StructDepth, w.CallCount,
				w.Name, truncPath(w.FilePath, 30))
		}
		return nil
	},
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

// ── MCP server ──────────────────────────────────────────────────────────────

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start MCP server over stdio (for Claude Code, Cursor, etc.)",
	Long: `Starts a Model Context Protocol (MCP) server over stdio.

Exposes Mantis graph intelligence as tools that any MCP-compatible
AI client can call. Requires 'mantis init' to have been run first.

Configure in your AI tool:
  {
    "mcpServers": {
      "mantis": { "command": "mantis", "args": ["mcp"] }
    }
  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return fmt.Errorf("graph not initialized — run 'mantis init' first: %w", err)
		}
		defer db.Close()

		srv := mcp.NewServer(db, root, version)
		return srv.Run()
	},
}

// ── LSP server ──────────────────────────────────────────────────────────────

var lspCmd = &cobra.Command{
	Use:   "lsp",
	Short: "Start LSP server over stdio (for VSCode, Neovim, etc.)",
	Long: `Starts a Language Server Protocol (LSP) server over stdio.

Augments standard language servers (gopls, tsserver) with Mantis
graph intelligence — hover enrichment, dead-code diagnostics,
hotspot indicators, and code lens for reference counts.

Requires 'mantis init' to have been run first.

Configure in VSCode settings:
  "mantis.binaryPath": "mantis"

Or use as a generic LSP server:
  mantis lsp`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		db, err := openDB(root)
		if err != nil {
			return fmt.Errorf("graph not initialized — run 'mantis init' first: %w", err)
		}
		defer db.Close()

		srv := lsp.NewServer(db, root, version)
		return srv.Run()
	},
}

// ── hooks install ─────────────────────────────────────────────────────────────

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Manage lifecycle hooks",
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install git hooks for Mantis integration",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		gitDir := filepath.Join(root, ".git", "hooks")
		if _, err := os.Stat(gitDir); os.IsNotExist(err) {
			return fmt.Errorf("not a git repository (no .git/hooks directory)")
		}

		// Install pre-commit hook that runs mantis lint.
		preCommit := filepath.Join(gitDir, "pre-commit")
		hookScript := `#!/bin/sh
# Mantis pre-commit hook — runs lint on staged files.
# Installed by 'mantis hooks install'. Edit or remove freely.
if command -v mantis >/dev/null 2>&1; then
    mantis lint --ci 2>/dev/null
fi
`
		if _, err := os.Stat(preCommit); err == nil {
			fmt.Printf("\033[38;5;244m  ⚠ %s already exists — skipping (use --force to overwrite)\033[0m\n", preCommit)
		} else {
			if err := os.WriteFile(preCommit, []byte(hookScript), 0o755); err != nil {
				return fmt.Errorf("write pre-commit: %w", err)
			}
			fmt.Printf("✓ Installed pre-commit hook  \033[38;5;244m(%s)\033[0m\n", preCommit)
		}

		// Ensure .mantisrc.yml exists with hooks section.
		rcPath := filepath.Join(root, ".mantisrc.yml")
		if _, err := os.Stat(rcPath); os.IsNotExist(err) {
			rcContent := `version: 1
hooks:
  # post_commit:
  #   - command: "echo 'committed'"
`
			if err := os.WriteFile(rcPath, []byte(rcContent), 0o644); err != nil {
				return fmt.Errorf("write .mantisrc.yml: %w", err)
			}
			fmt.Printf("✓ Created .mantisrc.yml  \033[38;5;244m(configure hooks here)\033[0m\n")
		}

		return nil
	},
}

// ── eval ──────────────────────────────────────────────────────────────────────

var evalCmd = &cobra.Command{
	Use:   "eval [prompt]",
	Short: "Evaluate code generation quality on a prompt",
	Long: `Runs the pipeline on a prompt and scores the output.
Measures: build success, test pass rate, lint violations, token efficiency.
Output: structured quality report (text or --json).

Flags:
  --offline   Run offline eval suite (Go tests) instead of online pipeline
  --compare   Compare current run against the last saved eval result`,
	Args: cobra.MinimumNArgs(0),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}

		// --compare implies --offline
		if evalCompare {
			evalOffline = true
		}

		// ── Offline eval mode ──
		if evalOffline {
			return runOfflineEval(root)
		}

		// ── Online eval mode (requires prompt) ──
		if len(args) == 0 {
			return fmt.Errorf("prompt required for online eval (use --offline for test-based eval)")
		}

		prompt := strings.Join(args, " ")
		fmt.Printf("╭─ eval ─╮\n")
		fmt.Printf("│ Prompt: %s\n", truncateEval(prompt, 60))
		fmt.Printf("╰────────╯\n\n")

		// Step 1: Baseline metrics.
		fmt.Printf("\033[38;5;244m⠋ collecting baseline...\033[0m\n")
		baselineLint := countLintIssues(root)
		baselineTests := countTestResults(root)

		// Step 2: Run pipeline.
		fmt.Printf("\033[38;5;244m⠋ running pipeline...\033[0m\n")
		// Use the pipeline via CLI to avoid import complexity.
		pipeOut, err := exec.Command(os.Args[0], prompt).CombinedOutput()
		pipeResult := string(pipeOut)
		buildOK := err == nil

		// Step 3: Post metrics.
		postLint := countLintIssues(root)
		postTests := countTestResults(root)

		// Step 4: Score.
		score := 0.0
		if buildOK {
			score += 4.0 // build passes = 4 points
		}
		if postTests.passed >= baselineTests.passed {
			score += 3.0 // no test regressions = 3 points
		}
		if postLint <= baselineLint {
			score += 2.0 // no new lint issues = 2 points
		}
		if len(pipeResult) > 100 {
			score += 1.0 // produced substantial output = 1 point
		}

		// Report.
		fmt.Printf("\n╭─ Quality Report ─╮\n")
		fmt.Printf("│  Build:    %s\n", boolIcon(buildOK))
		fmt.Printf("│  Tests:    %d/%d passed (was %d/%d)\n", postTests.passed, postTests.total, baselineTests.passed, baselineTests.total)
		fmt.Printf("│  Lint:     %d issues (was %d)\n", postLint, baselineLint)
		fmt.Printf("│  Score:    %.1f/10\n", score)
		fmt.Printf("╰──────────────────╯\n")

		return nil
	},
}

// ── eval history ─────────────────────────────────────────────────────────────

type evalHistoryEntry struct {
	Date   string  `json:"date"`
	Commit string  `json:"commit"`
	Mode   string  `json:"mode"`
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
	Pct    float64 `json:"pct"`
}

func evalHistoryPath(root string) string {
	return filepath.Join(root, ".mantis", "eval-history.json")
}

func loadEvalHistory(root string) ([]evalHistoryEntry, error) {
	data, err := os.ReadFile(evalHistoryPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var history []evalHistoryEntry
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, err
	}
	return history, nil
}

func saveEvalHistory(root string, history []evalHistoryEntry) error {
	dir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(evalHistoryPath(root), data, 0o644)
}

func currentCommitShort() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func runOfflineEval(root string) error {
	fmt.Printf("╭─ eval (offline) ─╮\n")
	fmt.Printf("│ Running offline eval suite...\n")
	fmt.Printf("╰──────────────────╯\n\n")

	// Load previous results for --compare.
	var previous *evalHistoryEntry
	if evalCompare {
		history, err := loadEvalHistory(root)
		if err != nil {
			fmt.Printf("\033[38;5;208mwarning: could not load eval history: %v\033[0m\n", err)
		} else if len(history) > 0 {
			previous = &history[len(history)-1]
		} else {
			fmt.Printf("\033[38;5;208mwarning: no previous eval run found for comparison\033[0m\n")
		}
	}

	// Run the offline eval test suite.
	goTestCmd := exec.Command("go", "test", "-run", "TestEval_OverallScorecard", "-v", "./internal/pipeline/...")
	goTestCmd.Dir = root
	out, testErr := goTestCmd.CombinedOutput()
	output := string(out)

	// Parse results from test output.
	passed, total := parseOfflineResults(output)
	allPassed := testErr == nil

	if total == 0 {
		// If no scorecard tests found, count pass/fail lines.
		passed, total = countPassFailLines(output)
	}

	var pct float64
	if total > 0 {
		pct = math.Round(float64(passed)/float64(total)*1000) / 10
	}

	// Display results.
	fmt.Printf("╭─ Offline Eval Report ─╮\n")
	fmt.Printf("│  Status:   %s\n", boolIcon(allPassed))
	fmt.Printf("│  Checks:   %d/%d passed\n", passed, total)
	fmt.Printf("│  Score:    %.1f%%\n", pct)
	fmt.Printf("╰────────────────────────╯\n")

	// Show comparison if requested.
	if evalCompare && previous != nil {
		deltaChecks := passed - previous.Passed
		deltaPct := pct - previous.Pct
		fmt.Printf("\n╭─ Comparison ─╮\n")
		fmt.Printf("│  Previous:  %d/%d (%.1f%%) on %s [%s]\n", previous.Passed, previous.Total, previous.Pct, previous.Date, previous.Commit)
		fmt.Printf("│  Current:   %d/%d (%.1f%%)\n", passed, total, pct)
		fmt.Printf("│  Delta:     %+d checks, %+.1f%%\n", deltaChecks, deltaPct)
		fmt.Printf("╰──────────────╯\n")
	}

	// Save to history.
	entry := evalHistoryEntry{
		Date:   time.Now().Format("2006-01-02"),
		Commit: currentCommitShort(),
		Mode:   "offline",
		Passed: passed,
		Total:  total,
		Pct:    pct,
	}
	history, _ := loadEvalHistory(root)
	history = append(history, entry)
	if err := saveEvalHistory(root, history); err != nil {
		fmt.Printf("\033[38;5;208mwarning: could not save eval history: %v\033[0m\n", err)
	} else {
		fmt.Printf("\nResults saved to %s\n", evalHistoryPath(root))
	}

	if !allPassed {
		return fmt.Errorf("offline eval: %d/%d checks failed", total-passed, total)
	}
	return nil
}

// parseOfflineResults extracts passed/total from scorecard-style test output.
// Looks for patterns like "95/95 checks passed" or "PASS: 42" / "FAIL: 3".
func parseOfflineResults(output string) (passed, total int) {
	// Pattern: "N/M checks passed" or "N/M passed"
	re := regexp.MustCompile(`(\d+)/(\d+)\s+(?:checks?\s+)?passed`)
	if m := re.FindStringSubmatch(output); len(m) == 3 {
		passed, _ = strconv.Atoi(m[1])
		total, _ = strconv.Atoi(m[2])
		return
	}
	return 0, 0
}

// countPassFailLines counts --- PASS and --- FAIL lines from go test -v output.
func countPassFailLines(output string) (passed, total int) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "--- PASS:") {
			passed++
			total++
		} else if strings.HasPrefix(line, "--- FAIL:") {
			total++
		}
	}
	return
}

type testCount struct {
	passed int
	total  int
}

func countLintIssues(root string) int {
	out, _ := exec.Command("go", "vet", "./...").CombinedOutput()
	if len(out) == 0 {
		return 0
	}
	return strings.Count(string(out), "\n")
}

func countTestResults(root string) testCount {
	out, _ := exec.Command("go", "test", "-count=1", "./...").CombinedOutput()
	output := string(out)
	passed := strings.Count(output, "ok ")
	failed := strings.Count(output, "FAIL")
	return testCount{passed: passed, total: passed + failed}
}

func boolIcon(b bool) string {
	if b {
		return "pass"
	}
	return "fail"
}

func truncateEval(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// ── tutorial ─────────────────────────────────────────────────────────────────

var tutorialCmd = &cobra.Command{
	Use:   "tutorial",
	Short: "Interactive guide to Mantis features",
	RunE: func(cmd *cobra.Command, args []string) error {
		gold := "\033[38;5;214m"
		dim := "\033[38;5;244m"
		cyan := "\033[38;5;81m"
		green := "\033[38;5;114m"
		bold := "\033[1m"
		reset := "\033[0m"

		fmt.Printf("%s╭─ Mantis Tutorial ──────────────────────────────────────────╮%s\n", gold, reset)
		fmt.Printf("%s│%s  Welcome to Mantis — your AI coding assistant.             %s│%s\n", gold, reset, gold, reset)
		fmt.Printf("%s│%s  This guide walks through the key features.                %s│%s\n", gold, reset, gold, reset)
		fmt.Printf("%s╰────────────────────────────────────────────────────────────╯%s\n\n", gold, reset)

		// 1. Getting Started
		fmt.Printf("%s%s① Getting Started%s\n", bold, cyan, reset)
		fmt.Printf("   Run %smantis init%s in your project root to index the codebase.\n", green, reset)
		fmt.Printf("   This creates a %s.mantis/%s directory with the dependency graph,\n", dim, reset)
		fmt.Printf("   embeddings, brain files, and ground truth signatures.\n\n")

		// 2. Interactive Chat
		fmt.Printf("%s%s② Interactive Chat%s\n", bold, cyan, reset)
		fmt.Printf("   Just run %smantis%s with no arguments to start a session.\n", green, reset)
		fmt.Printf("   Mantis auto-selects the best model tier for each message.\n")
		fmt.Printf("   %sFlags:%s --model fast|smart|heavy|vision  --plan  --continue\n\n", dim, reset)

		// 3. Slash Commands
		fmt.Printf("%s%s③ Slash Commands%s\n", bold, cyan, reset)
		fmt.Printf("   %s/file <path>%s    — inject a file into context\n", green, reset)
		fmt.Printf("   %s/vision <img>%s   — send an image for multimodal analysis\n", green, reset)
		fmt.Printf("   %s/brain%s          — view project memory (BRAIN.md)\n", green, reset)
		fmt.Printf("   %s/context%s        — show what files are in the context window\n", green, reset)
		fmt.Printf("   %s/reject <text>%s  — record a failed approach to avoid repeating\n", green, reset)
		fmt.Printf("   %s/decision <text>%s— log an architecture decision\n\n", green, reset)

		// 4. Code Generation Pipeline
		fmt.Printf("%s%s④ Code Generation Pipeline%s\n", bold, cyan, reset)
		fmt.Printf("   Prompts like %s\"build a REST API for users\"%s trigger the pipeline.\n", dim, reset)
		fmt.Printf("   The pipeline: plan → generate → build → test → fix (iterative).\n")
		fmt.Printf("   Uses diff-based edits for surgical changes to existing files.\n\n")

		// 5. Codebase Intelligence
		fmt.Printf("%s%s⑤ Codebase Intelligence%s\n", bold, cyan, reset)
		fmt.Printf("   %smantis hotspots%s  — files that change most often (refactor candidates)\n", green, reset)
		fmt.Printf("   %smantis dead%s      — detect unused symbols across the codebase\n", green, reset)
		fmt.Printf("   %smantis circular%s  — find circular dependency chains\n", green, reset)
		fmt.Printf("   %smantis impact%s    — blast radius analysis for a given file\n\n", green, reset)

		// 6. Graph Visualization
		fmt.Printf("%s%s⑥ Graph Visualization%s\n", bold, cyan, reset)
		fmt.Printf("   %smantis graph%s generates an interactive D3 dependency graph.\n", green, reset)
		fmt.Printf("   Open the output HTML in a browser to explore your architecture.\n")
		fmt.Printf("   %sFlags:%s --module <path>  --depth N  --out <file>\n\n", dim, reset)

		// 7. Quality Evaluation
		fmt.Printf("%s%s⑦ Quality Evaluation%s\n", bold, cyan, reset)
		fmt.Printf("   %smantis eval \"<prompt>\"%s scores code generation quality.\n", green, reset)
		fmt.Printf("   Measures: build success, test pass rate, lint violations.\n")
		fmt.Printf("   Outputs a 0–10 score with a structured report.\n\n")

		// 8. Project Templates
		fmt.Printf("%s%s⑧ Project Templates%s\n", bold, cyan, reset)
		fmt.Printf("   %smantis new <template> [name]%s scaffolds a new project.\n", green, reset)
		fmt.Printf("   Templates: go-cli, go-api, node-cli, python-pkg\n\n")

		fmt.Printf("%s╭─ Tip ─────────────────────────────────────────────────────╮%s\n", gold, reset)
		fmt.Printf("%s│%s  Run %smantis --help%s for the full command reference.          %s│%s\n", gold, reset, green, reset, gold, reset)
		fmt.Printf("%s╰───────────────────────────────────────────────────────────╯%s\n", gold, reset)

		return nil
	},
}

// ── new (project templates) ──────────────────────────────────────────────────

var newCmd = &cobra.Command{
	Use:   "new <template> [project-name]",
	Short: "Create a new project from a template",
	Long: `Available templates:
  go-cli      — Go CLI app with Cobra
  go-api      — Go REST API with net/http
  node-cli    — TypeScript CLI
  python-pkg  — Python package with pyproject.toml`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		template := args[0]
		name := template
		if len(args) > 1 {
			name = args[1]
		}

		templates := map[string]map[string]string{
			"go-cli": {
				"go.mod": "module " + name + "\n\ngo 1.21\n",
				"main.go": `package main

import "fmt"

func main() {
	fmt.Println("` + name + ` — built with Mantis")
}
`,
				"Makefile": `build:
	go build -o ./bin/` + name + ` .

test:
	go test ./...

lint:
	go vet ./...
`,
			},
			"go-api": {
				"go.mod": "module " + name + "\n\ngo 1.21\n",
				"main.go": `package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
`,
				"Makefile": `build:
	go build -o ./bin/` + name + ` .

run:
	go run .

test:
	go test ./...
`,
			},
			"node-cli": {
				"package.json": `{
  "name": "` + name + `",
  "version": "0.1.0",
  "type": "module",
  "main": "dist/index.js",
  "scripts": {
    "build": "tsc",
    "start": "node dist/index.js",
    "test": "vitest"
  }
}
`,
				"tsconfig.json": `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "node",
    "outDir": "dist",
    "strict": true
  },
  "include": ["src"]
}
`,
				"src/index.ts": `console.log("` + name + ` — built with Mantis");
`,
			},
			"python-pkg": {
				"pyproject.toml": `[project]
name = "` + name + `"
version = "0.1.0"
requires-python = ">=3.10"

[build-system]
requires = ["setuptools"]
build-backend = "setuptools.backends._legacy:_Backend"
`,
				"src/" + name + "/__init__.py": `"""` + name + ` — built with Mantis."""

__version__ = "0.1.0"
`,
				"tests/test_main.py": `def test_import():
    import ` + name + `
    assert ` + name + `.__version__ == "0.1.0"
`,
			},
		}

		tmpl, ok := templates[template]
		if !ok {
			fmt.Printf("Unknown template %q. Available:\n", template)
			for k := range templates {
				fmt.Printf("  %s\n", k)
			}
			return fmt.Errorf("unknown template: %s", template)
		}

		// Create project directory.
		if err := os.MkdirAll(name, 0o755); err != nil {
			return err
		}

		for path, content := range tmpl {
			full := filepath.Join(name, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return err
			}
			fmt.Printf("  ✓ %s\n", path)
		}

		fmt.Printf("\n✓ Created %s from %s template\n", name, template)
		fmt.Printf("  cd %s && mantis init\n", name)
		return nil
	},
}

// ── main ──────────────────────────────────────────────────────────────────────

func init() {
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
	rootCmd.Flags().BoolVar(&replPlan, "plan", false, "Plan mode: review plan before code execution")
	rootCmd.Flags().BoolVar(&replContinue, "continue", false, "Resume most recent session")
	rootCmd.Flags().BoolVar(&replOffline, "offline", false, "Skip GitHub auth gate — for local-only use without internet")
	rootCmd.Flags().StringArrayVar(&replAPIKeys, "api-key", nil, "Ollama API key (can be specified multiple times for rotation)")

	evalCmd.Flags().BoolVar(&evalOffline, "offline", false, "Run offline eval suite (no model needed)")
	evalCmd.Flags().BoolVar(&evalCompare, "compare", false, "Compare against last eval run")

	rootCmd.AddCommand(initCmd, contextCmd, watchCmd, findCmd, impactCmd, deadCmd, circularCmd, graphCmd, lintCmd, tuiCmd, handoffCmd, hotspotsCmd, riskyCmd, couplingCmd, intentCmd, todosCmd, specGapsCmd, workspaceCmd, traceCmd, mcpCmd, lspCmd, hooksCmd, evalCmd, newCmd, tutorialCmd)
	hooksCmd.AddCommand(hooksInstallCmd)

	workspaceCmd.AddCommand(wsInitCmd, wsFindCmd, wsImpactCmd, wsStatsCmd)
	traceCmd.AddCommand(traceIngestCmd, traceHotpathsCmd, traceColdCmd, traceWeightCmd)

	hotspotsCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
	riskyCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
	couplingCmd.Flags().IntVar(&temporalDays, "days", 90, "Look-back period in days")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
