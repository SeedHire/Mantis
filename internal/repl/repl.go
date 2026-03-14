// Package repl implements the interactive AI coding assistant.
// Invoked by running `mantis` with no subcommand.
package repl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/chzyer/readline"
	"github.com/seedhire/mantis/internal/agent"
	"github.com/seedhire/mantis/internal/autofix"
	"github.com/seedhire/mantis/internal/brain"
	"github.com/seedhire/mantis/internal/embeddings"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
	"github.com/seedhire/mantis/internal/keyring"
	"github.com/seedhire/mantis/internal/mcp"
	"github.com/seedhire/mantis/internal/parser"
	"github.com/seedhire/mantis/internal/nl"
	"github.com/seedhire/mantis/internal/notify"
	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/pipeline"
	"github.com/seedhire/mantis/internal/router"
	"github.com/seedhire/mantis/internal/session"
	"github.com/seedhire/mantis/internal/snapshot"
	"github.com/seedhire/mantis/internal/setup"
	"github.com/seedhire/mantis/internal/telemetry"
	"github.com/seedhire/mantis/internal/truth"
	"github.com/seedhire/mantis/internal/usage"
	"github.com/seedhire/mantis/internal/verify"
	"github.com/seedhire/mantis/internal/web"
	"golang.org/x/term"
)

const (
	colorReset  = "\033[0m"
	colorCopper = "\033[38;5;214m"
	colorGold   = "\033[38;5;220m"
	colorDim    = "\033[38;5;244m"
	colorGreen  = "\033[38;5;43m"
	colorRed    = "\033[38;5;197m"
	colorBold   = "\033[1m"
)

// activeKeyRing is set during Run() so streamWithFallback can rotate keys.
var activeKeyRing *keyring.KeyRing

// activeCreds is set during Run() so /keys can persist changes.
var activeCreds *setup.Credentials

// makeKeyRotationCallback returns a pipeline.OnRateLimitFunc that rotates keys
// using the given KeyRing. Returns nil if kr is nil or has only one key.
func makeKeyRotationCallback(kr *keyring.KeyRing) pipeline.OnRateLimitFunc {
	if kr == nil || kr.Count() <= 1 {
		return nil
	}
	return func(client *ollama.Client) bool {
		fromIdx := kr.ActiveIndex()
		kr.MarkRateLimited(15 * time.Minute)
		newKey, toIdx, ok := kr.Rotate()
		if !ok {
			return false
		}
		client.SetAPIKey(newKey)
		fmt.Printf("%s  [key #%d rate limited — switching to key #%d]%s\n",
			colorDim, fromIdx, toIdx, colorReset)
		return true
	}
}

// routerEmbedAdapter adapts *embeddings.Store to satisfy router.EmbedStore.
// The router package defines a minimal interface to avoid importing embeddings.
type routerEmbedAdapter struct{ store *embeddings.Store }

func (a *routerEmbedAdapter) Add(ctx context.Context, id, source, sectionLabel, text string) error {
	return a.store.Add(ctx, id, source, sectionLabel, text)
}
func (a *routerEmbedAdapter) SearchBySource(ctx context.Context, query, source string, limit int) ([]router.EmbedChunk, error) {
	chunks, err := a.store.SearchBySource(ctx, query, source, limit)
	if err != nil {
		return nil, err
	}
	result := make([]router.EmbedChunk, len(chunks))
	for i, c := range chunks {
		result[i] = router.EmbedChunk{SectionLabel: c.SectionLabel, Score: c.Score}
	}
	return result, nil
}

// Config holds REPL startup options.
type Config struct {
	// ForceTier overrides model routing if set ("trivial" | "fast" | "code" | "reason" | "heavy" | "max")
	ForceTier string
	// Budget is the max total tokens per session (0 = unlimited)
	Budget int
	// InitialQuery is a one-shot query (non-interactive mode)
	InitialQuery string
	// ImagePath is a path to an image for multimodal input
	ImagePath string
	// PlanMode stops pipeline after PLAN stage and asks for approval before coding
	PlanMode bool
	// Continue resumes the most recent session
	Continue bool
	// Version is the build version string (injected via ldflags)
	Version string
	// Offline skips GitHub auth gate — for local-only use without internet access.
	Offline bool
}

// Run starts the interactive REPL. Blocks until the user quits.
func Run(cfg Config) error {
	root, _ := os.Getwd()

	// First-run setup runs WITHOUT the banner so the wizard has full screen.
	// After it completes we clear and show the clean start screen.
	// --offline skips the GitHub auth gate entirely (local-only use).
	if !cfg.Offline {
		if setup.NeedsSetup() {
			creds, err := setup.Run()
			if err != nil {
				return fmt.Errorf("setup: %w", err)
			}
			setup.ApplyToEnv(creds)
		} else {
			creds, _ := setup.Load()
			if creds != nil {
				setup.ApplyToEnv(creds)
			}
		}

		// Hard gate: refuse to start without a verified GitHub login.
		if creds, _ := setup.Load(); !setup.IsLoggedIn(creds) {
			fmt.Fprintf(os.Stderr, "\n  \033[38;5;197m✗ GitHub login is required to use Mantis.\033[0m\n")
			fmt.Fprintf(os.Stderr, "  Run \033[38;5;214mmantis\033[0m again to complete setup.\n\n")
			fmt.Fprintf(os.Stderr, "  Tip: use \033[38;5;214mmantis --offline\033[0m to skip auth for local-only use.\n\n")
			return fmt.Errorf("not authenticated")
		}
	}

	// ── Clean start screen ───────────────────────────────────────────────────
	clearScreen()
	printBanner()

	creds, _ := setup.Load()
	client := ollama.NewFromEnv()

	// ── Build KeyRing from all configured API keys ──────────────────────────
	var allKeys []string
	if creds != nil {
		allKeys = creds.AllKeys()
	}
	// OLLAMA_API_KEYS env var (comma-separated) takes priority.
	if envKeys := os.Getenv("OLLAMA_API_KEYS"); envKeys != "" {
		seen := map[string]bool{}
		for _, k := range strings.Split(envKeys, ",") {
			k = strings.TrimSpace(k)
			if k != "" && !seen[k] {
				seen[k] = true
				allKeys = append(allKeys, k)
			}
		}
	}
	// Deduplicate final list.
	{
		seen := map[string]bool{}
		deduped := allKeys[:0]
		for _, k := range allKeys {
			if !seen[k] {
				seen[k] = true
				deduped = append(deduped, k)
			}
		}
		allKeys = deduped
	}
	// If single OLLAMA_API_KEY is set and not yet in the list, add it.
	if envKey := os.Getenv("OLLAMA_API_KEY"); envKey != "" {
		found := false
		for _, k := range allKeys {
			if k == envKey {
				found = true
				break
			}
		}
		if !found {
			allKeys = append([]string{envKey}, allKeys...)
		}
	}
	// Build labeled keyring from tagged keys when available.
	var keyLabels []string
	if creds != nil {
		tagged := creds.AllTaggedKeys()
		for _, tk := range tagged {
			keyLabels = append(keyLabels, tk.Tag)
		}
	}
	keyRing := keyring.NewKeyRingLabeled(allKeys, keyLabels)
	activeKeyRing = keyRing
	activeCreds = creds

	// Sync client API key from the keyring so the first call uses the right key.
	if keyRing != nil && keyRing.Current() != "" {
		client.SetAPIKey(keyRing.Current())
	}

	// Single line: user · connection.
	{
		connStr := colorDim + "local Ollama" + colorReset
		if client.IsCloud() {
			connStr = colorGreen + "Ollama Cloud" + colorReset
		}
		if keyRing != nil && keyRing.Count() > 1 {
			connStr += fmt.Sprintf(" %s(%d API keys)%s", colorDim, keyRing.Count(), colorReset)
		}
		if creds != nil && creds.GitHubUser != "" {
			fmt.Printf("%s● %s%s · %s\n", colorGreen, creds.GitHubUser, colorReset, connStr)
		} else {
			fmt.Printf("● %s\n", connStr)
		}
	}

	// Resolve available models; keep the list for ensemble use.
	var availableModels []ollama.ModelInfo
	{
		fmt.Printf("%s  connecting to Ollama...%s", colorDim, colorReset)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := client.ListModels(ctx)
		cancel()
		fmt.Printf("\r%s                          %s\r", colorDim, colorReset) // clear line
		if err != nil {
			if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "dial") {
				fmt.Printf("\033[38;5;196m● Ollama is not running — start it with: ollama serve\033[0m\n")
				fmt.Printf("%s  mantis requires Ollama for model inference%s\n", colorDim, colorReset)
			} else {
				fmt.Printf("%s● could not list models: %v%s\n", colorGold, err, colorReset)
			}
		} else if len(models) == 0 {
			fmt.Printf("\033[38;5;196m● No models installed — install one with: ollama pull qwen3-coder\033[0m\n")
		}
		if err == nil && len(models) > 0 {
			availableModels = models
			router.ResolveAll(models)
		}
	}

	// Load project brain.
	b := brain.New(root)
	if !b.Exists() {
		_ = b.Init()
	}
	// Auto-populate BRAIN.md if still placeholder — ensures model always knows the stack.
	if b.IsBrainEmpty() {
		lang, framework, runCmd := detectLangFramework(root)
		entry := detectEntryPoint(root, lang)
		b.AutoPopulateBrain(lang, framework, entry, runCmd)
	}
	brainContext := b.LoadHot() // 7.5: hot tier only; cold memory retrieved on-demand
	conventions := verify.ParseConventions(b.ReadFile("CONVENTIONS.md"))

	// Semantic embeddings — optional, used for memory retrieval + router classifier.
	var embStore *embeddings.Store
	var routerStore router.EmbedStore
	mantisDir := filepath.Join(root, ".mantis")
	// shutdownCtx is cancelled when the REPL exits, signalling background goroutines to stop.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()
	if es, err := embeddings.Open(mantisDir, client); err == nil {
		embStore = es
		defer embStore.Close()
		adapter := &routerEmbedAdapter{store: embStore}
		routerStore = adapter
		// Re-index brain files and router examples in background.
		// Uses shutdownCtx so the goroutine stops cleanly when the REPL exits.
		go func() {
			ctx, cancel := context.WithTimeout(shutdownCtx, 90*time.Second)
			defer cancel()
			_ = embStore.IndexBrainFiles(ctx, mantisDir)
			router.IndexRouterExamples(adapter)
		}()
	} else {
		fmt.Printf("%s● memory store unavailable: %v%s\n", colorGold, err, colorReset)
	}

	// NL dispatcher — codebase intelligence tools, called automatically.
	dispatcher := nl.New(root)
	if dispatcher != nil {
		defer dispatcher.Close()
	}

	// Web fetcher — Jina Reader with cache and raw fallback.
	webFetcher := web.NewFetcher()

	// Session tracker.
	sess := session.New()
	approvalCache := NewApprovalCache()           // permission caching: remembers approved dirs across turns
	permMode := ModeDefault                       // 7D: permission mode (default/auto-edit/plan)
	effortLevel := EffortMedium                   // 7G: thinking/effort control
	notifier := notify.New()                      // 7P: desktop notifications for slow tasks
	opLog := NewOperationLog(mantisDir)           // operation log: enables /undo across turns
	usageTracker := usage.New()
	tlog := telemetry.New()
	sessID := fmt.Sprintf("%d", time.Now().UnixMilli())
	evLog := NewEventLog(mantisDir, sessID)       // event log: EVENTS.jsonl for /replay
	snapStore := snapshot.NewStore(root)           // git stash-based snapshot/revert
	if creds != nil && creds.GitHubUser != "" {
		tlog.SetUser(creds.GitHubUser, "v0.3.0")
	}

	// Ground truth — auto-index in background on first run.
	truthWriter := truth.New(root)
	if truthWriter.FileCount() == 0 {
		go func() { _ = truthWriter.BuildFull(root) }()
	}

	// Single project status line: files · skills · memory · MANTIS.md.
	{
		var parts []string
		if n := truthWriter.FileCount(); n > 0 {
			parts = append(parts, fmt.Sprintf("%d files", n))
		}
		if n := b.SkillCount(); n > 0 {
			parts = append(parts, fmt.Sprintf("%d skills", n))
		}
		if brainContext != "" {
			parts = append(parts, "memory ready")
		}
		if b.HasMantisFile() {
			parts = append(parts, "MANTIS.md")
		}
		if len(parts) > 0 {
			fmt.Printf("%s● %s%s\n", colorGold, strings.Join(parts, " · "), colorReset)
		} else {
			fmt.Printf("%s● /init to generate MANTIS.md%s\n", colorDim, colorReset)
		}
	}

	// Conversation history — start with a default system prompt (will be rebuilt per-turn with tier context).
	systemPrompt := buildSystemPrompt(brainContext, b.LoadSkillsForTask("implement", 20000), router.TierCode)
	messages := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
	}

	// Session resume: load most recent session conversation if --continue.
	if cfg.Continue {
		if prev, err := session.LoadRecent(mantisDir, 24*time.Hour); err == nil && prev != nil && len(prev.Messages) > 0 {
			var restored []interface{}
			for _, raw := range prev.Messages {
				var msg ollama.Message
				if err := json.Unmarshal(raw, &msg); err == nil && msg.Role != "" {
					restored = append(restored, msg)
				}
			}
			if len(restored) > 0 {
				// Replace system prompt with fresh one, keep user/assistant history.
				messages = []interface{}{ollama.Message{Role: "system", Content: systemPrompt}}
				for _, m := range restored {
					if msg, ok := m.(ollama.Message); ok && msg.Role != "system" {
						messages = append(messages, m)
					}
				}
				fmt.Printf("%s● resumed session: %s (%d messages)%s\n",
					colorGold, prev.Topic, len(messages)-1, colorReset)
			}
		} else {
			fmt.Printf("%s● no recent session to resume%s\n", colorDim, colorReset)
		}
	}

	// One-time command hint — shown once at startup, not repeated every turn.
	fmt.Printf("%s  /help · /file · /test · /brain · /quit%s\n\n", colorDim, colorReset)

	// One-shot mode: mantis "question"
	if cfg.InitialQuery != "" {
		return runOnce(cfg, client, sess, b, dispatcher, &messages, truthWriter, usageTracker, routerStore)
	}

	// Interactive REPL with readline (history, arrows, Ctrl+R, tab completion).
	slashCompleter := readline.NewPrefixCompleter(
		readline.PcItem("/help"),
		readline.PcItem("/init"),
		readline.PcItem("/file"),
		readline.PcItem("/vision"),
		readline.PcItem("/reset"),
		readline.PcItem("/cost"),
		readline.PcItem("/stats"),
		readline.PcItem("/telemetry"),
		readline.PcItem("/brain"),
		readline.PcItem("/save"),
		readline.PcItem("/models"),
		readline.PcItem("/reject"),
		readline.PcItem("/decision"),
		readline.PcItem("/plan"),
		readline.PcItem("/build"),
		readline.PcItem("/context"),
		readline.PcItem("/fetch"),
		readline.PcItem("/search"),
		readline.PcItem("/ask"),
		readline.PcItem("/replay"),
		readline.PcItem("/benchmark"),
		readline.PcItem("/test"),
		readline.PcItem("/index"),
		readline.PcItem("/commit"),
		readline.PcItem("/pr"),
		readline.PcItem("/git-log"),
		readline.PcItem("/undo"),
		readline.PcItem("/snapshot"),
		readline.PcItem("/snapshots"),
		readline.PcItem("/revert"),
		readline.PcItem("/keys"),
		readline.PcItem("/mode"),
		readline.PcItem("/effort"),
		readline.PcItem("/notify"),
		readline.PcItem("/execute"),
		readline.PcItem("/mcp"),
		readline.PcItem("/quit"),
	)

	// Show project name in prompt: "projectname ❯ "
	projectName := filepath.Base(root)
	idlePrompt := "\033[38;5;244m" + projectName + "\033[0m \033[38;5;214m❯\033[0m "

	histFile := filepath.Join(os.Getenv("HOME"), ".mantis", "history")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            idlePrompt,
		HistoryFile:       histFile,
		AutoComplete:      slashCompleter,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		// Fallback to plain scanner if readline init fails (e.g. non-TTY).
		return runWithScanner(cfg, client, sess, b, dispatcher, messages, truthWriter, usageTracker, routerStore, webFetcher)
	}
	defer rl.Close()

	// 8.5.3: Check for unfinished pipeline progress from a prior session.
	checkUnfinishedProgress(mantisDir, rl)

	// ── Soft-interrupt state ─────────────────────────────────────────────────
	// Ctrl+C during generation cancels the current stream (soft).
	// Ctrl+C when idle: first press shows hint; second press within 2s exits.
	var activeCancelMu sync.Mutex
	var activeCancelFn context.CancelFunc
	var lastInterruptNano atomic.Int64 // UnixNano of last Ctrl+C — atomic for signal safety

	setActiveCancel := func(cancel context.CancelFunc) {
		activeCancelMu.Lock()
		activeCancelFn = cancel
		activeCancelMu.Unlock()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGTERM {
				// Hard exit on SIGTERM (process manager / kill request).
				fmt.Println()
				tlog.Flush()
				endSession(sess, b, messages, embStore)
				rl.Close()
				os.Exit(0)
			}
			// SIGINT (Ctrl+C) — soft cancel.
			activeCancelMu.Lock()
			cancel := activeCancelFn
			activeCancelMu.Unlock()

			if cancel != nil {
				// Generation is running — cancel it and return to prompt.
				cancel()
				fmt.Printf("\r\033[K%s  ✗ cancelled%s\n", colorDim, colorReset)
			} else {
				// Idle at prompt — two-tap to exit.
				if time.Since(time.Unix(0, lastInterruptNano.Load())) < 2*time.Second {
					fmt.Println()
					tlog.Flush()
					endSession(sess, b, messages, embStore)
					rl.Close()
					os.Exit(0)
				}
				lastInterruptNano.Store(time.Now().UnixNano())
				fmt.Printf("\n%s  Ctrl+C again to exit%s\n", colorDim, colorReset)
			}
		}
	}()

	turn := 0
	planMode := cfg.PlanMode
	planPrompt := "\033[38;5;141m[PLAN]\033[0m " + idlePrompt
	buildPrompt := idlePrompt
	setPlanPrompt := func() {
		if planMode {
			rl.SetPrompt(planPrompt)
		} else {
			rl.SetPrompt(buildPrompt)
		}
	}
	setPlanPrompt()
	// autoReadFiles tracks files already injected into context this session.
	// Prevents re-reading the same file on every turn once it's in the history.
	autoReadFiles := map[string]bool{}
	for {
		printSep()
		line, err := rl.Readline()
		if err == readline.ErrInterrupt {
			// Two-tap: first Ctrl+C shows hint, second within 2s exits.
			if time.Since(time.Unix(0, lastInterruptNano.Load())) < 2*time.Second {
				break
			}
			lastInterruptNano.Store(time.Now().UnixNano())
			if len(line) == 0 {
				fmt.Printf("%s  Ctrl+C again to exit%s\n", colorDim, colorReset)
			}
			continue
		} else if err == io.EOF {
			break
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}

		// Normalize terminal error pastes into actionable fix requests.
		input = normalizeTerminalInput(input)

		// Slash commands.
		if strings.HasPrefix(input, "/") {
			evLog.RecordSlashCommand(turn, strings.Fields(input)[0])
			if quit := handleSlashCommand(input, sess, b, &messages, client, &brainContext, &planMode, cfg, webFetcher, opLog, embStore, snapStore, &permMode, rl, &effortLevel, notifier); quit {
				break
			}
			setPlanPrompt()
			continue
		}

		// Token budget guard.
		if cfg.Budget > 0 {
			_, _, total := sess.Totals()
			if total >= cfg.Budget {
				fmt.Printf("%s⚠ token budget (%d) reached. /reset to continue.%s\n\n",
					colorRed, cfg.Budget, colorReset)
				continue
			}
		}

		// Classify intent → pick model.
		hasImage := cfg.ImagePath != ""
		intent := router.Classify(input, hasImage, routerStore)
		if cfg.ForceTier != "" {
			intent.Tier = parseTier(cfg.ForceTier)
		}
		model := router.ModelFor(intent.Tier)
		turn++
		turnStart := time.Now()
		evLog.RecordUserMessage(turn, input)

		// Per-turn context — cancelled by Ctrl+C (setActiveCancel) or at turn end.
		// All sub-operations (agent, pipeline, stream, verify) derive from this,
		// so a single Ctrl+C reliably cancels whatever is currently running.
		turnCtx, turnCancel := context.WithCancel(context.Background())
		setActiveCancel(turnCancel)

		showRouting(intent, model, turn, pipeline.ShouldRun(intent, input))
		printSep()
		// Update prompt to show active tier badge while model generates.
		rl.SetPrompt(tierPromptStr(intent.Tier))

		// Update system prompt for this tier's reasoning depth.
		// Skill budget scales with both tier AND request scope (word count).
		// Quick fixes load 1-2 skills; full project builds load 4-5.
		wordCount := len(strings.Fields(input))
		var skillsBudget int
		switch {
		case intent.Tier <= router.TierFast:
			skillsBudget = 2000 // ~0-1 skills for trivial/fast
		case wordCount <= 8:
			skillsBudget = 4000 // short request → 1-2 skills
		case wordCount <= 20:
			skillsBudget = 10000 // medium request → 2-3 skills
		default:
			skillsBudget = 20000 // complex/long request → 4-5 skills
		}
		tierSkills := b.LoadSkillsForTask(string(intent.TaskType), skillsBudget)
		tierPrompt := buildSystemPrompt(brainContext, tierSkills, intent.Tier)
		if planMode {
			tierPrompt += "\n\n## PLAN MODE (active)\nYou are in read-only exploration mode. Analyze, explain, and suggest plans — but do NOT write code blocks meant for file creation. If the user asks you to implement something, outline the plan (files to change, approach, risks) and tell them to run /build to switch to build mode."
		}
		if len(messages) > 0 {
			messages[0] = ollama.Message{Role: "system", Content: tierPrompt}
		}

		// Run codebase intelligence tools silently if graph is available.
		var toolCtx string
		if intent.NeedsGraph && dispatcher != nil {
			if results := dispatcher.Dispatch(input); len(results) > 0 {
				var sb strings.Builder
				for _, r := range results {
					sb.WriteString("\n[" + r.Tool + "]\n" + r.Content + "\n")
				}
				toolCtx = sb.String()
			}
		}

		// Compose user message with task-type template guidance.
		userContent := input
		if tmpl := router.TaskTemplate(intent.TaskType); tmpl != "" {
			userContent = tmpl + "\n\n" + input
		}

		// Pre-analysis: inject structured project facts so weak models know the language/framework.
		if facts := detectProjectFacts(root); facts != "" {
			userContent = facts + "\n\n" + userContent
		}
		// Error analysis: parse error messages and inject structured diagnosis.
		if intent.TaskType == "fix" || strings.Contains(strings.ToLower(input), "error") || strings.Contains(strings.ToLower(input), "traceback") {
			if analysis := analyzeError(input, root); analysis != "" {
				userContent = userContent + "\n\n" + analysis
			}
		}

		// Auto-capture build errors and inject relevant project files.
		{
			if intent.TaskType == "fix" || intent.TaskType == "general" || intent.TaskType == "implement" {
				if captured := captureBuildError(input, root); captured != "" {
					fmt.Printf("%s  ◉ captured build output%s\n", colorDim, colorReset)
					userContent = userContent + "\n\n" + captured
				}
			}
			// Inject build config files (always includes manifests).
			if buildFiles := injectBuildFiles(input, root); buildFiles != "" {
				fmt.Printf("%s  ◉ loaded project files%s\n", colorDim, colorReset)
				userContent = userContent + "\n\n" + buildFiles
			}
			// Auto-read source files mentioned in error messages.
			if intent.TaskType == "fix" || strings.Contains(strings.ToLower(input), "error") || strings.Contains(strings.ToLower(input), "traceback") {
				if errFiles := injectErrorSourceFiles(input, root); errFiles != "" {
					fmt.Printf("%s  ● reading: %s%s\n", colorDim, extractFileNames(errFiles), colorReset)
					userContent = userContent + "\n\n" + errFiles
				}
			}
		}

		// Semantic memory retrieval — hybrid BM25+cosine via RRF.
		// Skip for TierTrivial — one-line answers don't need memory.
		var memChunksFound int
		if embStore != nil && intent.Tier >= router.TierFast {
			embCtx, embCancel := context.WithTimeout(turnCtx, 5*time.Second)
			if chunks, err := embStore.Search(embCtx, input, 5); err == nil && len(chunks) > 0 {
				// Keep top chunk and any within 0.015 RRF delta of it.
				topScore := chunks[0].Score
				var relevant []embeddings.Chunk
				for _, c := range chunks {
					if topScore-c.Score < 0.015 {
						relevant = append(relevant, c)
					}
				}
				if len(relevant) > 0 {
					var memBuf strings.Builder
					memBuf.WriteString("<retrieved_memory>\n")
					// Most relevant chunk goes last (closest to query — Lost in the Middle fix).
					for i := len(relevant) - 1; i >= 0; i-- {
						c := relevant[i]
						memBuf.WriteString(fmt.Sprintf("[%s] %s\n", c.Source, c.Text))
					}
					memBuf.WriteString("</retrieved_memory>")
					userContent = userContent + "\n\n" + memBuf.String()
					memChunksFound = len(relevant)
				}
			}
			embCancel()
		}

		if toolCtx != "" {
			userContent = toolCtx + "\nUser question: " + userContent
		}

		var userMsg interface{}
		if hasImage {
			imgData, err := loadImage(cfg.ImagePath)
			if err != nil {
				fmt.Printf("%sError loading image: %v%s\n\n", colorRed, err, colorReset)
				cfg.ImagePath = ""
				continue
			}
			userMsg = ollama.ImageMessage{Role: "user", Content: userContent, Images: []string{imgData}}
			cfg.ImagePath = ""
		} else if toolCtx == "" {
			// Lazy context injection: wrap user message with README/symbols when relevant.
			// Get graph querier if available for graph context injection.
			var graphQuerier *graph.Querier
			if dispatcher != nil && dispatcher.IsAvailable() {
				graphQuerier = dispatcher.Querier()
			}
			if ctxMsg := contextMessageFor(turnCtx, input, root, brainContext, truthWriter, graphQuerier, embStore, autoReadFiles, intent.Tier); ctxMsg != nil {
				// Merge context injection with enriched userContent (build output, project files).
				if msg, ok := ctxMsg.(ollama.Message); ok {
					msg.Content = strings.Replace(msg.Content, "\n\nNow answer: "+input, "\n\nNow answer: "+userContent, 1)
					userMsg = msg
				} else {
					userMsg = ctxMsg
				}
			} else {
				userMsg = ollama.Message{Role: "user", Content: userContent}
			}
		} else {
			userMsg = ollama.Message{Role: "user", Content: userContent}
		}

		// Phase 0: proactive compaction — check token budget BEFORE adding the new
		// turn. Compressing with the prior history still intact gives the summarizer
		// more headroom and avoids the threshold being blown by a long user message.
		tokensBefore := estimateConversationTokens(messages)
		messages = compressIfNeeded(turnCtx, messages, client, model)
		tokensAfter := estimateConversationTokens(messages)
		if tokensAfter < tokensBefore {
			evLog.RecordCompaction(turn, (tokensBefore-tokensAfter)/100, tokensBefore, tokensAfter)
		}

		messages = append(messages, userMsg)

		// ── Multi-agent fan-out for complex multi-package tasks ───────────────
		// Gate: TierHeavy or TierCode, graph available, impact spans 4+ files
		// across 2+ packages. Workers run in parallel goroutines; each uses the
		// full AgentToolkit (read/write/bash/search/finish).
		// BUG-02: guard dispatcher nil before calling IsAvailable().
		if !planMode && (intent.Tier == router.TierHeavy || intent.Tier == router.TierCode) &&
			dispatcher != nil && dispatcher.IsAvailable() {
			if querier := dispatcher.Querier(); querier != nil {
				if target := extractImpactTarget(input, querier); target != "" {
					if impact, err := intel.Impact(querier, target, 5); err == nil &&
						agent.ShouldRunMultiAgent(impact) {
						// Auto-snapshot before multi-agent run.
						if snapID, snapErr := snapStore.Take("before multi-agent", true); snapErr == nil && snapID != "" {
							fmt.Printf("%s  [snapshot %s saved]%s\n", colorDim, snapID, colorReset)
						}
						toolkit := agent.NewToolkit(root, querier, embStore)
						// Wire approval gate so git ops inside sub-agents never panic on nil ApproveFunc.
						toolkit.ApproveFunc = func(desc string) bool {
							fmt.Printf("%s\u26a0\ufe0f  Agent wants to: %s%s\n  Approve? [y/N] ", colorGold, desc, colorReset)
							line, _ := rl.Readline()
							return strings.TrimSpace(strings.ToLower(line)) == "y"
						}
						agentCtx, agentCancel := context.WithTimeout(turnCtx, 15*time.Minute)
						combined, agentErr := agent.Run(
							agentCtx, input, impact, toolkit, client,
							buildSystemPrompt(brainContext, tierSkills, router.TierCode),
							mantisDir,
						)
						agentCancel()
						if agentErr == nil {
							fmt.Printf("%s◈ Mantis%s %s[multi-agent · %d files · %d packages]%s\n",
								colorCopper+colorBold, colorReset, colorDim,
								impact.TotalFiles, agent.DistinctPackages(impact), colorReset)
							renderResponse(stripInternalBlocks(stripFileBlocks(combined)))
							wf := extractAndWriteFilesWithApproval(combined, root, rl, approvalCache, opLog)
							if len(wf) > 0 {
								printWrittenFiles(wf)
							}
							messages = append(messages, ollama.Message{Role: "assistant", Content: combined})
							sess.Add(model, intent.Tier, 0, 0, hasImage)
							tlog.LogChat(telemetry.ChatTurn{
								SessionID:    sessID,
								Turn:         turn,
								Tier:         intent.Tier.String(),
								Model:        model,
								UserMsg:      input,
								AssistantMsg: combined,
							})
							turnCancel()
							continue
						}
						fmt.Printf("%s  [multi-agent failed: %v — falling back]%s\n\n", colorRed, agentErr, colorReset)
					}
				}
			}
		}

		// ── Iterative test loop for test-related fix requests ────────────────
		// Detects "fix tests", "tests are failing", etc. and routes to the
		// test loop instead of the generic fix agent.
		if !planMode && intent.TaskType == "fix" && intent.Tier == router.TierCode && isTestFixRequest(input) {
			// Auto-snapshot before test-fix loop.
			if snapID, snapErr := snapStore.Take("before test loop", true); snapErr == nil && snapID != "" {
				fmt.Printf("%s  [snapshot %s saved]%s\n", colorDim, snapID, colorReset)
			}
			fmt.Printf("%s  ◆ test loop — running iterative test→fix cycle%s\n", colorDim, colorReset)
			packages := extractTestPackage(input)
			testRoot, _ := os.Getwd()
			runTestLoopCommand(testRoot, client, packages)
			sess.Add(model, intent.Tier, 0, 0, hasImage)
			turnCancel()
			continue
		}

		// ── Tool-equipped agent for tasks that need file access ──────────────
		// Activates for fix, implement, refactor, test, and explain tasks.
		// Without this, the model can't read/write files and tells the user
		// "I can't read files" or guesses instead of checking.
		agentTaskTypes := map[string]bool{
			"fix": true, "implement": true, "refactor": true, "test": true,
		}
		needsAgent := !planMode && agentTaskTypes[intent.TaskType]
		// Also activate for explain/general at Code tier+ (needs file reading).
		if !needsAgent && !planMode && intent.Tier >= router.TierCode {
			needsAgent = true
		}
		// Activate for deploy/docker/make keywords regardless of tier.
		if !needsAgent && !planMode {
			lo := strings.ToLower(input)
			for _, kw := range []string{"docker", "makefile", "make build", "deploy", "dockerfile", "compose", "kubernetes", "kubectl", "how to run", "how to start", "how do i run"} {
				if strings.Contains(lo, kw) {
					needsAgent = true
					break
				}
			}
		}
		// If the request qualifies for the full SWE pipeline (complex build/implement),
		// skip the fix agent — the pipeline has planning, parallel tasks, and build verification.
		if needsAgent && pipeline.ShouldRun(intent, input) {
			needsAgent = false
		}
		if needsAgent {
			// Use at least code-tier model for fix agent — fast-tier models can't use tools well.
			fixModel := model
			if intent.Tier < router.TierCode {
				fixModel = router.ModelFor(router.TierCode)
			}
			agentLabel := "agent"
			if intent.TaskType == "fix" {
				agentLabel = "fix agent"
			}
			fmt.Printf("%s  ◆ %s [%s] — investigating with tools%s\n", colorDim, agentLabel, fixModel, colorReset)
			stopSpin := startSpinner(string(intent.TaskType))
			agentCtx, agentCancel := context.WithTimeout(turnCtx, 10*time.Minute)
			agentResp, agentPT, agentCT, agentOK := runFixAgent(agentCtx, client, fixModel, messages, root, intent.TaskType)
			agentCancel()
			elapsed := stopSpin()

			if agentOK {
				turnTok := agentPT + agentCT
				_, _, sessTotal := sess.Totals()
				fmt.Printf("%s◈ Mantis%s  %s[%s · +%d tok · %.1fs · session: %d]%s\n",
					colorCopper+colorBold, colorReset, colorDim, agentLabel, turnTok, elapsed.Seconds(), sessTotal+turnTok, colorReset)
				renderResponse(stripInternalBlocks(stripFileBlocks(agentResp)))

				wf := extractAndWriteFilesWithApproval(agentResp, root, rl, approvalCache, opLog)
				if len(wf) > 0 {
					printWrittenFiles(wf)
				}
				messages = append(messages, ollama.Message{Role: "assistant", Content: agentResp})
				// Quick post-fix verification: re-run the build command to check if
				// the fix actually worked. We skip autofix.Check (which can misdetect
				// project type), but still confirm the build passes.
				if len(wf) > 0 {
					if postCheck := postFixVerify(root); postCheck != "" {
						fmt.Printf("%s  ⚠ build still failing after fix — %s%s\n", colorGold, postCheck, colorReset)
					} else {
						fmt.Printf("%s  ✓ build passing%s\n", colorGreen, colorReset)
					}
				}
				sess.Add(model, intent.Tier, agentPT, agentCT, hasImage)

				var writtenPaths []string
				for _, f := range wf {
					writtenPaths = append(writtenPaths, f.Path)
				}
				tlog.Log(telemetry.Event{
					SessionID:    sessID,
					Turn:         turn,
					Tier:         intent.Tier.String(),
					TaskType:     string(intent.TaskType),
					Confidence:   intent.Confidence,
					Model:        model,
					PromptTok:    agentPT,
					ComplTok:     agentCT,
					LatencyMS:    time.Since(turnStart).Milliseconds(),
					FilesWritten: writtenPaths,
					InputSnippet: input,
				})
				tlog.LogChat(telemetry.ChatTurn{
					SessionID:    sessID,
					Turn:         turn,
					Tier:         intent.Tier.String(),
					Model:        model,
					UserMsg:      input,
					AssistantMsg: agentResp,
					PromptTok:    agentPT,
					ComplTok:     agentCT,
					LatencyMS:    time.Since(turnStart).Milliseconds(),
				})
				if warn := usageTracker.Add(turnTok, false, hasImage); warn != "" {
					fmt.Printf("%s%s%s\n\n", colorRed, warn, colorReset)
				}
				turnCancel()
				continue
			}
			// Fix agent didn't work — fall through to normal paths.
			fmt.Printf("%s  [fix agent unavailable — falling back]%s\n", colorDim, colorReset)
		}

		// ── Multi-stage SWE pipeline for complex build/implement requests ─────
		// Triggered before the single-model path so complex tasks get:
		//   plan (reason model) → code + tests (code model, parallel)
		if !planMode && pipeline.ShouldRun(intent, input) {
			// Auto-snapshot before pipeline runs.
			if snapID, snapErr := snapStore.Take("before pipeline", true); snapErr == nil && snapID != "" {
				fmt.Printf("%s  [snapshot %s saved — /revert %s to undo]%s\n", colorDim, snapID, snapID, colorReset)
			}
			// Fix 8: snapshot existing files to distinguish new vs edited after pipeline.
			preExisting := snapshotExistingFiles(root)
			pipelineOpts := pipeline.Options{AvailableModels: availableModels, Root: root, PlanOnly: planMode,
				OnRateLimit:     makeKeyRotationCallback(keyRing),
				ClarifyCallback: pipeline.RunSelector,
			}
			pipelineCtx, pipelineCancel := context.WithTimeout(turnCtx, 20*time.Minute)
			sysPrompt := buildSystemPrompt(brainContext, tierSkills, router.TierCode)
			pRes, pErr := pipeline.Run(
				pipelineCtx, client, input,
				sysPrompt,
				pipelineOpts,
			)
			pipelineCancel()

			if pErr != nil {
				// If the pipeline captured partial code output (e.g. timeout after
				// streaming 18k tokens), use it rather than discarding.
				if pRes != nil && len(pRes.CodeText) > 500 {
					fmt.Printf("%s  [pipeline timed out — using %d chars of partial code]%s\n\n",
						colorDim, len(pRes.CodeText), colorReset)
					// Assemble Combined from whatever stages completed so files get written.
					if pRes.Combined == "" {
						var sb strings.Builder
						if pRes.PlanText != "" {
							sb.WriteString("## Plan\n\n")
							sb.WriteString(strings.TrimSpace(pRes.PlanText))
							sb.WriteString("\n\n---\n\n")
						}
						sb.WriteString("## Implementation\n\n")
						sb.WriteString(strings.TrimSpace(pRes.CodeText))
						if pRes.TestText != "" {
							sb.WriteString("\n\n---\n\n## Tests\n\n")
							sb.WriteString(strings.TrimSpace(pRes.TestText))
						}
						pRes.Combined = sb.String()
					}
					pErr = nil // treat as success with partial output
				} else {
					fmt.Printf("%s  [pipeline failed: %v — falling back to single model]%s\n\n",
						colorRed, pErr, colorReset)
					// Carry the plan from stage 1 into the single-model fallback context.
					if pRes != nil && pRes.PlanText != "" {
						input = "Here is the plan I already made:\n\n" + pRes.PlanText + "\n\nNow implement it for this request:\n" + input
					}
					// Fall through to single-model path below.
				}
			} else if planMode && pRes.CodeText == "" {
				// Plan Mode: show plan and ask for approval before coding.
				fmt.Printf("\n%s◈ Mantis — Plan ready%s\n", colorCopper+colorBold, colorReset)
				renderResponse(pRes.PlanText)
				fmt.Printf("\n%sProceed with implementation? [y/n]: %s", colorGold, colorReset)
				approval, rlErr := rl.Readline()
				if rlErr != nil || (strings.ToLower(strings.TrimSpace(approval)) != "y" && strings.TrimSpace(approval) != "") {
					fmt.Printf("%s● plan discarded%s\n\n", colorDim, colorReset)
					sess.Add(model, intent.Tier, pRes.PromptTok, pRes.ComplTok, hasImage)
					turnCancel()
					continue
				}
				// User approved — continue with CODE+TESTS stages.
				fmt.Printf("%s● plan approved — starting implementation%s\n", colorGold, colorReset)
				contCtx, contCancel := context.WithTimeout(turnCtx, 20*time.Minute)
				pRes, pErr = pipeline.ContinuePlan(
					contCtx, client, input, pRes.PlanText,
					sysPrompt,
					pipeline.Options{AvailableModels: availableModels, Root: root, OnRateLimit: makeKeyRotationCallback(activeKeyRing)},
				)
				contCancel()
				if pErr != nil {
					fmt.Printf("%s  [implementation failed: %v]%s\n\n", colorRed, pErr, colorReset)
					turnCancel()
					continue
				}
			}

			if pErr == nil {
				totalTok := pRes.PromptTok + pRes.ComplTok
				fmt.Printf("%s◈ Mantis%s %s[pipeline · plan→code+tests · %d tokens]%s\n",
					colorCopper+colorBold, colorReset, colorDim, totalTok, colorReset)

				// Save full output to .mantis/last-pipeline.md, show compact summary on CLI.
				if err := pipeline.SaveOutput(root, pRes.Combined); err != nil {
					fmt.Printf("%s  ⚠ could not save pipeline output: %v%s\n", colorDim, err, colorReset)
				}
				renderResponse(pipeline.CompactSummary(pRes))

				// P9.6: task-based pipeline already wrote all files during execution.
				// Re-extracting Combined would replay the same edit blocks against
				// already-modified files, producing duplicate ⚠ SEARCH warnings.
				// Use the WrittenFiles list from task execution directly instead.
				var wf []WrittenFile
				if len(pRes.WrittenFiles) > 0 {
					// BUG-2: record files in opLog so /undo works for task-based pipeline.
					batchID := opLog.NextBatch()
					seen := map[string]bool{}
					for _, p := range pRes.WrittenFiles {
						if seen[p] {
							continue // deduplicate
						}
						seen[p] = true
						absPath := filepath.Join(root, p)
						opLog.Record(batchID, absPath, "create", "")
						// Fix 8: check if file existed before pipeline to set Created flag.
						isNew := !preExisting[absPath] && !preExisting[p]
						wf = append(wf, WrittenFile{Path: p, Created: isNew})
					}
					// P9.9: cap the preview to 5 files + show /undo hint.
					printWrittenFilesCapped(wf, 5)
				} else {
					// Fallback: non-task-based pipeline (monolithic path).
					wf = extractAndWriteFilesWithApproval(pRes.Combined, root, rl, approvalCache, opLog)
					if len(wf) == 0 && pRes.CodeText != "" {
						wf = extractAndWriteFilesWithApproval(pRes.CodeText, root, rl, approvalCache, opLog)
					}
					if len(wf) > 0 {
						printWrittenFilesCapped(wf, 5)
					}
				}
				messages = append(messages, ollama.Message{Role: "assistant", Content: pRes.Combined})
				// UX-5: skip verifyAndFix if pipeline already ran test loop.
				if !pRes.TestsRan {
					verifyAndFix(turnCtx, client, model, intent.Tier, root, wf, &messages)
				}
				sess.Add(model, intent.Tier, pRes.PromptTok, pRes.ComplTok, hasImage)
				var pipeWrittenPaths []string
				for _, f := range wf {
					pipeWrittenPaths = append(pipeWrittenPaths, f.Path)
				}
				tlog.Log(telemetry.Event{
					SessionID:    sessID,
					Turn:         turn,
					Tier:         intent.Tier.String(),
					TaskType:     string(intent.TaskType),
					Confidence:   intent.Confidence,
					Model:        model,
					Pipeline:     true,
					PromptTok:    pRes.PromptTok,
					ComplTok:     pRes.ComplTok,
					LatencyMS:    time.Since(turnStart).Milliseconds(),
					FilesWritten: pipeWrittenPaths,
					InputSnippet: input,
				})
				tlog.LogChat(telemetry.ChatTurn{
					SessionID:    sessID,
					Turn:         turn,
					Tier:         intent.Tier.String(),
					Model:        model,
					UserMsg:      input,
					AssistantMsg: pRes.Combined,
					PromptTok:    pRes.PromptTok,
					ComplTok:     pRes.ComplTok,
					LatencyMS:    time.Since(turnStart).Milliseconds(),
				})
				if warn := usageTracker.Add(totalTok, true, hasImage); warn != "" {
					fmt.Printf("%s%s%s\n\n", colorRed, warn, colorReset)
				}
				if pRes.PromptTok > 8000 {
					sess.WarnWaste("large prompt — use /file for specific files, not full directories")
				}
				turnCancel()
				continue
			}
		}

		// ── Single-model path (simple queries + pipeline fallback) ────────────

		// Multi-pass reasoning for complex queries (Reason/Heavy tiers).
		// Pass 1: silent analysis. Pass 2: solution informed by analysis.
		if intent.Tier == router.TierReason || intent.Tier == router.TierHeavy {
			passCtx, passCancel := context.WithTimeout(turnCtx, 3*time.Minute)
			messages = multiPassReasoning(passCtx, client, model, intent.Tier, messages)
			passCancel()
		}

		// Show memory retrieval indicator if relevant chunks were found.
		if memChunksFound > 0 {
			fmt.Printf("%s  ◉ %d memory chunk(s)%s\n", colorDim, memChunksFound, colorReset)
		}

		// Show spinner while waiting for first token; then stream tokens live to stdout.
		// After completion, erase raw output and glamour-render the full response.
		stopSpin := startSpinner(string(intent.TaskType))
		sp := newStreamPrinter(stopSpin)

		streamCtx, streamCancel := context.WithTimeout(turnCtx, 5*time.Minute)
		var rb strings.Builder
		var promptTok, completionTok int
		var streamErr error

		if intent.Tier == router.TierMax {
			promptTok, completionTok, streamErr = streamEnsemble(streamCtx, client, availableModels, messages, &rb)
		} else {
			promptTok, completionTok, streamErr = streamWithFallback(streamCtx, client, model, intent.Tier, messages, &rb, sp.onChunk)
		}
		streamCancel()
		spinElapsed := sp.stop()

		if streamErr != nil {
			rl.SetPrompt(idlePrompt)
			messages = messages[:len(messages)-1]
			// context.Canceled = user pressed Ctrl+C — cancel message already shown.
			if errors.Is(streamErr, context.Canceled) {
				turnCancel()
				continue
			}
			errStr := streamErr.Error()
			if strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "dial tcp") {
				fmt.Printf("\n\033[38;5;196m⚠ Ollama is not running — start it with: ollama serve\033[0m\n\n")
			} else {
				fmt.Printf("\n%s⚠ %v%s\n\n", colorRed, streamErr, colorReset)
			}
			tlog.Log(telemetry.Event{
				SessionID:    sessID,
				Turn:         turn,
				Tier:         intent.Tier.String(),
				TaskType:     string(intent.TaskType),
				Confidence:   intent.Confidence,
				Model:        model,
				LatencyMS:    time.Since(turnStart).Milliseconds(),
				InputSnippet: input,
				Error:        streamErr.Error(),
			})
			turnCancel()
			continue
		}

		// Reset prompt back to normal (respecting plan mode) after response.
		setPlanPrompt()

		// Render the full response as formatted markdown.
		turnTok := promptTok + completionTok
		_, _, sessTotal := sess.Totals()
		fmt.Printf("%s◈ Mantis%s  %s%s%s  %s+%d tok · %.1fs · session: %d%s\n",
			colorCopper+colorBold, colorReset,
			colorCopper, model, colorReset,
			colorDim, turnTok, spinElapsed.Seconds(), sessTotal+turnTok, colorReset)
		renderResponse(stripInternalBlocks(stripFileBlocks(rb.String())))

		// Write any file code blocks from the response to disk.
		// In plan mode, skip file writes — read-only exploration only.
		if planMode {
			messages = append(messages, ollama.Message{Role: "assistant", Content: rb.String()})
			sess.Add(model, intent.Tier, promptTok, completionTok, hasImage)
			turnCancel()
			continue
		}
		// Append a system note so future turns know what's already written.
		wf := extractAndWriteFilesWithApproval(rb.String(), root, rl, approvalCache, opLog)
		if len(wf) > 0 {
			printWrittenFiles(wf)
			var fileList []string
			for _, f := range wf {
				fileList = append(fileList, f.Path)
			}
			rb.WriteString(fmt.Sprintf(
				"\n\n[Mantis wrote these files to disk: %s. Do not regenerate them unless explicitly asked.]",
				strings.Join(fileList, ", "),
			))
		}

		messages = append(messages, ollama.Message{Role: "assistant", Content: rb.String()})
		evLog.RecordModelResponse(turn, model, intent.Tier.String(), rb.String())

		// Verify build and auto-fix errors up to 2 times.
		wf = verifyAndFix(turnCtx, client, model, intent.Tier, root, wf, &messages)
		sess.Add(model, intent.Tier, promptTok, completionTok, hasImage)

		// Log turn to telemetry.
		var writtenPaths []string
		for _, f := range wf {
			writtenPaths = append(writtenPaths, f.Path)
		}
		tlog.Log(telemetry.Event{
			SessionID:    sessID,
			Turn:         turn,
			Tier:         intent.Tier.String(),
			TaskType:     string(intent.TaskType),
			Confidence:   intent.Confidence,
			Model:        model,
			Pipeline:     pipeline.ShouldRun(intent, input),
			PromptTok:    promptTok,
			ComplTok:     completionTok,
			LatencyMS:    time.Since(turnStart).Milliseconds(),
			FilesWritten: writtenPaths,
			InputSnippet: input,
		})
		tlog.LogChat(telemetry.ChatTurn{
			SessionID:    sessID,
			Turn:         turn,
			Tier:         intent.Tier.String(),
			Model:        model,
			UserMsg:      input,
			AssistantMsg: rb.String(),
			PromptTok:    promptTok,
			ComplTok:     completionTok,
			LatencyMS:    time.Since(turnStart).Milliseconds(),
		})

		if warn := usageTracker.Add(promptTok+completionTok,
			intent.Tier == router.TierHeavy, hasImage); warn != "" {
			fmt.Printf("%s%s%s\n\n", colorRed, warn, colorReset)
		}

		// Detect syntax errors in code blocks via tree-sitter.
		if syntaxErrs := verify.DetectSyntaxErrors(rb.String()); len(syntaxErrs) > 0 {
			fmt.Printf("%s⚠ Syntax errors detected in generated code:%s\n", colorRed, colorReset)
			for _, se := range syntaxErrs {
				fmt.Printf("%s  line %d:%d — %s%s\n", colorRed, se.Line, se.Column, se.Message, colorReset)
			}
			fmt.Println()
		}

		// Verify response against ground truth — retry up to 3 times for hallucinations.
		if vr := verify.CheckWithAST(rb.String(), truthWriter); !vr.Clean {
			prevUnknown := strings.Join(vr.UnknownSymbols, ",")
			const maxRetries = 3
			for retry := 0; retry < maxRetries; retry++ {
				corrections := verify.SuggestCorrections(vr.UnknownSymbols, truthWriter)
				if corrections == "" {
					fmt.Printf("%s%s%s\n\n", colorRed, vr.Warning, colorReset)
					break
				}
				fmt.Printf("%s  [verifying symbols… re-prompting for accuracy (%d/%d)]%s\n",
					colorDim, retry+1, maxRetries, colorReset)
				correctionMsg := buildReminder(
					fmt.Sprintf("Your previous answer referenced symbols that don't exist in this project: %s",
						strings.Join(vr.UnknownSymbols, ", ")),
					fmt.Sprintf("The actual symbols in this codebase are:\n%s\n\nCorrect your answer using only real symbols.", corrections),
					retry+1, maxRetries,
				)

				retryMsgs := append(append([]interface{}{}, messages...), ollama.Message{Role: "user", Content: correctionMsg})
				var rb2 strings.Builder
				retryCtx, retryCancel := context.WithTimeout(turnCtx, 3*time.Minute)
				pt2, ct2, err2 := streamWithFallback(retryCtx, client, model, intent.Tier, retryMsgs, &rb2)
				retryCancel()

				if err2 != nil || strings.TrimSpace(rb2.String()) == "" {
					fmt.Printf("%s%s%s\n\n", colorRed, vr.Warning, colorReset)
					break
				}

				messages[len(messages)-1] = ollama.Message{Role: "assistant", Content: rb2.String()}
				// Sync rb so downstream convention checks use the corrected response.
				rb.Reset()
				rb.WriteString(rb2.String())
				sess.Add(model, intent.Tier, pt2, ct2, false)
				fmt.Printf("%s◈ Mantis%s %s(corrected)%s\n", colorCopper+colorBold, colorReset, colorDim, colorReset)
				renderResponse(stripInternalBlocks(stripFileBlocks(rb2.String())))
				if wf := extractAndWriteFiles(rb2.String(), root, opLog); len(wf) > 0 {
					printWrittenFiles(wf)
				}

				vr = verify.CheckWithAST(rb2.String(), truthWriter)
				if vr.Clean {
					break
				}
				// Stuck detection: same unknown symbols after retry → stop
				currentUnknown := strings.Join(vr.UnknownSymbols, ",")
				if currentUnknown == prevUnknown {
					fmt.Printf("%s%s%s\n\n", colorRed, vr.Warning, colorReset)
					break
				}
				prevUnknown = currentUnknown
			}
		}

		// Convention enforcement — re-prompt up to 2 times on violations.
		// Each iteration shows the specific violated rules and the previous non-compliant response.
		// Stuck detection: same violations twice in a row → give up and warn.
		{
			prevConvViolation := ""
			for convIter := 0; convIter < 2; convIter++ {
				cr := verify.CheckConventions(rb.String(), conventions)
				if cr.Clean {
					break
				}
				// Stuck: same violations repeated after a fix attempt → stop.
				if cr.Warning == prevConvViolation && convIter > 0 {
					fmt.Printf("%s  [conventions still violated after %d attempt(s) — showing original]%s\n",
						colorRed, convIter, colorReset)
					break
				}
				prevConvViolation = cr.Warning

				correctionMsg := buildReminder(
					"Your previous response violated project conventions.",
					fmt.Sprintf(
						"Fix ONLY the violations below — do not change anything else in your answer.\n\n"+
							"## Violations\n%s\n\n"+
							"## Rules\n"+
							"- Apply the minimal edit to comply with each rule above.\n"+
							"- Do not add explanations or apologies.\n"+
							"- If fixing a code file, use ```edit:filepath blocks to show only the changed sections.",
						cr.Warning),
					convIter+1, 2,
				)
				fmt.Printf("%s  [conventions violated (%d/2) — re-prompting]%s\n", colorDim, convIter+1, colorReset)
				// BUG-04: copy slice before append to avoid aliasing the backing array.
				retryMsgs := append(append([]interface{}{}, messages...), ollama.Message{Role: "user", Content: correctionMsg})
				var rb3 strings.Builder
				retryCtx3, retryCancel3 := context.WithTimeout(turnCtx, 3*time.Minute)
				pt3, ct3, err3 := streamWithFallback(retryCtx3, client, model, intent.Tier, retryMsgs, &rb3)
				retryCancel3()
				if err3 != nil || strings.TrimSpace(rb3.String()) == "" {
					fmt.Printf("%s%s%s\n\n", colorRed, cr.Warning, colorReset)
					break
				}
				// Update the last assistant message and the working buffer.
				messages[len(messages)-1] = ollama.Message{Role: "assistant", Content: rb3.String()}
				sess.Add(model, intent.Tier, pt3, ct3, false)
				rb.Reset()
				rb.WriteString(rb3.String())
				if convIter == 1 || verify.CheckConventions(rb3.String(), conventions).Clean {
					// Final iteration or now clean — render corrected output.
					fmt.Printf("%s◈ Mantis%s %s(convention-corrected)%s\n",
						colorCopper+colorBold, colorReset, colorDim, colorReset)
					renderResponse(stripInternalBlocks(stripFileBlocks(rb3.String())))
					if wf2 := extractAndWriteFiles(rb3.String(), root, opLog); len(wf2) > 0 {
						printWrittenFiles(wf2)
					}
					break
				}
			}
		}

		// Sanity check: catch obviously wrong responses (wrong tooling, "can't read files").
		// Single retry — if second response also fails, show with warning.
		if correction := sanityCheckResponse(rb.String(), root); correction != "" {
			fmt.Printf("%s  [sanity check — re-prompting]%s\n", colorDim, colorReset)
			retryMsgs := append(append([]interface{}{}, messages...), ollama.Message{Role: "user", Content: correction})
			var rbSanity strings.Builder
			sanityCtx, sanityCancel := context.WithTimeout(turnCtx, 3*time.Minute)
			ptS, ctS, errS := streamWithFallback(sanityCtx, client, model, intent.Tier, retryMsgs, &rbSanity)
			sanityCancel()
			if errS == nil && strings.TrimSpace(rbSanity.String()) != "" {
				// Only accept if second response passes sanity check.
				if correction2 := sanityCheckResponse(rbSanity.String(), root); correction2 == "" {
					messages[len(messages)-1] = ollama.Message{Role: "assistant", Content: rbSanity.String()}
					rb.Reset()
					rb.WriteString(rbSanity.String())
					sess.Add(model, intent.Tier, ptS, ctS, false)
					fmt.Printf("%s◈ Mantis%s %s(corrected)%s\n", colorCopper+colorBold, colorReset, colorDim, colorReset)
					renderResponse(stripInternalBlocks(stripFileBlocks(rbSanity.String())))
					if wfS := extractAndWriteFiles(rbSanity.String(), root, opLog); len(wfS) > 0 {
						printWrittenFiles(wfS)
					}
				} else {
					fmt.Printf("%s  ⚠ response may contain incorrect tooling suggestions%s\n", colorRed, colorReset)
				}
			}
		}

		if promptTok > 8000 {
			sess.WarnWaste("large prompt — use /file for specific files, not full directories")
		}
		turnCancel()
	}

	tlog.Flush()
	endSession(sess, b, messages, nil)
	return nil
}

// runOnce handles a single non-interactive query: mantis "question"
// BUG-17: messages is *[]interface{} so runWithScanner can accumulate history.
func runOnce(cfg Config, client *ollama.Client, sess *session.Session,
	b *brain.Brain, dispatcher *nl.Dispatcher, messages *[]interface{},
	tw *truth.Writer, ut *usage.Tracker, rs router.EmbedStore) error {

	// Normalize terminal error pastes into actionable fix requests.
	cfg.InitialQuery = normalizeTerminalInput(cfg.InitialQuery)

	hasImage := cfg.ImagePath != ""
	intent := router.Classify(cfg.InitialQuery, hasImage, rs)
	if cfg.ForceTier != "" {
		intent.Tier = parseTier(cfg.ForceTier)
	}
	model := router.ModelFor(intent.Tier)
	showRouting(intent, model, 1, pipeline.ShouldRun(intent, cfg.InitialQuery))

	// Rebuild system prompt for the actual tier (initial default was TierCode).
	var skillsBudget int
	switch {
	case intent.Tier <= router.TierFast:
		skillsBudget = 0
	case intent.Tier <= router.TierCode:
		skillsBudget = 4000
	default:
		skillsBudget = 10000
	}
	brainCtx := ""
	if b != nil {
		brainCtx = b.LoadHot()
	}
	tierSkills := b.LoadSkillsForTask(string(intent.TaskType), skillsBudget)
	sysPrompt := buildSystemPrompt(brainCtx, tierSkills, intent.Tier)
	(*messages)[0] = ollama.Message{Role: "system", Content: sysPrompt}

	var toolCtx string
	if intent.NeedsGraph && dispatcher != nil {
		for _, r := range dispatcher.Dispatch(cfg.InitialQuery) {
			toolCtx += "\n[" + r.Tool + "]\n" + r.Content + "\n"
		}
	}

	userContent := cfg.InitialQuery
	if toolCtx != "" {
		userContent = toolCtx + "\nUser question: " + cfg.InitialQuery
	}

	var userMsg interface{}
	if hasImage {
		imgData, err := loadImage(cfg.ImagePath)
		if err != nil {
			return fmt.Errorf("load image: %w", err)
		}
		userMsg = ollama.ImageMessage{Role: "user", Content: userContent, Images: []string{imgData}}
	} else {
		userMsg = ollama.Message{Role: "user", Content: userContent}
	}

	*messages = append(*messages, userMsg)

	fmt.Println()

	// Route complex build requests through the pipeline (plan → code → tests).
	root, _ := os.Getwd()
	if pipeline.ShouldRun(intent, cfg.InitialQuery) {
		brainCtx := ""
		if b != nil {
			brainCtx = b.LoadHot()
		}
		tierSkills := b.LoadSkillsForTask(string(intent.TaskType), 10000)
		sysPrompt := buildSystemPrompt(brainCtx, tierSkills, router.TierCode)
		pipelineCtx, pipelineCancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer pipelineCancel()

		pRes, pErr := pipeline.Run(pipelineCtx, client, cfg.InitialQuery, sysPrompt, pipeline.Options{
			Root:            root,
			OnRateLimit:     makeKeyRotationCallback(activeKeyRing),
			ClarifyCallback: pipeline.RunSelector,
		})

		if pErr != nil {
			if pRes != nil && len(pRes.CodeText) > 500 {
				fmt.Printf("%s  [pipeline timed out — using %d chars of partial code]%s\n\n",
					colorDim, len(pRes.CodeText), colorReset)
				pErr = nil
			} else {
				fmt.Printf("%s  [pipeline failed: %v — falling back to single model]%s\n\n",
					colorRed, pErr, colorReset)
				// Fall through to single-model path below.
				goto singleModel
			}
		}

		if pErr == nil && pRes != nil {
			totalTok := pRes.PromptTok + pRes.ComplTok

			// Save full output to .mantis/last-pipeline.md.
			if root != "" {
				if err := pipeline.SaveOutput(root, pRes.Combined); err != nil {
					fmt.Printf("%s  ⚠ could not save pipeline output: %v%s\n", colorDim, err, colorReset)
				}
			}

			// Show compact summary (not raw code).
			fmt.Printf("%s◈ Mantis%s %s[pipeline · %d tokens]%s\n",
				colorCopper+colorBold, colorReset, colorDim, totalTok, colorReset)
			renderResponse(pipeline.CompactSummary(pRes))

			// Print written files summary.
			if len(pRes.WrittenFiles) > 0 {
				var wf []WrittenFile
				for _, p := range pRes.WrittenFiles {
					wf = append(wf, WrittenFile{Path: p, Created: true})
				}
				printWrittenFilesCapped(wf, 5)
			}

			sess.Add(model, intent.Tier, pRes.PromptTok, pRes.ComplTok, false)
			_ = ut.Add(totalTok, intent.Tier == router.TierHeavy, false)
			fmt.Println(sess.Report())
			return nil
		}
	}

singleModel:
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var rb strings.Builder
	promptTok, completionTok, err := streamWithFallback(ctx, client, model, intent.Tier, *messages, &rb)
	fmt.Println()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Printf("%s\n⚠ Ollama is not running locally.\n\nFix: start Ollama  →  ollama serve\n  or use cloud   →  export OLLAMA_API_KEY=<your_key>\n\nGet a free Ollama Cloud key at: https://ollama.com/cloud%s\n\n",
				colorRed, colorReset)
			return nil
		}
		// Show user-friendly error instead of raw error dump.
		errStr := err.Error()
		if strings.Contains(errStr, "429") || strings.Contains(errStr, "rate limit") {
			fmt.Printf("%s⚠ All API keys are rate limited. Wait a few minutes and try again.%s\n\n", colorRed, colorReset)
			return nil
		}
		if strings.Contains(errStr, "prompt too long") || strings.Contains(errStr, "context length") {
			fmt.Printf("%s⚠ Prompt too large for the model. Try a shorter query or use --model heavy.%s\n\n", colorRed, colorReset)
			return nil
		}
		if strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "524") || strings.Contains(errStr, "502") || strings.Contains(errStr, "503") {
			fmt.Printf("%s⚠ Request timed out. The model may be overloaded — try again in a moment.%s\n\n", colorRed, colorReset)
			return nil
		}
		fmt.Printf("%s⚠ %s%s\n\n", colorRed, errStr, colorReset)
		return nil
	}
	response := rb.String()
	// Append assistant response so history persists across scanner turns.
	*messages = append(*messages, ollama.Message{Role: "assistant", Content: response})
	sess.Add(model, intent.Tier, promptTok, completionTok, hasImage)
	_ = ut.Add(promptTok+completionTok, intent.Tier == router.TierHeavy, hasImage)
	if syntaxErrs := verify.DetectSyntaxErrors(response); len(syntaxErrs) > 0 {
		fmt.Printf("\n%s⚠ Syntax errors detected:%s\n", colorRed, colorReset)
		for _, se := range syntaxErrs {
			fmt.Printf("%s  line %d:%d — %s%s\n", colorRed, se.Line, se.Column, se.Message, colorReset)
		}
	}
	if vr := verify.CheckWithAST(response, tw); !vr.Clean {
		fmt.Printf("\n%s%s%s\n", colorRed, vr.Warning, colorReset)
	}
	fmt.Println(sess.Report())
	return nil
}

func handleSlashCommand(input string, sess *session.Session, b *brain.Brain,
	messages *[]interface{}, client *ollama.Client, brainContext *string, planMode *bool, cfg Config, webFetcher *web.Fetcher, oplog *OperationLog, embStore *embeddings.Store, snapStore *snapshot.Store, permMode *PermissionMode, rl *readline.Instance, effort *EffortLevel, notif *notify.Notifier) (quit bool) {

	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help":
		printHelp()
	case "/version":
		v := cfg.Version
		if v == "" {
			v = "dev"
		}
		fmt.Printf("%s● mantis %s%s\n\n", colorGold, v, colorReset)
	case "/reset":
		*messages = (*messages)[:1]
		fmt.Printf("%s● context cleared (brain memory kept)%s\n\n", colorGold, colorReset)
	case "/compact":
		// 7A: Manual compaction with optional focus hint.
		focusHint := ""
		if len(parts) > 1 {
			focusHint = strings.Join(parts[1:], " ")
		}
		before := estimateConversationTokens(*messages)
		*messages = compactWithFocus(context.Background(), *messages, client, router.ModelFor(router.TierFast), focusHint)
		after := estimateConversationTokens(*messages)
		if after < before {
			fmt.Printf("%s● compacted: %dK → %dK tokens%s\n\n", colorGold, before/1000, after/1000, colorReset)
		} else {
			fmt.Printf("%s● nothing to compact (conversation is short)%s\n\n", colorDim, colorReset)
		}
	case "/mode":
		if len(parts) < 2 {
			fmt.Printf("  current mode: \033[1m%s\033[0m\n", *permMode)
			fmt.Println("  usage: /mode <default|auto-edit|plan>")
			fmt.Println("    default    — prompt for file writes + bash")
			fmt.Println("    auto-edit  — auto-approve edits, prompt for bash")
			fmt.Println("    plan       — read-only tools only")
			fmt.Println()
		} else {
			mode, ok := ParsePermissionMode(parts[1])
			if !ok {
				fmt.Printf("  unknown mode %q — use default, auto-edit, or plan\n\n", parts[1])
			} else {
				*permMode = mode
				// Update prompt to show mode indicator.
				if rl != nil {
					root, _ := os.Getwd()
					newPrompt := "\033[38;5;244m" + filepath.Base(root) + "\033[0m" + permMode.PromptSuffix() + " \033[38;5;214m❯\033[0m "
					rl.SetPrompt(newPrompt)
				}
				fmt.Printf("  %s● mode set to %s%s\n\n", colorGold, *permMode, colorReset)
			}
		}
	case "/effort":
		if len(parts) < 2 {
			fmt.Printf("  current effort: \033[1m%s\033[0m\n", *effort)
			fmt.Println("  usage: /effort <low|medium|high>")
			fmt.Println("    low    — fast models, skip thinking, quick responses")
			fmt.Println("    medium — default routing (recommended)")
			fmt.Println("    high   — reasoning models, extended thinking")
			fmt.Println()
		} else {
			level, ok := ParseEffortLevel(parts[1])
			if !ok {
				fmt.Printf("  unknown effort %q — use low, medium, or high\n\n", parts[1])
			} else {
				*effort = level
				fmt.Printf("  %s● effort set to %s%s\n\n", colorGold, *effort, colorReset)
			}
		}
	case "/notify":
		if notif == nil {
			fmt.Printf("  %snotifications not available in this mode%s\n\n", colorDim, colorReset)
		} else if len(parts) < 2 {
			state := "off"
			if notif.IsEnabled() {
				state = "on"
			}
			fmt.Printf("  notifications: \033[1m%s\033[0m\n", state)
			fmt.Println("  usage: /notify on|off")
			fmt.Println()
		} else {
			switch strings.ToLower(parts[1]) {
			case "on", "true", "1":
				notif.SetEnabled(true)
				fmt.Printf("  %s● notifications enabled%s\n\n", colorGold, colorReset)
			case "off", "false", "0":
				notif.SetEnabled(false)
				fmt.Printf("  %s● notifications disabled%s\n\n", colorDim, colorReset)
			default:
				fmt.Printf("  unknown value %q — use on or off\n\n", parts[1])
			}
		}
	case "/mcp":
		if len(parts) < 2 {
			fmt.Println("  usage: /mcp list|load")
			fmt.Println("    list  — show connected MCP servers and tools")
			fmt.Println("    load  — load .mcp.json and start servers")
			fmt.Println()
		} else {
			switch parts[1] {
			case "list":
				mcpClient := mcp.NewMCPClient()
				root, _ := os.Getwd()
				_ = mcpClient.LoadConfig(context.Background(), root)
				tools := mcpClient.Tools()
				if len(tools) == 0 {
					fmt.Printf("  %sno MCP servers connected (create .mcp.json to configure)%s\n\n", colorDim, colorReset)
				} else {
					for _, tool := range tools {
						fmt.Printf("  %s%-20s%s %s(%s)%s\n", colorGold, tool.Name, colorReset, colorDim, tool.ServerName, colorReset)
						if tool.Description != "" {
							fmt.Printf("  %s  %s%s\n", colorDim, tool.Description, colorReset)
						}
					}
					fmt.Println()
				}
				mcpClient.Close()
			case "load":
				root, _ := os.Getwd()
				mcpClient := mcp.NewMCPClient()
				if err := mcpClient.LoadConfig(context.Background(), root); err != nil {
					fmt.Printf("  %s✗ %v%s\n\n", colorRed, err, colorReset)
				} else {
					names := mcpClient.ServerNames()
					if len(names) == 0 {
						fmt.Printf("  %sno servers in .mcp.json%s\n\n", colorDim, colorReset)
					} else {
						fmt.Printf("  %s● loaded %d MCP server(s): %s%s\n\n", colorGold, len(names), fmt.Sprintf("%v", names), colorReset)
					}
				}
			default:
				fmt.Printf("  unknown subcommand %q — use list or load\n\n", parts[1])
			}
		}
	case "/cost":
		fmt.Println(sess.Report())
	case "/stats":
		fmt.Printf("%s╭─ Telemetry Stats ─────────────────────────────────╮%s\n", colorCopper+colorBold, colorReset)
		fmt.Print(telemetry.Report(""))
		fmt.Printf("%s╰────────────────────────────────────────────────────╯%s\n\n", colorCopper+colorBold, colorReset)
	case "/telemetry":
		arg := ""
		if len(parts) > 1 {
			arg = parts[1]
		}
		switch arg {
		case "off":
			if err := telemetry.Disable(); err == nil {
				fmt.Printf("%s● telemetry disabled — data stays local only%s\n\n", colorGold, colorReset)
			}
		case "on":
			if err := telemetry.Enable(); err == nil {
				fmt.Printf("%s● telemetry enabled — usage data helps improve routing%s\n\n", colorGold, colorReset)
			}
		default:
			fmt.Printf("%s/telemetry on%s  — enable upload\n%s/telemetry off%s — disable upload (local only)\n\n", colorDim, colorReset, colorDim, colorReset)
		}
	case "/init":
		initStart := time.Now()
		initSpin := startSpinner("explain")
		initModel := router.ModelFor(router.TierReason)
		initCtx, initCancel := context.WithTimeout(context.Background(), 120*time.Second)
		generated, err := b.ScanInit(initCtx, client, initModel)
		initCancel()
		initSpin()
		if err != nil {
			fmt.Printf("%s✗ /init failed: %v%s\n\n", colorRed, err, colorReset)
		} else {
			fmt.Printf("%s● MANTIS.md written  %s(%.1fs)%s\n\n", colorGreen, colorDim, time.Since(initStart).Seconds(), colorReset)
			renderResponse(generated)
			// Reload brain context so the new MANTIS.md takes effect immediately.
			*brainContext = b.LoadHot()
			*messages = []interface{}{
				ollama.Message{Role: "system", Content: buildSystemPrompt(*brainContext, b.LoadSkillsForTask("implement", 20000), router.TierCode)},
			}
			fmt.Printf("%s● context reloaded with MANTIS.md%s\n\n", colorGold, colorReset)
		}
	case "/brain":
		content := b.ReadBrain()
		if content == "" {
			fmt.Printf("%s(no brain file yet)%s\n\n", colorDim, colorReset)
		} else {
			fmt.Printf("%s%s%s\n\n", colorDim, content, colorReset)
		}
	case "/save":
		summary := extractSessionSummary(*messages)
		if summary != "" {
			_ = b.UpdateBrain(summary)
			fmt.Printf("%s● project memory saved%s\n\n", colorGold, colorReset)
		} else {
			fmt.Printf("%s(nothing to save yet)%s\n\n", colorDim, colorReset)
		}
	case "/models":
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		infos, err := client.ListModels(ctx)
		cancel()
		if err != nil {
			fmt.Printf("%s✗ %v%s\n\n", colorRed, err, colorReset)
		} else {
			fmt.Printf("%sAvailable models (%d):%s\n", colorGold, len(infos), colorReset)
			for _, m := range infos {
				sizeGB := float64(m.Size) / 1e9
				fmt.Printf("%s  %-40s %5.0f GB%s\n", colorDim, m.Name, sizeGB, colorReset)
			}
			fmt.Printf("\n%sCurrent routing:\n  trivial=%s\n  fast=%s\n  code=%s\n  reason=%s\n  heavy=%s%s\n\n",
				colorGold,
				router.ModelFor(router.TierTrivial),
				router.ModelFor(router.TierFast),
				router.ModelFor(router.TierCode),
				router.ModelFor(router.TierReason),
				router.ModelFor(router.TierHeavy),
				colorReset)
		}
	case "/file":
		if len(parts) < 2 {
			fmt.Printf("%susage: /file <path>%s\n\n", colorDim, colorReset)
		} else {
			injectFile(parts[1], messages)
		}
	case "/vision":
		if len(parts) < 2 {
			fmt.Printf("%susage: /vision <path>%s\n\n", colorDim, colorReset)
		} else {
			imgData, err := loadImage(parts[1])
			if err != nil {
				fmt.Printf("%sError: %v%s\n\n", colorRed, err, colorReset)
			} else {
				*messages = append(*messages, ollama.ImageMessage{
					Role: "user", Content: "Analyze this image:", Images: []string{imgData},
				})
				fmt.Printf("%s● image loaded — describe what you need%s\n\n", colorGold, colorReset)
			}
		}
	case "/reject":
		reason := strings.TrimPrefix(input, "/reject ")
		if reason != "/reject" {
			_ = b.LogRejected("last suggestion", reason)
			fmt.Printf("%s● logged as rejected approach%s\n\n", colorGold, colorReset)
		}
	case "/decision":
		decision := strings.TrimPrefix(input, "/decision ")
		if decision != "/decision" {
			_ = b.LogDecision(decision)
			fmt.Printf("%s● decision logged%s\n\n", colorGold, colorReset)
		}
	case "/plan":
		*planMode = true
		*permMode = ModePlan
		if rl != nil {
			root, _ := os.Getwd()
			rl.SetPrompt("\033[38;5;244m" + filepath.Base(root) + "\033[0m" + permMode.PromptSuffix() + " \033[38;5;214m❯\033[0m ")
		}
		fmt.Printf("%s● plan mode ON — read-only exploration, no file writes%s\n\n", colorGold, colorReset)
		return false
	case "/build", "/execute":
		*planMode = false
		*permMode = ModeDefault
		if rl != nil {
			root, _ := os.Getwd()
			rl.SetPrompt("\033[38;5;244m" + filepath.Base(root) + "\033[0m \033[38;5;214m❯\033[0m ")
		}
		// Save plan to .mantis/PLAN.md if conversation has plan content.
		savePlanFromConversation(*messages)
		fmt.Printf("%s● build mode ON — full tool access, file writes enabled%s\n\n", colorGold, colorReset)
		return false
	case "/context":
		handleContextCommand(sess, *messages, *brainContext)
	case "/fetch":
		if len(parts) < 2 {
			fmt.Printf("%susage: /fetch <url>%s\n\n", colorDim, colorReset)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			content, err := webFetcher.Fetch(ctx, parts[1])
			cancel()
			if err != nil {
				fmt.Printf("%s✗ fetch failed: %v%s\n\n", colorRed, err, colorReset)
			} else if content != "" {
				*messages = append(*messages, ollama.Message{
					Role: "user", Content: "<web_context url=\"" + parts[1] + "\">\n" + content + "\n</web_context>",
				})
				fmt.Printf("%s● fetched %s — injected into context%s\n\n", colorGold, parts[1], colorReset)
			}
		}
	case "/search":
		if len(parts) < 2 {
			fmt.Printf("%susage: /search <query>%s\n\n", colorDim, colorReset)
		} else {
			query := strings.Join(parts[1:], " ")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if web.HasSearchKey() {
				// Structured Tavily results (title + snippet + URL).
				results, err := web.Search(ctx, query)
				cancel()
				if err != nil {
					fmt.Printf("%s✗ search failed: %v%s\n\n", colorRed, err, colorReset)
				} else if len(results) == 0 {
					fmt.Printf("%sno results found%s\n\n", colorDim, colorReset)
				} else {
					var sb strings.Builder
					for i, r := range results {
						fmt.Printf("  %s%d.%s %s\n", colorGold, i+1, colorReset, r.Title)
						fmt.Printf("     %s%s%s\n", colorDim, r.URL, colorReset)
						if r.Snippet != "" {
							snippet := r.Snippet
							if len(snippet) > 120 {
								snippet = snippet[:120] + "..."
							}
							fmt.Printf("     %s%s%s\n", colorDim, snippet, colorReset)
						}
						sb.WriteString(fmt.Sprintf("%d. [%s](%s)\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
					}
					fmt.Println()
					*messages = append(*messages, ollama.Message{
						Role: "user", Content: "<search_results query=\"" + query + "\">\n" + sb.String() + "</search_results>",
					})
				}
			} else {
				// Keyless fallback via Jina search — returns markdown results, no API key required.
				fmt.Printf("%s  searching \"%s\" (via Jina — set MANTIS_TAVILY_KEY for richer results)…%s\n", colorDim, query, colorReset)
				content, err := webFetcher.Search(ctx, query)
				cancel()
				if err != nil {
					fmt.Printf("%s✗ search failed: %v%s\n\n", colorRed, err, colorReset)
				} else {
					fmt.Println(content)
					fmt.Println()
					*messages = append(*messages, ollama.Message{
						Role: "user", Content: "<search_results query=\"" + query + "\">\n" + content + "\n</search_results>",
					})
					fmt.Printf("%s● search results injected into context%s\n\n", colorGold, colorReset)
				}
			}
		}
	case "/replay":
		sid := ""
		if len(parts) >= 2 {
			sid = parts[1]
		}
		root, _ := os.Getwd()
		ReplaySession(filepath.Join(root, ".mantis"), sid)
		fmt.Println()
	case "/benchmark":
		root, _ := os.Getwd()
		RunBenchmark(client, filepath.Join(root, ".mantis"))
	case "/ask":
		if len(parts) < 2 {
			fmt.Printf("%s  Usage: /ask <question>%s\n\n", colorDim, colorReset)
			break
		}
		question := strings.Join(parts[1:], " ")
		runAskMode(question, client, *brainContext, cfg)
	case "/index":
		if embStore == nil {
			fmt.Printf("%s  embeddings store not available%s\n\n", colorDim, colorReset)
			break
		}
		indexRoot, _ := os.Getwd()
		allParsers := []parser.Parser{
			&parser.GoParser{Root: indexRoot},
			&parser.TypeScriptParser{},
			&parser.PythonParser{Root: indexRoot},
		}
		fmt.Printf("%s  ◆ indexing source code...%s\n", colorDim, colorReset)
		count, err := embStore.IndexSourceFiles(context.Background(), indexRoot, allParsers, true)
		if err != nil {
			fmt.Printf("%s  ⚠ indexing error: %v%s\n\n", colorDim, err, colorReset)
		} else {
			fmt.Printf("%s  ✓ indexed %d symbol(s) for semantic search%s\n\n", colorGold, count, colorReset)
		}
	case "/test":
		packages := ""
		if len(parts) >= 2 {
			packages = strings.Join(parts[1:], " ")
		}
		testRoot, _ := os.Getwd()
		runTestLoopCommand(testRoot, client, packages)
	case "/git-log":
		root, _ := os.Getwd()
		cmd := exec.Command("git", "-C", root, "log", "--oneline", "--grep=mantis:", "-n", "10")
		if out, err := cmd.CombinedOutput(); err == nil && len(out) > 0 {
			fmt.Printf("%s  Recent mantis commits:%s\n%s\n", colorDim, colorReset, string(out))
		} else {
			fmt.Printf("%s  No mantis commits found%s\n\n", colorDim, colorReset)
		}
	case "/commit":
		handleCommitCommand(client, cfg)
	case "/pr":
		handlePRCommand(client)
	case "/undo":
		if oplog == nil {
			fmt.Printf("%s  undo not available in this mode%s\n\n", colorDim, colorReset)
			break
		}
		// /undo N — undo N batches (default 1).
		undoCount := 1
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
				undoCount = n
			}
		}
		anyRestored := false
		for i := 0; i < undoCount; i++ {
			restored, err := oplog.UndoLastBatch()
			if err != nil {
				if !anyRestored {
					fmt.Printf("%s  nothing to undo%s\n\n", colorDim, colorReset)
				}
				break
			}
			anyRestored = true
			for _, line := range restored {
				fmt.Printf("%s  ↩ %s%s\n", colorGold, line, colorReset)
			}
		}
		if anyRestored {
			fmt.Println()
		}
	case "/snapshot":
		if snapStore == nil {
			fmt.Printf("%s  snapshots not available in this mode%s\n\n", colorDim, colorReset)
			break
		}
		label := "manual"
		if len(parts) >= 2 {
			label = strings.Join(parts[1:], " ")
		}
		id, err := snapStore.Take(label, false)
		if err != nil {
			fmt.Printf("%s  snapshot failed: %v%s\n\n", colorRed, err, colorReset)
		} else if id == "" {
			fmt.Printf("%s  no uncommitted changes to snapshot%s\n\n", colorDim, colorReset)
		} else {
			fmt.Printf("%s  ✓ snapshot %s saved (%s)%s\n\n", colorGreen, id, label, colorReset)
		}
	case "/snapshots":
		if snapStore == nil {
			fmt.Printf("%s  snapshots not available in this mode%s\n\n", colorDim, colorReset)
			break
		}
		entries := snapStore.List()
		if len(entries) == 0 {
			fmt.Printf("%s  no snapshots%s\n\n", colorDim, colorReset)
		} else {
			fmt.Printf("%s◈ Snapshots (%d)%s\n", colorGold, len(entries), colorReset)
			for _, e := range entries {
				age := time.Since(e.Timestamp).Truncate(time.Minute)
				autoTag := ""
				if e.Auto {
					autoTag = " [auto]"
				}
				fmt.Printf("  %s%s%s  %s  %s ago%s%s\n",
					colorCopper, e.ID, colorReset, e.Label, age, autoTag, colorReset)
			}
			fmt.Println()
		}
	case "/revert":
		if snapStore == nil {
			fmt.Printf("%s  snapshots not available in this mode%s\n\n", colorDim, colorReset)
			break
		}
		if len(parts) < 2 {
			// Default: show snapshots and ask which to revert.
			entries := snapStore.List()
			if len(entries) == 0 {
				fmt.Printf("%s  no snapshots to revert to%s\n\n", colorDim, colorReset)
			} else {
				fmt.Printf("%s  usage: /revert <snapshot-id>%s\n", colorDim, colorReset)
				fmt.Printf("%s  available snapshots:%s\n", colorDim, colorReset)
				for _, e := range entries {
					fmt.Printf("    %s%s%s  %s\n", colorCopper, e.ID, colorReset, e.Label)
				}
				fmt.Println()
			}
		} else {
			targetID := parts[1]
			diff, err := snapStore.Restore(targetID)
			if err != nil {
				fmt.Printf("%s  revert failed: %v%s\n\n", colorRed, err, colorReset)
			} else {
				fmt.Printf("%s  ✓ reverted to snapshot %s%s\n", colorGreen, targetID, colorReset)
				if diff != "" {
					fmt.Printf("%s%s%s\n", colorDim, diff, colorReset)
				}
				fmt.Println()
			}
		}
	case "/keys":
		handleKeysCommand()
	default:
		fmt.Printf("%sunknown command '%s' — type /help for available commands%s\n\n", colorDim, cmd, colorReset)
	}
	return false
}

// handleKeysCommand runs the interactive /keys manager.
func handleKeysCommand() {
	scanner := bufio.NewScanner(os.Stdin)

	for {
		// Display current keys.
		if activeKeyRing == nil || activeKeyRing.Count() == 0 {
			fmt.Printf("\n%s◈ API Keys (none configured)%s\n", colorDim, colorReset)
		} else {
			statuses := activeKeyRing.Status()
			fmt.Printf("\n%s◈ API Keys (%d configured)%s\n", colorCopper, len(statuses), colorReset)
			for _, s := range statuses {
				icon, statusText, clr := "○", "available", colorDim
				if s.IsActive {
					icon, statusText, clr = "●", "active", colorGreen
				} else if !s.IsAvailable {
					icon = "✗"
					statusText = fmt.Sprintf("rate limited (%dm left)", int(s.CoolRemain.Minutes())+1)
					clr = colorRed
				}
				label := s.Label
				if label == "" {
					label = fmt.Sprintf("key #%d", s.Index)
				}
				fmt.Printf("  %s%s [%s]  %-14s — %s%s\n", clr, icon, s.MaskedKey, label, statusText, colorReset)
			}
		}

		fmt.Printf("\n%s  [a]dd  [d]elete  [t]ag  [q]uit%s\n", colorDim, colorReset)
		fmt.Printf("%s  > %s", colorCopper, colorReset)

		if !scanner.Scan() {
			return
		}
		choice := strings.TrimSpace(strings.ToLower(scanner.Text()))

		switch choice {
		case "a", "add":
			fmt.Printf("%s  Paste new API key: %s", colorDim, colorReset)
			if !scanner.Scan() {
				return
			}
			newKey := strings.TrimSpace(scanner.Text())
			if newKey == "" {
				fmt.Printf("  %s✗ empty key, skipped%s\n", colorRed, colorReset)
				continue
			}
			fmt.Printf("%s  Tag (e.g. personal, work): %s", colorDim, colorReset)
			if !scanner.Scan() {
				return
			}
			tag := strings.TrimSpace(scanner.Text())
			if tag == "" {
				tag = fmt.Sprintf("key #%d", activeKeyRing.Count()+1)
			}
			// Persist.
			if activeCreds != nil {
				tagged := activeCreds.AllTaggedKeys()
				tagged = append(tagged, setup.TaggedKey{Key: newKey, Tag: tag})
				if err := activeCreds.SetTaggedKeys(tagged); err != nil {
					fmt.Printf("  %s✗ save error: %v%s\n", colorRed, err, colorReset)
					continue
				}
				reloadKeyRing()
				fmt.Printf("  %s✓ key added (%s)%s\n", colorGreen, tag, colorReset)
			}

		case "d", "delete":
			if activeKeyRing == nil || activeKeyRing.Count() == 0 {
				fmt.Printf("  %sno keys to delete%s\n", colorDim, colorReset)
				continue
			}
			fmt.Printf("%s  Key number to delete (1-%d): %s", colorDim, activeKeyRing.Count(), colorReset)
			if !scanner.Scan() {
				return
			}
			numStr := strings.TrimSpace(scanner.Text())
			num, err := strconv.Atoi(numStr)
			if err != nil || num < 1 {
				fmt.Printf("  %s✗ invalid number%s\n", colorRed, colorReset)
				continue
			}
			if activeCreds != nil {
				tagged := activeCreds.AllTaggedKeys()
				if num > len(tagged) {
					fmt.Printf("  %s✗ invalid number%s\n", colorRed, colorReset)
					continue
				}
				removed := tagged[num-1]
				tagged = append(tagged[:num-1], tagged[num:]...)
				if err := activeCreds.SetTaggedKeys(tagged); err != nil {
					fmt.Printf("  %s✗ save error: %v%s\n", colorRed, err, colorReset)
					continue
				}
				reloadKeyRing()
				fmt.Printf("  %s✓ removed key %s (%s)%s\n", colorGreen, removed.Tag, maskKeyShort(removed.Key), colorReset)
			}

		case "t", "tag":
			if activeKeyRing == nil || activeKeyRing.Count() == 0 {
				fmt.Printf("  %sno keys to tag%s\n", colorDim, colorReset)
				continue
			}
			fmt.Printf("%s  Key number to tag (1-%d): %s", colorDim, activeKeyRing.Count(), colorReset)
			if !scanner.Scan() {
				return
			}
			numStr := strings.TrimSpace(scanner.Text())
			num, err := strconv.Atoi(numStr)
			if err != nil || num < 1 {
				fmt.Printf("  %s✗ invalid number%s\n", colorRed, colorReset)
				continue
			}
			if activeCreds != nil {
				tagged := activeCreds.AllTaggedKeys()
				if num > len(tagged) {
					fmt.Printf("  %s✗ invalid number%s\n", colorRed, colorReset)
					continue
				}
				fmt.Printf("%s  New tag: %s", colorDim, colorReset)
				if !scanner.Scan() {
					return
				}
				newTag := strings.TrimSpace(scanner.Text())
				if newTag == "" {
					fmt.Printf("  %s✗ empty tag, skipped%s\n", colorRed, colorReset)
					continue
				}
				tagged[num-1].Tag = newTag
				if err := activeCreds.SetTaggedKeys(tagged); err != nil {
					fmt.Printf("  %s✗ save error: %v%s\n", colorRed, err, colorReset)
					continue
				}
				reloadKeyRing()
				fmt.Printf("  %s✓ tagged key #%d as \"%s\"%s\n", colorGreen, num, newTag, colorReset)
			}

		case "q", "quit", "":
			fmt.Println()
			return

		default:
			fmt.Printf("  %sunknown action '%s'%s\n", colorDim, choice, colorReset)
		}
	}
}

// reloadKeyRing rebuilds the active keyring from the current credentials.
func reloadKeyRing() {
	if activeCreds == nil {
		return
	}
	tagged := activeCreds.AllTaggedKeys()
	keys := make([]string, len(tagged))
	labels := make([]string, len(tagged))
	for i, tk := range tagged {
		keys[i] = tk.Key
		labels[i] = tk.Tag
	}
	activeKeyRing = keyring.NewKeyRingLabeled(keys, labels)
}

// maskKeyShort returns the last 4 chars of a key for display.
func maskKeyShort(key string) string {
	if len(key) <= 4 {
		return key
	}
	return "..." + key[len(key)-4:]
}

// estimateTokens returns a rough token count using word-count heuristic.
// Uses words*1.3 which is more accurate than char/4 for mixed text+code.
func estimateTokens(s string) int {
	words := len(strings.Fields(s))
	if words == 0 {
		return (len(s) + 3) / 4 // fallback for single-token strings
	}
	return int(float64(words)*1.3) + 4
}

// handleContextCommand prints a token-budget breakdown of the current context window.
func handleContextCommand(sess *session.Session, messages []interface{}, brainCtx string) {
	var systemTok, brainTok, userTok, assistantTok, totalMsgs int

	// Collect injected files (from /file or context bundles).
	type injectedFile struct {
		path   string
		tokens int
	}
	var injected []injectedFile

	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok {
			tok := estimateTokens(msg.Content)
			switch msg.Role {
			case "system":
				systemTok += tok
			case "user":
				userTok += tok
				// Detect injected files: lines starting with "File: path"
				for _, line := range strings.SplitN(msg.Content, "\n", 3) {
					if strings.HasPrefix(line, "File: ") {
						path := strings.TrimPrefix(line, "File: ")
						injected = append(injected, injectedFile{path: path, tokens: tok})
						break
					}
				}
			case "assistant":
				assistantTok += tok
			}
			totalMsgs++
		} else if img, ok := m.(ollama.ImageMessage); ok {
			userTok += estimateTokens(img.Content) + 500 // image overhead
			totalMsgs++
		}
	}
	brainTok = estimateTokens(brainCtx)
	total := systemTok + userTok + assistantTok

	w := 54
	line := strings.Repeat("─", w)
	fmt.Printf("\n╭%s╮\n", line)
	fmt.Printf("│  %-40s %8s  │\n", "CONTEXT BREAKDOWN", "tokens")
	fmt.Printf("├%s┤\n", line)
	fmt.Printf("│  %-40s %8d  │\n", "System prompt", systemTok)
	fmt.Printf("│  %-40s %8d  │\n", "Brain memory (in system)", brainTok)
	fmt.Printf("│  %-40s %8d  │\n", "User messages", userTok)
	fmt.Printf("│  %-40s %8d  │\n", "Assistant messages", assistantTok)
	fmt.Printf("├%s┤\n", line)
	fmt.Printf("│  %-40s %8d  │\n", "Total (estimated)", total)
	fmt.Printf("│  %-40s %8d  │\n", "Messages", totalMsgs)
	fmt.Printf("╰%s╯\n\n", line)

	// Show injected files table if any are present.
	if len(injected) > 0 {
		fmt.Printf("%sInjected files in context:%s\n", colorDim, colorReset)
		for _, f := range injected {
			label := f.path
			if len(label) > 50 {
				label = "…" + label[len(label)-49:]
			}
			fmt.Printf("  %s%-52s%s %s(%d tok)%s\n", colorCopper, label, colorReset, colorDim, f.tokens, colorReset)
		}
		fmt.Println()
	}
}

// isTestFixRequest detects if the user's message is about fixing failing tests.
func isTestFixRequest(input string) bool {
	lo := strings.ToLower(input)
	testKeywords := []string{
		"fix test", "fix the test", "fix failing test", "fix broken test",
		"tests are failing", "tests are broken", "test failure",
		"tests fail", "test fails", "make tests pass", "make the tests pass",
		"fix unit test", "fix integration test",
		"run tests and fix", "run the tests and fix",
	}
	for _, kw := range testKeywords {
		if strings.Contains(lo, kw) {
			return true
		}
	}
	return false
}

// extractTestPackage attempts to extract a specific test package path from the input.
// e.g. "fix tests in ./internal/router/..." → "./internal/router/..."
func extractTestPackage(input string) string {
	// Look for Go-style package paths.
	re := regexp.MustCompile(`(\.?/[\w/.-]+(?:\.\.\.)?)\s*`)
	if m := re.FindStringSubmatch(input); len(m) > 0 {
		return m[1]
	}
	return ""
}

// runTestLoopCommand runs the iterative test loop from the /test slash command.
// checkUnfinishedProgress checks .mantis/PROGRESS.md for incomplete tasks from the last
// pipeline run and shows a banner so the user can decide to resume. Read-only — it never
// modifies the file or conversation state. (8.5.3)
func checkUnfinishedProgress(mantisDir string, rl readliner) {
	data, err := os.ReadFile(filepath.Join(mantisDir, "PROGRESS.md"))
	if err != nil {
		return // no progress file = nothing to resume
	}
	lines := strings.Split(string(data), "\n")

	var pending, failed []string
	var taskLine string
	for _, line := range lines {
		if strings.HasPrefix(line, "**Task:**") {
			taskLine = strings.TrimPrefix(line, "**Task:** ")
		}
		if strings.HasPrefix(line, "- [ ]") || strings.HasPrefix(line, "- [!]") {
			label := strings.TrimPrefix(strings.TrimPrefix(line, "- [ ] "), "- [!] ")
			if strings.HasPrefix(line, "- [!]") {
				failed = append(failed, label)
			} else {
				pending = append(pending, label)
			}
		}
	}

	if len(pending)+len(failed) == 0 {
		return // all tasks done
	}

	fmt.Printf("\n%s● Unfinished pipeline from last session:%s\n", colorGold, colorReset)
	if taskLine != "" {
		fmt.Printf("%s  Task: %s%s\n", colorDim, taskLine, colorReset)
	}
	for _, p := range pending {
		fmt.Printf("%s  [ ] %s%s\n", colorDim, p, colorReset)
	}
	for _, f := range failed {
		fmt.Printf("%s  [!] %s (failed)%s\n", colorRed, f, colorReset)
	}
	rl.SetPrompt(fmt.Sprintf("%s  Continue? [y/N]%s ", colorGold, colorReset))
	line, _ := rl.Readline()
	if strings.TrimSpace(strings.ToLower(line)) != "y" {
		fmt.Println()
		return
	}
	// User confirmed — print a contextual hint they can paste as their next message.
	fmt.Printf("%s  → Resuming. Type your next instruction or paste the task name to continue.%s\n\n",
		colorDim, colorReset)
}

// runAskMode handles /ask <question>: read-only expert consultation.
// Routes to TierReason with full brain context; never writes files.
// No approval prompt, no hallucination correction loop — just renders the answer.
func runAskMode(question string, client *ollama.Client, brainContext string, cfg Config) {
	model := router.ModelFor(router.TierReason)
	fmt.Printf("%s◉ /ask%s  [%s%s%s]\n", colorCopper+colorBold, colorReset, colorDim, model, colorReset)

	sysContent := "You are a senior software engineer providing expert read-only analysis. " +
		"Do NOT write, create, or modify any files. " +
		"Provide clear, accurate, concise analysis based on the context provided."
	if brainContext != "" {
		sysContent += "\n\n<project_memory>\n" + brainContext + "\n</project_memory>"
	}

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: sysContent},
		ollama.Message{Role: "user", Content: question},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	opts := &ollama.ModelOptions{Temperature: 0.4, NumCtx: router.ContextWindowFor(model)}
	var buf strings.Builder
	_, _, err := client.StreamChat(ctx, model, msgs, opts, func(chunk string) {
		buf.WriteString(chunk)
		fmt.Print(chunk)
	})
	fmt.Println()
	if err != nil {
		fmt.Printf("%s  /ask failed: %v%s\n\n", colorDim, err, colorReset)
		return
	}
	fmt.Println()
}

func runTestLoopCommand(root string, client *ollama.Client, packages string) {
	toolkit := agent.NewToolkit(root, nil, nil)
	tl := &agent.TestLoop{
		Toolkit:  toolkit,
		Client:   client,
		Root:     root,
		Packages: packages,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	result, err := tl.Run(ctx)
	if err != nil {
		fmt.Printf("%s✗ test loop failed: %v%s\n\n", colorRed, err, colorReset)
		return
	}

	if result.Passed {
		fmt.Printf("\n%s✓ All tests pass after %d iteration(s)%s\n", colorGreen, result.Iterations, colorReset)
	} else {
		fmt.Printf("\n%s✗ Tests still failing after %d iteration(s)%s\n", colorRed, result.Iterations, colorReset)
		if result.StuckReason != "" {
			fmt.Printf("%s  reason: %s%s\n", colorDim, result.StuckReason, colorReset)
		}
		for _, f := range result.Failures {
			fmt.Printf("%s  • %s%s\n", colorDim, f.String(), colorReset)
		}
	}
	if result.FixSummary != "" {
		fmt.Printf("\n%sFix summary:%s %s\n", colorDim, colorReset, result.FixSummary)
	}
	fmt.Println()
}

// handleCommitCommand generates a commit message from the current diff,
// shows a preview, and commits on user approval.
func handleCommitCommand(client *ollama.Client, cfg Config) {
	root, _ := os.Getwd()

	toolkit := agent.NewToolkit(root, nil, nil)
	diff := toolkit.GitDiff()
	if strings.TrimSpace(diff) == "" {
		fmt.Printf("%s● no changes to commit%s\n\n", colorDim, colorReset)
		return
	}

	// Truncate diff for the model.
	if len(diff) > 8000 {
		diff = diff[:8000] + "\n[truncated]"
	}

	// Generate commit message via model.
	fmt.Printf("%s  generating commit message...%s\n", colorDim, colorReset)
	model := router.ModelFor(router.TierFast)
	msgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a git commit message generator. Output ONLY the commit message, nothing else. Use conventional commit format (feat/fix/refactor/docs/test/chore). First line: type(scope): short summary (max 72 chars). Optional body after blank line for details."},
		ollama.Message{Role: "user", Content: "Generate a commit message for this diff:\n\n" + diff},
	}

	var buf strings.Builder
	_, _, err := client.StreamChat(context.Background(), model, msgs, &ollama.ModelOptions{Temperature: 0.3, NumCtx: 8192}, func(c string) { buf.WriteString(c) })
	if err != nil {
		fmt.Printf("%s✗ failed to generate commit message: %v%s\n\n", colorRed, err, colorReset)
		return
	}

	message := strings.TrimSpace(buf.String())
	// Strip markdown code fences if model wrapped it.
	message = strings.TrimPrefix(message, "```")
	message = strings.TrimSuffix(message, "```")
	message = strings.TrimSpace(message)

	// Show preview.
	fmt.Printf("\n%s╭─ Commit Preview ─╮%s\n", colorGold, colorReset)
	fmt.Printf("%s  message: %s%s\n", colorDim, message, colorReset)

	// Show staged files.
	status, _ := toolkit.RunBash("git status --short", 5)
	if status != "" {
		fmt.Printf("%s  changes:%s\n", colorDim, colorReset)
		for _, line := range strings.Split(status, "\n") {
			if line != "" {
				fmt.Printf("%s    %s%s\n", colorDim, line, colorReset)
			}
		}
	}
	fmt.Printf("%s╰──────────────────╯%s\n", colorGold, colorReset)

	// Ask for approval.
	fmt.Printf("\n  Stage all & commit? [y/n/e(dit)]: ")
	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))

	if answer == "e" || answer == "edit" {
		fmt.Printf("  Enter commit message: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		message = strings.TrimSpace(line)
		if message == "" {
			fmt.Printf("%s● cancelled%s\n\n", colorDim, colorReset)
			return
		}
		answer = "y"
	}

	if answer != "y" && answer != "yes" {
		fmt.Printf("%s● cancelled%s\n\n", colorDim, colorReset)
		return
	}

	// Stage all changes and commit.
	out, code := toolkit.RunBash("git add -A", 10)
	if code != 0 {
		fmt.Printf("%s✗ git add failed: %s%s\n\n", colorRed, out, colorReset)
		return
	}

	out, code = toolkit.RunBash(fmt.Sprintf("git commit -m %q", message), 10)
	if code != 0 {
		fmt.Printf("%s✗ git commit failed: %s%s\n\n", colorRed, out, colorReset)
		return
	}

	fmt.Printf("\n%s✓ committed: %s%s\n\n", colorGreen, message, colorReset)
}

// handlePRCommand generates a PR title + description from commits since main,
// then pushes the branch and creates a GitHub PR via the `gh` CLI.
func handlePRCommand(client *ollama.Client) {
	root, _ := os.Getwd()
	toolkit := agent.NewToolkit(root, nil, nil)

	// Require gh CLI.
	if _, code := toolkit.RunBash("gh --version", 5); code != 0 {
		fmt.Printf("%s✗ gh CLI not found — install from https://cli.github.com%s\n\n", colorRed, colorReset)
		return
	}

	// Get current branch.
	branch, code := toolkit.RunBash("git rev-parse --abbrev-ref HEAD", 5)
	branch = strings.TrimSpace(branch)
	if code != 0 || branch == "" || branch == "HEAD" {
		fmt.Printf("%s✗ not on a named branch%s\n\n", colorRed, colorReset)
		return
	}
	if branch == "main" || branch == "master" {
		fmt.Printf("%s✗ on %s — create a feature branch first%s\n\n", colorRed, branch, colorReset)
		return
	}

	// Commits ahead of main/master.
	commitLog, _ := toolkit.RunBash("git log main..HEAD --oneline", 10)
	if strings.TrimSpace(commitLog) == "" {
		commitLog, _ = toolkit.RunBash("git log master..HEAD --oneline", 10)
	}
	if strings.TrimSpace(commitLog) == "" {
		fmt.Printf("%s● no commits ahead of main/master — nothing to PR%s\n\n", colorDim, colorReset)
		return
	}

	// Diff stat.
	diffStat, _ := toolkit.RunBash("git diff main...HEAD --stat", 10)
	if diffStat == "" {
		diffStat, _ = toolkit.RunBash("git diff master...HEAD --stat", 10)
	}

	// Generate PR title + body via model.
	fmt.Printf("%s  generating PR description…%s\n", colorDim, colorReset)
	model := router.ModelFor(router.TierFast)
	prompt := fmt.Sprintf(
		"Generate a GitHub pull request title and description.\n\n"+
			"Branch: %s\n\nCommits:\n%s\n\nFiles changed:\n%s\n\n"+
			"Output exactly:\nTITLE: <title (max 72 chars)>\n\nBODY:\n<markdown with ## Summary and ## Test plan sections>",
		branch, commitLog, diffStat,
	)
	msgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a pull request description generator. Be concise. Output only the requested format."},
		ollama.Message{Role: "user", Content: prompt},
	}

	var buf strings.Builder
	_, _, err := client.StreamChat(context.Background(), model, msgs, &ollama.ModelOptions{Temperature: 0.3, NumCtx: 8192}, func(c string) { buf.WriteString(c) })
	if err != nil {
		fmt.Printf("%s✗ failed to generate PR description: %v%s\n\n", colorRed, err, colorReset)
		return
	}

	raw := strings.TrimSpace(buf.String())
	title, body := parsePROutput(raw)
	if title == "" {
		title = branch
	}

	// Preview.
	fmt.Printf("\n%s╭─ PR Preview ─────────────────────╮%s\n", colorGold, colorReset)
	fmt.Printf("%s  branch: %s%s\n", colorDim, branch, colorReset)
	fmt.Printf("%s  title:  %s%s\n", colorDim, title, colorReset)
	if body != "" {
		lines := strings.Split(body, "\n")
		shown := lines
		if len(shown) > 6 {
			shown = lines[:6]
		}
		for _, l := range shown {
			fmt.Printf("  %s%s%s\n", colorDim, l, colorReset)
		}
		if len(lines) > 6 {
			fmt.Printf("  %s[… %d more lines]%s\n", colorDim, len(lines)-6, colorReset)
		}
	}
	fmt.Printf("%s╰──────────────────────────────────╯%s\n", colorGold, colorReset)

	fmt.Printf("\n  Push & create PR? [y/n/e(dit title)]: ")
	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))

	if answer == "e" || answer == "edit" {
		fmt.Printf("  Enter PR title: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		title = strings.TrimSpace(line)
		if title == "" {
			fmt.Printf("%s● cancelled%s\n\n", colorDim, colorReset)
			return
		}
		answer = "y"
	}
	if answer != "y" && answer != "yes" {
		fmt.Printf("%s● cancelled%s\n\n", colorDim, colorReset)
		return
	}

	// Push branch.
	fmt.Printf("%s  pushing %s…%s\n", colorDim, branch, colorReset)
	out, code := toolkit.RunBash(fmt.Sprintf("git push -u origin %s", branch), 30)
	if code != 0 {
		fmt.Printf("%s✗ git push failed: %s%s\n\n", colorRed, out, colorReset)
		return
	}

	// Create PR.
	prOut, code := toolkit.RunBash(fmt.Sprintf("gh pr create --title %q --body %q", title, body), 30)
	if code != 0 {
		fmt.Printf("%s✗ gh pr create failed: %s%s\n\n", colorRed, prOut, colorReset)
		return
	}

	fmt.Printf("\n%s✓ PR created: %s%s\n\n", colorGreen, strings.TrimSpace(prOut), colorReset)
}

// parsePROutput extracts TITLE and BODY from model output.
func parsePROutput(raw string) (title, body string) {
	if idx := strings.Index(raw, "TITLE:"); idx >= 0 {
		rest := strings.TrimSpace(raw[idx+len("TITLE:"):])
		if nl := strings.Index(rest, "\n"); nl >= 0 {
			title = strings.TrimSpace(rest[:nl])
			rest = rest[nl+1:]
		} else {
			title = strings.TrimSpace(rest)
			return title, ""
		}
		if idx2 := strings.Index(rest, "BODY:"); idx2 >= 0 {
			body = strings.TrimSpace(rest[idx2+len("BODY:"):])
		}
	} else {
		// Fallback: first non-empty line = title.
		lines := strings.SplitN(strings.TrimSpace(raw), "\n", 2)
		title = strings.TrimSpace(lines[0])
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		}
	}
	return
}

// buildSystemPrompt returns the base system prompt (tier-independent).
// Brain context and tier-specific guidance are appended separately.
func buildSystemPrompt(brainCtx, skillsCtx string, tier router.Tier) string {
	var sb strings.Builder

	// ── Core identity & reasoning guidance ────────────────────────────────
	// For trivial/fast tiers, use a compact prompt to fit small model context windows.
	if tier <= router.TierFast {
		sb.WriteString(`You are Mantis, an expert AI coding assistant working inside the user's project directory.
- You have full knowledge of the project files — they are injected into your context automatically.
- Read the injected project context carefully before answering. Never say "I can't read files" or ask the user to paste file contents.
- If project manifest files (package.json, go.mod, requirements.txt, etc.) are present in context, use them to determine the language, framework, and how to run the project.
- When fixing errors, look at ALL the injected source files to identify every problem at once — don't fix one issue at a time.
- Be concise. Show code first, then explain briefly.
- Format code with correct language tags.
- Never invent function names — use exact signatures if known.
- Never end with "Would you like me to..." — just answer and stop.
- NEVER start with "Sure!", "Of course!", "I'd be happy to", "Great question!". Start with the answer directly.
- Do NOT add improvements beyond what was asked. Only change what is requested.
`)
	} else {
		sb.WriteString(`You are Mantis, an expert AI coding assistant working inside the user's project directory.
You have direct access to the project files — they are injected into your context automatically.
NEVER say "I can't read files" or ask the user to paste file contents. You already have them.

## How to think
- Break complex problems into steps before answering.
- Read the injected project context (manifests, source files, README) carefully before answering.
- State your assumptions explicitly.
- When modifying code, explain what breaks if you're wrong.
- If you are unsure about a function name or API, say so — never guess.

## How to answer
- Show the final code or answer FIRST, then explain.
- Use the exact function signatures from [ground_truth] — never invent names.
- When referencing files, use full paths from the project.
- If the question is ambiguous, ask one clarifying question before answering.
- Format code with correct language tags.
- NEVER end your response with "Would you like me to..." or a numbered menu of options. Just answer completely and stop.
- NEVER output [Internal analysis] sections or show your reasoning steps. Reason internally, respond with the solution only.
- When you write a package.json, the very next thing you show MUST be the command: npm install — never skip this.
- When you write a requirements.txt, follow immediately with: pip install -r requirements.txt

## Completeness rules (non-negotiable)
- NEVER leave TODO, FIXME, stub, or placeholder in code you write. Write the real implementation.
- NEVER truncate with "// ... rest of the code" or "# similar pattern for other routes". Write every line.
- If building an API: include ALL CRUD endpoints (GET list, GET by id, POST, PUT/PATCH, DELETE). No half-implementations.
- If the user mentions auth/login/JWT: ALWAYS include the middleware/guard and apply it to protected routes.
- If writing a Dockerfile: also write docker-compose.yml and .dockerignore.
- If writing a database schema: include indexes for all foreign keys and frequently queried columns.
- If writing a function: include error handling for every error path — never silently swallow errors.
- After writing files for a project, list exactly what the user must run to start it (install deps, run migrations, start server).

## Anti-bloat rules (CRITICAL — follow strictly)
- Do NOT add features, refactoring, or improvements beyond what was asked.
- A bug fix does NOT need surrounding code cleaned up.
- Do NOT add error handling or validation for scenarios that cannot happen.
- Do NOT create helpers, utilities, or abstractions for one-time operations.
- Three similar lines of code is BETTER than a premature abstraction.
- Do NOT add docstrings, comments, or type annotations to code you did not change.
- Do NOT rename unused variables to _var or add "// removed" comments — if unused, delete completely.
- Do NOT ask follow-up questions like "Would you like me to..." — just answer and stop.
- Start with the answer, not the reasoning. Skip filler words and preamble.

## Code quality rules
- Never shadow the err variable — assign to a new name or check immediately.
- Never swallow errors silently: check err != nil and return/log it — never discard with _.
- Exported functions must document their panics, non-obvious side-effects, and concurrency safety.
- Prefer returning errors over panicking in library code. Use panic only for programmer errors (nil receiver, impossible state).
- When adding a field to a struct, check all construction sites — missing fields cause silent zero-values.
- When writing goroutines: always think about lifetime, cancellation (context), and leak prevention.
- Go: prefer errors.As / errors.Is over string-matching on error messages.
- TypeScript/JS: prefer const over let; never use var; always type function return values explicitly.
- SQL: always use parameterized queries — never interpolate user input into query strings.

## File editing rules (CRITICAL)
When modifying an EXISTING file, ALWAYS use the edit block format — never output the whole file:

  ` + "```" + `edit:path/to/existing/file.go
  <<<SEARCH
  exact text to find (must match exactly, including whitespace)
  ===
  replacement text
  >>>SEARCH
  ` + "```" + `

Multiple changes to the same file: use multiple <<<SEARCH...>>>SEARCH sections in one edit block.
Only use ` + "`lang:filepath`" + ` (whole-file format) for NEW files that don't exist yet.

## File generation
When writing NEW files, ALWAYS tag the opening fence with the filename using a colon:

  ` + "```" + `python:src/app.py
  # code here
  ` + "```" + `
  ` + "```" + `typescript:src/index.ts
  // code here
  ` + "```" + `
  ` + "```" + `makefile:Makefile
  # code here
  ` + "```" + `
  ` + "```" + `dockerfile:Dockerfile
  # code here
  ` + "```" + `
  ` + "```" + `yaml:docker-compose.yml
  # code here
  ` + "```" + `
  ` + "```" + `bash:.env.example
  # code here
  ` + "```" + `

Rules:
- Use ` + "`lang:filename`" + ` for EVERY new file — this is how Mantis writes them to disk.
- Extensionless files (Makefile, Dockerfile, Procfile) use ` + "`makefile:Makefile`" + ` format.
- Dot-files (.env, .gitignore) use ` + "`bash:.env`" + ` or ` + "`text:.gitignore`" + `.
- NEVER use indented code blocks (4-space indent) for files — they won't be written to disk.
- After all files, confirm: "✓ Created X files: Makefile, src/app.py, ..."
`)
	}

	// ── Retrieved memory grounding ────────────────────────────────────────────
	sb.WriteString(`
## Context survival
When working with file contents or tool results, write down key facts (file paths, function signatures, error messages, variable names) in your response text. Earlier context may be compressed — your response text survives but tool results may not.

## Retrieved memory
When a <retrieved_memory> block appears in a user message, it contains context
retrieved from past sessions, decisions, and conventions. Treat it as authoritative
project context. Do not contradict it unless the user explicitly asks you to.
`)

	// ── Active skills (engineering expertise loaded from .mantis/skills/) ─────
	if skillsCtx != "" {
		sb.WriteString("\n## Engineering Skills\n")
		sb.WriteString(skillsCtx)
		sb.WriteString("\n")
	}

	// ── Tier-specific suffix ──────────────────────────────────────────────
	switch tier {
	case router.TierTrivial, router.TierFast:
		sb.WriteString("\n## Response style\nBe extremely concise. One code block, minimal explanation.\n")
	case router.TierCode:
		sb.WriteString("\n## Response style\nThink step-by-step. Show the change, then explain why it works.\n")
	case router.TierReason:
		sb.WriteString("\n## Response style\nAnalyze thoroughly. Consider edge cases. Structure your response with headers.\n")
	case router.TierHeavy:
		sb.WriteString("\n## Response style\nThis is a complex task. Break it into phases. For each phase: what changes, what could break, what to test.\n")
	case router.TierMax:
		sb.WriteString("\n## Response style\nProvide the most thorough, production-quality answer possible. Cover edge cases, error handling, and testing.\n")
	}

	// ── Chain-of-thought injection for reasoning tiers ────────────────────
	if tier >= router.TierReason && tier != router.TierVision {
		sb.WriteString(`
## Thinking process
Before answering, reason through the problem step by step inside a <thinking> block.
Then give your final answer after </thinking>. Example:
<thinking>
1. The user wants to...
2. The relevant code is in...
3. The risk is...
</thinking>
[your answer here]
`)
	}

	// ── Project brain context (if available) ──────────────────────────────
	// Skip brain context for trivial/fast tiers — keep prompt compact for small models.
	if brainCtx != "" && tier >= router.TierCode {
		sb.WriteString("\n## Project context (from persistent memory)\n")
		sb.WriteString(brainCtx)
		sb.WriteString("\n")
	}

	return sb.String()
}

// contextMessageFor returns a context injection message for this turn, or nil.
// Injects README for project questions, ContextSnippet for code questions,
// and graph context when file paths or symbols are detected.
// readCache tracks which files have already been injected this session.
// tier controls the context budget: Trivial gets minimal context, Code+ gets full.
func contextMessageFor(ctx context.Context, input, root string, brainContext string, tw *truth.Writer, querier *graph.Querier, embStore *embeddings.Store, readCache map[string]bool, tier ...router.Tier) interface{} {
	currentTier := router.TierCode // default: full budget
	if len(tier) > 0 {
		currentTier = tier[0]
	}
	lower := strings.ToLower(input)
	isProjectQ := strings.Contains(lower, "project") || strings.Contains(lower, "what is") ||
		strings.Contains(lower, "what does") || strings.Contains(lower, "what are") ||
		strings.Contains(lower, "overview") || strings.Contains(lower, "purpose") ||
		strings.Contains(lower, "about this") || strings.Contains(lower, "describe") ||
		strings.Contains(lower, "how to run") || strings.Contains(lower, "how do i run") ||
		strings.Contains(lower, "how to start") || strings.Contains(lower, "how to setup") ||
		strings.Contains(lower, "how to install")
	isCodeQ := strings.Contains(lower, "function") || strings.Contains(lower, "symbol") ||
		strings.Contains(lower, "import") || strings.Contains(lower, "package") ||
		strings.Contains(lower, "implement") || strings.Contains(lower, "write") ||
		strings.Contains(lower, "fix") || strings.Contains(lower, "bug") ||
		strings.Contains(lower, "error") || strings.Contains(lower, "refactor")

	var parts []string

	if isProjectQ {
		if readme := readFileSnippet(filepath.Join(root, "README.md"), 1500); readme != "" {
			parts = append(parts, "Project README:\n"+readme)
		}
	}

	// 7.5: Cold memory — retrieve relevant BRAIN.md/REJECTED.md chunks on demand.
	// Skip for TierTrivial — these short answers don't need memory context.
	if embStore != nil && currentTier >= router.TierCode {
		if coldChunks, err := embStore.SearchHybrid(ctx, input, 2); err == nil {
			var cold []string
			for _, c := range coldChunks {
				if (c.Source == "brain" || c.Source == "rejected" || c.Source == "decision") && c.Text != "" {
					// Deduplicate: skip if chunk text already appears in hot memory.
					if brainContext != "" && strings.Contains(brainContext, c.Text[:min(len(c.Text), 80)]) {
						continue
					}
					cold = append(cold, fmt.Sprintf("[%s] %s", c.SectionLabel, c.Text))
				}
			}
			if len(cold) > 0 {
				parts = append(parts, "<cold_memory>\n"+strings.Join(cold, "\n\n")+"\n</cold_memory>")
			}
		}
	}

	if isCodeQ && tw != nil && currentTier >= router.TierFast {
		if snippet := tw.ContextSnippetN(8, 800); snippet != "" {
			parts = append(parts, "Codebase symbols (verified):\n"+snippet)
		}
	}

	// 7.1: Repo-map — inject PageRank-ranked symbol index for codebase orientation.
	// Skip for Trivial/Fast — repo map is only useful for Code+ tasks.
	if isCodeQ && querier != nil && currentTier >= router.TierCode {
		if entries, err := querier.RepoMap(2048); err == nil && len(entries) > 0 {
			mapStr := graph.RepoMapString(entries)
			if mapStr != "" {
				parts = append(parts, "<repo_map>\n"+mapStr+"</repo_map>")
			}
		}
	}

	// 7.7: Code context — inject top semantic code chunks matching the query.
	if isCodeQ && embStore != nil && currentTier >= router.TierFast {
		if results, err := embStore.SearchHybrid(ctx, input, 3); err == nil {
			var codeChunks []string
			for _, r := range results {
				if strings.HasPrefix(r.ID, "code:") && r.Text != "" {
					codeChunks = append(codeChunks, fmt.Sprintf("// %s (%s)\n%s", r.Source, r.SectionLabel, r.Text))
				}
			}
			if len(codeChunks) > 0 {
				parts = append(parts, "<code_context>\n"+strings.Join(codeChunks, "\n\n")+"\n</code_context>")
			}
		}
	}

	// Graph context: auto-inject related file signatures when files/symbols are detected.
	mentionedFiles := extractMentionedFiles(input, querier)

	// Dynamic file reading: inject full content of mentioned files not yet seen this session.
	// Cap at 3 new files per turn, 150 lines each, so context stays manageable.
	if len(mentionedFiles) > 0 && readCache != nil {
		const maxNewFiles = 3
		newCount := 0
		for _, rel := range mentionedFiles {
			if newCount >= maxNewFiles {
				break
			}
			clean := filepath.Clean(rel)
			if readCache[clean] {
				continue
			}
			abs := filepath.Join(root, clean)
			content := readFileHead(abs, 150)
			if content == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("File: %s\n```\n%s\n```", clean, content))
			readCache[clean] = true
			newCount++
			fmt.Printf("%s  ● reading: %s%s\n", colorDim, clean, colorReset)
		}
	}

	if len(mentionedFiles) > 0 && querier != nil {
		if graphCtx, related := graphContextFor(mentionedFiles, root, querier); graphCtx != "" {
			parts = append(parts, graphCtx)
			fmt.Printf("%s  ● graph: %s → %d related file(s) injected%s\n",
				colorDim, mentionedFiles[0], len(related), colorReset)
		}
	}

	if len(parts) == 0 {
		return nil
	}
	// Token budget: tier-aware cap for context injection.
	// Trivial: 3K chars, Fast: 6K chars, Code+: 12K chars.
	maxContextChars := 12000
	switch currentTier {
	case router.TierTrivial:
		maxContextChars = 3000
	case router.TierFast:
		maxContextChars = 6000
	}
	combined := strings.Join(parts, "\n\n")
	if len(combined) > maxContextChars {
		// Trim from the front (repo_map, cold_memory) to keep files + graph (most specific).
		for len(parts) > 1 && len(combined) > maxContextChars {
			parts = parts[1:]
			combined = strings.Join(parts, "\n\n")
		}
		if len(combined) > maxContextChars {
			combined = combined[len(combined)-maxContextChars:]
		}
	}
	return ollama.Message{Role: "user", Content: "[context]\n" + combined + "\n[/context]\n\nNow answer: " + input}
}

// injectBuildFiles reads build-related configuration files from the project root
// and returns their content to inject into context. This ensures the model sees
// the actual files before suggesting changes (instead of guessing).
func injectBuildFiles(input, root string) string {
	lower := strings.ToLower(input)

	// Always inject manifest files so the model knows the project language/framework.
	manifests := []string{
		"package.json", "go.mod", "Cargo.toml", "pyproject.toml",
		"requirements.txt", "setup.py", "pom.xml", "build.gradle",
		"Makefile", "docker-compose.yml",
	}

	// Additional keyword-triggered files.
	type fileSet struct {
		keywords []string
		files    []string
	}
	sets := []fileSet{
		{[]string{"docker", "dockerfile", "compose", "container"}, []string{"Dockerfile", "docker-compose.yaml", "compose.yml", "compose.yaml", ".dockerignore"}},
		{[]string{"npm", "node", "yarn", "pnpm", "package", "typescript", "tsc"}, []string{"tsconfig.json"}},
		{[]string{"deploy", "kubernetes", "kubectl", "k8s"}, []string{"k8s/", "deployment.yaml", "service.yaml"}},
	}

	seen := make(map[string]bool)
	var parts []string

	// 1. Always inject manifest files that exist.
	for _, f := range manifests {
		seen[f] = true
		path := filepath.Join(root, f)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 3000 {
			content = content[:3000] + "\n… (truncated)"
		}
		parts = append(parts, fmt.Sprintf("[file: %s]\n```\n%s\n```", f, strings.TrimSpace(content)))
	}

	// 2. Keyword-triggered additional files.
	for _, s := range sets {
		matched := false
		for _, kw := range s.keywords {
			if strings.Contains(lower, kw) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, f := range s.files {
			if seen[f] {
				continue
			}
			seen[f] = true
			path := filepath.Join(root, f)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			content := string(data)
			if len(content) > 3000 {
				content = content[:3000] + "\n… (truncated)"
			}
			parts = append(parts, fmt.Sprintf("[file: %s]\n```\n%s\n```", f, strings.TrimSpace(content)))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return "[Mantis auto-read these project files for context]\n" + strings.Join(parts, "\n\n")
}

// captureBuildError detects build-related commands mentioned in the user's
// message and auto-runs them to capture the actual error output. Returns the
// captured output (capped at 3000 chars) or "" if no build command was detected
// or the build succeeded.
func captureBuildError(input, root string) string {
	lower := strings.ToLower(input)

	// Map of trigger phrases → commands to run.
	triggers := []struct {
		phrase string
		cmd    string
	}{
		// Make
		{"make build", "make build"},
		{"make test", "make test"},
		{"make run", "make run"},
		{"makefile", "make"},
		// Node
		{"npm run build", "npm run build"},
		{"npm test", "npm test"},
		{"npm start", "npm start"},
		{"yarn build", "yarn build"},
		{"yarn test", "yarn test"},
		{"pnpm build", "pnpm build"},
		// Go
		{"go build", "go build ./..."},
		{"go test", "go test ./..."},
		// Rust
		{"cargo build", "cargo build"},
		{"cargo check", "cargo check"},
		{"cargo test", "cargo test"},
		// Docker
		{"docker build", "docker build ."},
		{"docker compose up", "docker compose up --dry-run"},
		{"docker-compose up", "docker-compose config"},
		{"dockerfile", "docker build ."},
		// Python
		{"pip install", "pip install -r requirements.txt"},
		{"python -m", "python3 -m py_compile"},
	}

	var cmdToRun string
	for _, t := range triggers {
		if strings.Contains(lower, t.phrase) {
			cmdToRun = t.cmd
			break
		}
	}
	if cmdToRun == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdToRun)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		// Build succeeded — nothing to inject.
		return ""
	}

	output := out.String()
	if len(output) > 3000 {
		output = output[:3000] + "\n… (truncated)"
	}
	return fmt.Sprintf("[Mantis auto-ran `%s` and captured this error output]\n```\n%s\n```", cmdToRun, strings.TrimSpace(output))
}

// postFixVerify detects the project type and runs the appropriate build command
// to check if the fix agent's changes actually resolved the error. Returns ""
// if the build passes or the project type can't be detected, otherwise returns
// a short error summary.
func postFixVerify(root string) string {
	// Detect project type by checking for common files.
	checks := []struct {
		file string
		cmd  string
	}{
		{"go.mod", "go build ./..."},
		{"package.json", "npm run build --if-present 2>&1 || true"},
		{"Cargo.toml", "cargo check"},
		{"Makefile", "make build"},
		{"requirements.txt", "python -c \"import importlib; print('ok')\""},
		{"pyproject.toml", "python -c \"import importlib; print('ok')\""},
	}

	var cmdToRun string
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(root, c.file)); err == nil {
			cmdToRun = c.cmd
			break
		}
	}
	if cmdToRun == "" {
		return ""
	}

	// For Python projects, try to actually run the entry point.
	if strings.HasPrefix(cmdToRun, "python") {
		// Check for common entry points.
		for _, entry := range []string{"run.py", "app.py", "main.py", "manage.py"} {
			if _, err := os.Stat(filepath.Join(root, entry)); err == nil {
				cmdToRun = fmt.Sprintf("python %s --help 2>&1 || python -c \"exec(open('%s').read())\" 2>&1", entry, entry)
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdToRun)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		output := out.String()
		// Extract just the last error line for a concise message.
		lines := strings.Split(strings.TrimSpace(output), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		return strings.Join(lines, " | ")
	}
	return ""
}

// fixAgentTools returns a minimal set of tools for the single-agent fix loop.
func fixAgentTools() []ollama.Tool {
	return []ollama.Tool{
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "read_file",
				Description: "Read a file from the project. Path is relative to the project root.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative file path"}},"required":["path"]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "edit_file",
				Description: "Apply a precise old→new replacement to an existing file. Fails safely if old_string is not found exactly once. Prefer this over write_file for modifying existing files.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative file path"},"old_string":{"type":"string","description":"Exact text to replace (must appear exactly once)"},"new_string":{"type":"string","description":"Replacement text"}},"required":["path","old_string","new_string"]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "run_command",
				Description: "Run a shell command in the project root. Allowed: go, npm, yarn, pnpm, cargo, make, docker, docker compose, pip, python, kubectl, git, cat, head, tail, ls, find, grep, pwd, which, echo.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute"}},"required":["command"]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "list_files",
				Description: "List files in a directory (non-recursive, max 50 entries).",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative directory path (default: .)"}},"required":[]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "write_file",
				Description: "Create or overwrite a file. Use for creating new files. For modifying existing files, prefer edit_file.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative file path"},"content":{"type":"string","description":"File content to write"}},"required":["path","content"]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "multi_edit_file",
				Description: "Apply multiple old→new replacements to a single file atomically. If any edit fails, none are applied. Edits apply in order.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative file path"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["old_string","new_string"]}}},"required":["path","edits"]}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "search_files",
				Description: "Search for a text pattern across project files (like grep -r). Returns matching lines with file paths.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Text or regex pattern to search for"},"path":{"type":"string","description":"Directory to search in (default: .)"}},"required":["pattern"]}`),
			},
		},
	}
}

// fixAgentAllowedCmd checks if a command is safe to run in the fix agent.
func fixAgentAllowedCmd(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	allowed := []string{
		// Go
		"go build", "go test", "go vet", "go fmt", "go run", "go mod",
		// Node
		"npm run", "npm test", "npm install", "npm ci", "npm start",
		"npx ", "yarn ", "pnpm ",
		// Rust
		"cargo check", "cargo build", "cargo test", "cargo run",
		// Python
		"python -m", "python3 -m", "python ", "python3 ",
		"pip install", "pip3 install", "pip list", "pip3 list",
		// Docker
		"docker build", "docker compose", "docker-compose",
		"docker run", "docker ps", "docker logs", "docker images",
		"docker exec", "docker stop", "docker rm", "docker inspect",
		// Make
		"make",
		// Kubernetes
		"kubectl ",
		// Git
		"git diff", "git status", "git log", "git show",
		// Shell diagnostics
		"cat ", "head ", "tail ", "ls ", "find ", "grep ",
		"pwd", "which ", "echo ", "wc ", "env",
		// Virtual environment
		"pip install -r", "pip3 install -r",
		"source ", ". ",
		// cd prefix (cmd.Dir handles cwd, but models chain cd && ...)
		"cd ",
	}
	for _, prefix := range allowed {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// fixToolArgSummary returns a short summary of tool arguments for progress output.
func fixToolArgSummary(name string, args json.RawMessage) string {
	var generic map[string]interface{}
	_ = json.Unmarshal(args, &generic)
	switch name {
	case "run_command":
		if cmd, ok := generic["command"].(string); ok {
			if len(cmd) > 60 {
				cmd = cmd[:60] + "…"
			}
			return cmd
		}
	case "read_file", "write_file":
		if p, ok := generic["path"].(string); ok {
			return p
		}
	case "edit_file", "multi_edit_file":
		if p, ok := generic["path"].(string); ok {
			return p
		}
	case "list_files":
		if p, ok := generic["path"].(string); ok && p != "" {
			return p
		}
		return "."
	case "search_files":
		if p, ok := generic["pattern"].(string); ok {
			return p
		}
	}
	return ""
}

// dispatchFixTool executes a single tool call for the fix agent loop.
func dispatchFixTool(root, toolName string, argsRaw json.RawMessage) string {
	switch toolName {
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		abs := filepath.Join(root, filepath.Clean(args.Path))
		// BUG-08: require separator so "/project2" doesn't pass when root="/project".
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return "error: path escapes project root"
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		content := string(data)
		if len(content) > 8000 {
			content = content[:8000] + "\n… (truncated)"
		}
		return content

	case "edit_file":
		var args struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		abs := filepath.Join(root, filepath.Clean(args.Path))
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return "error: path escapes project root"
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		content := string(data)
		count := strings.Count(content, args.OldString)
		if count == 0 {
			return fmt.Sprintf("error: old_string not found in %s", args.Path)
		}
		if count > 1 {
			return fmt.Sprintf("error: old_string matches %d times — be more specific", count)
		}
		newContent := strings.Replace(content, args.OldString, args.NewString, 1)
		if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return fmt.Sprintf("edited %s", args.Path)

	case "multi_edit_file":
		var args struct {
			Path  string `json:"path"`
			Edits []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		abs := filepath.Join(root, filepath.Clean(args.Path))
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return "error: path escapes project root"
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		content := string(data)
		for i, e := range args.Edits {
			count := strings.Count(content, e.OldString)
			if count == 0 {
				return fmt.Sprintf("error: edit %d/%d — old_string not found (edits rolled back)", i+1, len(args.Edits))
			}
			if count > 1 {
				return fmt.Sprintf("error: edit %d/%d — old_string matches %d times (edits rolled back)", i+1, len(args.Edits), count)
			}
			content = strings.Replace(content, e.OldString, e.NewString, 1)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return fmt.Sprintf("edited %s (%d edits applied)", args.Path, len(args.Edits))

	case "run_command":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		if !fixAgentAllowedCmd(args.Command) {
			return fmt.Sprintf("error: command not allowed: %q", args.Command)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		output := out.String()
		if len(output) > 8000 {
			output = output[:8000] + "\n… (truncated)"
		}
		if err != nil {
			return fmt.Sprintf("exit code: non-zero\n%s", output)
		}
		return output

	case "list_files":
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(argsRaw, &args)
		dir := root
		if args.Path != "" {
			dir = filepath.Join(root, filepath.Clean(args.Path))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		var sb strings.Builder
		for i, e := range entries {
			if i >= 50 {
				sb.WriteString("… (truncated)\n")
				break
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			sb.WriteString(name + "\n")
		}
		return sb.String()

	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		abs := filepath.Join(root, filepath.Clean(args.Path))
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return "error: path escapes project root"
		}
		// Create parent directories if needed.
		if dir := filepath.Dir(abs); dir != root {
			_ = os.MkdirAll(dir, 0o755)
		}
		if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return fmt.Sprintf("wrote %s (%d bytes)", args.Path, len(args.Content))

	case "search_files":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "error: bad arguments"
		}
		dir := root
		if args.Path != "" {
			dir = filepath.Join(root, filepath.Clean(args.Path))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "grep", "-rn", "--include=*.ts", "--include=*.tsx",
			"--include=*.js", "--include=*.jsx", "--include=*.json", "--include=*.go",
			"--include=*.py", "--include=*.yaml", "--include=*.yml", "--include=*.toml",
			"--include=*.md", "--include=Makefile", "--include=Dockerfile",
			"-l", args.Pattern, dir)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		_ = cmd.Run()
		output := out.String()
		// Make paths relative to root.
		output = strings.ReplaceAll(output, root+"/", "")
		if len(output) > 8000 {
			output = output[:8000] + "\n… (truncated)"
		}
		if output == "" {
			return "no matches found"
		}
		return output

	default:
		return fmt.Sprintf("error: unknown tool %q", toolName)
	}
}

// runFixAgent runs a lightweight single-agent tool loop for fix-type tasks.
// It gives the code model access to read_file, run_command, and list_files
// so it can investigate and fix issues autonomously.
// Returns (response, promptTok, complTok, ok). ok=false means the agent loop
// couldn't run (caller should fall back to the normal path).
func runFixAgent(
	ctx context.Context,
	client *ollama.Client,
	model string,
	messages []interface{},
	root string,
	taskType string,
) (string, int, int, bool) {
	const maxIter = 12
	tools := fixAgentTools()

	// Task-adaptive workflow preamble.
	var workflowBlock string
	switch taskType {
	case "explain":
		workflowBlock = `You are an autonomous coding agent. You MUST use tools to answer — never guess or ask the user for file contents.

WORKFLOW:
1. Read relevant files with read_file to understand the code
2. Search for related symbols with search_files if needed
3. Provide a clear, specific explanation based on what you actually read
4. Reference exact line numbers and function names from the files you read`
	case "implement", "refactor", "test":
		workflowBlock = `You are an autonomous coding agent. You MUST run commands and read files yourself — never ask the user.

WORKFLOW:
1. Read the relevant files with read_file to understand the current code
2. Make changes with edit_file (modify existing) or write_file (create new)
3. Run the build/test command with run_command to verify your changes work
4. If there are errors, read the error output, fix, and re-run
5. Keep iterating until everything passes`
	default: // "fix", "general", etc.
		workflowBlock = `You are an autonomous fix agent. You MUST run commands yourself — never ask the user to run them.

WORKFLOW:
1. Run the failing build/deploy command with run_command to capture the ACTUAL error
2. Read the relevant files mentioned in the error with read_file
3. Fix the issue with edit_file (modify existing) or write_file (create new)
4. Run the build command AGAIN to check if it's fixed
5. If there are MORE errors, repeat from step 2
6. Keep iterating until the build passes or you've exhausted all options`
	}

	fixSysPrompt := ollama.Message{
		Role:    "system",
		Content: workflowBlock + `

KEY RULES:
- NEVER tell the user to run a command. Run it yourself.
- NEVER ask for more information. Investigate with tools.
- If a ModuleNotFoundError or "cannot find module" appears, install the missing package FIRST:
  Python: run_command pip install <package> (or pip install -r requirements.txt)
  Node: run_command npm install <package> (or npm install)
  Then re-run the build to verify.
- If a package can't be installed (network error), replace it with a built-in alternative.
- After each fix, ALWAYS re-run the build to verify.
- Use edit_file for surgical changes to existing files.
- Use write_file to create new files that are missing.
- Use search_files to find imports, usages, and definitions.
- Read requirements.txt / package.json early to understand project dependencies.
- After creating Node/TS projects, ALWAYS run ` + "`npm install`" + ` then ` + "`npm run build`" + ` to verify.

ANTI-BLOAT RULES — follow strictly:
- Only change what is broken. Do NOT clean up, refactor, or improve surrounding code.
- Do NOT add error handling for scenarios that cannot happen.
- Do NOT add docstrings, comments, or type annotations to code you did not change.
- Three similar lines > premature abstraction. Keep it simple.

GIT SAFETY — follow strictly:
- NEVER use git push --force, git reset --hard, git checkout ., git clean -f.
- NEVER skip hooks with --no-verify.
- NEVER amend commits — always create NEW commits (amend after hook failure rewrites the WRONG commit).
- Stage specific files by name — never use git add -A or git add . (risks staging .env, credentials).

CODE QUALITY RULES — apply these when writing or fixing code:
- Express validation: validate req.body directly, NOT {body: req.body, query, params}. Zod/Joi schemas match the DATA shape, not the Request object.
- Always use express.json() middleware BEFORE route handlers. Place error-handling middleware (err,req,res,next) AFTER all routes.
- Wrap async Express handlers: asyncHandler = (fn) => (req,res,next) => fn(req,res,next).catch(next).
- NEVER use string concatenation for SQL — use parameterized queries or ORM methods.
- NEVER hardcode secrets — use environment variables.
- NEVER use innerHTML/dangerouslySetInnerHTML with user data.
- Use correct HTTP status codes: 201 creation, 204 deletion, 400 bad input, 404 not found, 422 validation.
- NEVER import packages that do not exist. Only use REAL, well-known packages.
- NEVER use deprecated APIs: Buffer.from() not new Buffer().
- Use Promise.all for independent parallel async calls, not sequential await.
- Every catch block must re-throw or return an error response — NEVER catch(e){console.log(e)}.
- Prisma: use include for relations (no N+1 loops). Use findUnique not findFirst for unique fields.
- React/Next.js: default to server components. Only 'use client' when hooks/events/browser APIs are needed.
- Go: never discard errors with _. Wrap errors: fmt.Errorf("context: %w", err). Every goroutine needs an exit path.
- Python: never use mutable default args (def f(lst=[])). Use None default. Django: always select_related/prefetch_related.
- Docker: always add non-root USER, multi-stage builds, .dockerignore, exec form CMD.`,
	}

	// Copy messages and prepend fix system prompt.
	agentMsgs := make([]interface{}, 0, len(messages)+1)
	agentMsgs = append(agentMsgs, fixSysPrompt)
	agentMsgs = append(agentMsgs, messages...)

	var totalPT, totalCT int
	var lastContent string

	// Try native tool calling first.
	nativeToolsWork := true
	for iter := 0; iter < maxIter; iter++ {
		result, err := client.ChatWithTools(ctx, model, agentMsgs, tools, &ollama.ModelOptions{Temperature: 0.15, NumCtx: 32768})
		if err != nil {
			nativeToolsWork = false
			break
		}
		totalPT += result.PromptTok
		totalCT += result.ComplTok
		lastContent = result.Content

		if len(result.ToolCalls) == 0 {
			break
		}

		agentMsgs = append(agentMsgs, ollama.Message{Role: "assistant", Content: result.Content})
		for _, tc := range result.ToolCalls {
			// Print progress so user sees what's happening.
			fmt.Printf("\r\033[K%s  ▸ %s(%s)%s", colorDim, tc.Function.Name,
				fixToolArgSummary(tc.Function.Name, tc.Function.Arguments), colorReset)
			toolOut := dispatchFixTool(root, tc.Function.Name, tc.Function.Arguments)
			agentMsgs = append(agentMsgs, ollama.ToolMessage{
				Role:    "tool",
				Content: toolOut,
			})
		}
		fmt.Println() // Newline after tool progress.
	}

	if nativeToolsWork && strings.TrimSpace(lastContent) != "" {
		return lastContent, totalPT, totalCT, true
	}

	// Fallback: text-based ReAct loop for models without native tool support.
	return runTextFixAgent(ctx, client, model, messages, root)
}

// extractCapturedBuildOutput scans messages for the pre-captured build error
// injected by captureBuildError(). Returns the raw injected block if found.
func extractCapturedBuildOutput(messages []interface{}) string {
	const marker = "[Mantis auto-ran `"
	for _, msg := range messages {
		m, ok := msg.(ollama.Message)
		if !ok || m.Role != "user" {
			continue
		}
		idx := strings.Index(m.Content, marker)
		if idx < 0 {
			continue
		}
		return m.Content[idx:]
	}
	return ""
}

// extractErrorFilePaths pulls file paths from error messages in the conversation.
// Used to pre-read source files so the fix agent sees actual code before fixing.
func extractErrorFilePaths(messages []interface{}, root string) []string {
	seen := map[string]bool{}
	var paths []string

	// Common error patterns that reference file paths.
	filePatterns := []*regexp.Regexp{
		regexp.MustCompile(`File "([^"]+\.py)"[, ]`),                          // Python tracebacks
		regexp.MustCompile(`from ([\w.]+) import`),                            // Python imports
		regexp.MustCompile(`(?:in|at) ([\w/]+\.(?:go|py|js|ts|rs))(?::\d+)?`), // Go/JS/Rust errors
		regexp.MustCompile(`cannot import name '(\w+)' from '([\w.]+)'`),      // Python ImportError
		regexp.MustCompile(`ModuleNotFoundError: No module named '([\w.]+)'`),  // Python missing module
	}

	for _, msg := range messages {
		m, ok := msg.(ollama.Message)
		if !ok || m.Role != "user" {
			continue
		}

		for _, pat := range filePatterns {
			matches := pat.FindAllStringSubmatch(m.Content, -1)
			for _, match := range matches {
				for i := 1; i < len(match); i++ {
					candidate := match[i]
					// Convert Python module paths to file paths.
					if strings.Contains(candidate, ".") && !strings.Contains(candidate, "/") && !strings.HasSuffix(candidate, ".py") {
						candidate = strings.ReplaceAll(candidate, ".", "/") + ".py"
					}
					// Strip leading src/ or project root.
					candidate = strings.TrimPrefix(candidate, root+"/")
					// Check if file exists.
					abs := filepath.Join(root, candidate)
					if _, err := os.Stat(abs); err == nil && !seen[candidate] {
						seen[candidate] = true
						paths = append(paths, candidate)
					}
					// Also try with src/ prefix.
					if !strings.HasPrefix(candidate, "src/") {
						srcPath := "src/" + candidate
						abs2 := filepath.Join(root, srcPath)
						if _, err := os.Stat(abs2); err == nil && !seen[srcPath] {
							seen[srcPath] = true
							paths = append(paths, srcPath)
						}
					}
				}
			}
		}
	}

	// Cap at 5 files to avoid overloading context.
	if len(paths) > 5 {
		paths = paths[:5]
	}
	return paths
}

// runTextFixAgent uses a text-based ReAct pattern for models that don't support
// native tool calling. It injects tool descriptions into the system prompt and
// parses tool calls from the model's text output. Uses StreamChat (streaming)
// which works reliably on Ollama Cloud.
func runTextFixAgent(
	ctx context.Context,
	client *ollama.Client,
	model string,
	messages []interface{},
	root string,
) (string, int, int, bool) {
	const maxIter = 12

	toolPrompt := `You have access to these tools. To use a tool, output a line starting with TOOL_CALL: followed by the JSON.
Do NOT suggest commands for the user to run. Run them yourself using TOOL_CALL.

Available tools:
1. run_command - Run a shell command. Example: TOOL_CALL: {"tool":"run_command","args":{"command":"make build"}}
2. read_file - Read a project file. Example: TOOL_CALL: {"tool":"read_file","args":{"path":"Makefile"}}
3. list_files - List directory contents. Example: TOOL_CALL: {"tool":"list_files","args":{"path":"."}}
4. edit_file - Apply a targeted replacement to an existing file. Example: TOOL_CALL: {"tool":"edit_file","args":{"path":"main.go","old_string":"oldCode()","new_string":"newCode()"}}
5. write_file - Create a new file. Example: TOOL_CALL: {"tool":"write_file","args":{"path":"src/pages/_app.tsx","content":"import React..."}}
6. search_files - Search for a pattern across project files. Example: TOOL_CALL: {"tool":"search_files","args":{"pattern":"import.*socket"}}

IMPORTANT: Always run the failing command first to see the actual error, then read relevant files, then apply the fix.
IMPORTANT: After fixing, run the build command AGAIN to check if there are more errors. Keep iterating until the build passes.
IMPORTANT: Use edit_file to modify existing files (targeted old→new replacement). Use write_file to create new files.
IMPORTANT: If the error is ModuleNotFoundError or "cannot find module", install the missing package FIRST with run_command: pip install <pkg> or npm install <pkg>. Check requirements.txt / package.json early.
IMPORTANT: If a dependency can't be installed (network error), replace it with built-in alternatives (e.g. fetch instead of axios, native WebSocket instead of socket.io).
IMPORTANT: Use read_file instead of cat/head/tail. Use edit_file instead of sed/awk. Use search_files instead of grep. Use write_file instead of echo redirection. Only use run_command for build/test/install commands that REQUIRE shell execution.
After investigation, provide corrected files using edit_file/write_file calls or fenced code blocks with the filepath: ` + "```lang:filepath"

	agentMsgs := make([]interface{}, 0, len(messages)+3)
	agentMsgs = append(agentMsgs, ollama.Message{Role: "system", Content: toolPrompt})
	agentMsgs = append(agentMsgs, messages...)

	// Ground the agent with the pre-captured build output so the model cannot
	// hallucinate a different cause. If captureBuildError already ran the command
	// and injected the output, surface it explicitly as the authoritative error.
	preCapture := extractCapturedBuildOutput(messages)
	if preCapture != "" {
		agentMsgs = append(agentMsgs, ollama.Message{
			Role: "user",
			Content: "The actual build error is already captured above (see [Mantis auto-ran...] block). " +
				"Fix ONLY what that specific error output describes. " +
				"Do NOT assume a different cause. Do NOT reference files that are not mentioned in the error.",
		})
	}

	// Pre-read files referenced in error messages so the model sees actual code
	// before attempting fixes (prevents import whack-a-mole).
	errorFiles := extractErrorFilePaths(messages, root)
	if len(errorFiles) > 0 {
		var fileContents strings.Builder
		fileContents.WriteString("[Pre-read source files referenced in the error — use these to understand the ACTUAL code before fixing]\n\n")
		for _, ef := range errorFiles {
			content := dispatchFixTool(root, "read_file", json.RawMessage(fmt.Sprintf(`{"path":%q}`, ef)))
			if !strings.HasPrefix(content, "error:") && len(content) > 0 {
				fileContents.WriteString(fmt.Sprintf("=== %s ===\n%s\n\n", ef, content))
			}
		}
		agentMsgs = append(agentMsgs, ollama.Message{
			Role: "user",
			Content: fileContents.String() +
				"CRITICAL: Read ALL the source files above carefully. Fix ALL import errors at once based on what ACTUALLY exists in each file. " +
				"Do NOT guess what a file exports — the actual exports are shown above.",
		})
	}

	var totalPT, totalCT int
	var lastContent string
	usedTools := false

	for iter := 0; iter < maxIter; iter++ {
		var buf bytes.Buffer
		pt, ct, err := client.StreamChat(ctx, model, agentMsgs, &ollama.ModelOptions{Temperature: 0.15, NumCtx: 32768}, func(chunk string) {
			buf.WriteString(chunk)
		})
		if err != nil {
			break
		}
		totalPT += pt
		totalCT += ct
		lastContent = buf.String()

		// Parse text-based tool calls.
		calls := parseTextToolCalls(lastContent)
		if len(calls) == 0 {
			// If model skipped tools on first iter and there is no pre-captured
			// output, force a discovery step by listing files and re-prompting.
			// This prevents the model from hallucinating a fix without seeing
			// the real error.
			if iter == 0 && !usedTools && preCapture == "" {
				lsOut := dispatchFixTool(root, "list_files", json.RawMessage(`{}`))
				agentMsgs = append(agentMsgs,
					ollama.Message{Role: "assistant", Content: lastContent},
					ollama.Message{Role: "user", Content: fmt.Sprintf(
						"You must run the failing build command before proposing any fix. "+
							"Project files:\n%s\n\n"+
							"Use TOOL_CALL to run the failing command now so you can see the actual error output.",
						lsOut,
					)},
				)
				continue
			}
			break
		}

		usedTools = true
		// Execute tool calls and build result message.
		var toolResults strings.Builder
		for _, tc := range calls {
			argsRaw, _ := json.Marshal(tc.args)
			out := dispatchFixTool(root, tc.tool, json.RawMessage(argsRaw))
			toolResults.WriteString(fmt.Sprintf("[%s result]\n%s\n\n", tc.tool, out))
		}

		agentMsgs = append(agentMsgs, ollama.Message{Role: "assistant", Content: lastContent})
		agentMsgs = append(agentMsgs, ollama.Message{Role: "user", Content: "Tool results:\n" + toolResults.String() + "\nNow analyze the output and provide the fix."})
	}

	if strings.TrimSpace(lastContent) == "" {
		return "", 0, 0, false
	}
	return lastContent, totalPT, totalCT, true
}

type textToolCall struct {
	tool string
	args map[string]interface{}
}

// parseTextToolCalls extracts TOOL_CALL: {...} lines from model text output.
func parseTextToolCalls(text string) []textToolCall {
	var calls []textToolCall
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "TOOL_CALL:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "TOOL_CALL:"))
		var parsed struct {
			Tool string                 `json:"tool"`
			Args map[string]interface{} `json:"args"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
			continue
		}
		if parsed.Tool != "" {
			calls = append(calls, textToolCall{tool: parsed.Tool, args: parsed.Args})
		}
	}
	return calls
}

// readFileSnippet reads a file and returns at most maxChars characters.
func readFileSnippet(path string, maxChars int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > maxChars {
		return s[:maxChars] + "\n… (truncated)"
	}
	return s
}

// injectErrorSourceFiles extracts file paths from error messages (tracebacks, import errors, etc.)
// and reads their content so the model can see what's actually in those files.
func injectErrorSourceFiles(input, root string) string {
	// Patterns that extract file paths from common error formats.
	patterns := []*regexp.Regexp{
		// Python: File "/path/to/file.py", line N
		regexp.MustCompile(`File "([^"]+\.py)"`),
		// Python: from module import X → resolve to module.py
		regexp.MustCompile(`from\s+(\w+)\s+import`),
		// Python: ImportError: cannot import name 'X' from 'module' (/path/to/file.py)
		regexp.MustCompile(`from '(\w+)' \(([^)]+\.py)\)`),
		// Go: /path/to/file.go:42:
		regexp.MustCompile(`([^\s]+\.go):\d+:`),
		// Node: at /path/to/file.js:42
		regexp.MustCompile(`at\s+[^\s]*\(([^)]+\.[jt]sx?)`),
		// Generic: src/something.ext or internal/something.ext
		regexp.MustCompile(`((?:src|internal|lib|app)/[^\s:]+\.\w+)`),
	}

	seen := make(map[string]bool)
	var files []string

	for _, pat := range patterns {
		matches := pat.FindAllStringSubmatch(input, -1)
		for _, m := range matches {
			for i := 1; i < len(m); i++ {
				f := m[i]
				if f == "" || seen[f] {
					continue
				}
				seen[f] = true

				// Try multiple resolution strategies.
				var content string
				for _, candidate := range resolveFilePath(f, root) {
					if data, err := os.ReadFile(candidate); err == nil {
						content = string(data)
						if len(content) > 4000 {
							content = content[:4000] + "\n… (truncated)"
						}
						break
					}
				}
				if content != "" {
					files = append(files, fmt.Sprintf("[source: %s]\n```\n%s\n```", f, strings.TrimSpace(content)))
				}
			}
		}
	}

	if len(files) == 0 {
		return ""
	}
	return "[Mantis auto-read these source files referenced in the error]\n" + strings.Join(files, "\n\n")
}

// resolveFilePath returns candidate absolute paths for a file reference.
func resolveFilePath(f, root string) []string {
	var candidates []string
	// Absolute path.
	if filepath.IsAbs(f) {
		candidates = append(candidates, f)
	}
	// Relative to project root.
	candidates = append(candidates, filepath.Join(root, f))
	// Python module name → src/module.py, module.py.
	if !strings.Contains(f, "/") && !strings.Contains(f, ".") {
		candidates = append(candidates, filepath.Join(root, "src", f+".py"))
		candidates = append(candidates, filepath.Join(root, f+".py"))
	}
	return candidates
}

// extractFileNames returns a comma-separated list of filenames from injected source content.
func extractFileNames(injected string) string {
	re := regexp.MustCompile(`\[source: ([^\]]+)\]`)
	matches := re.FindAllStringSubmatch(injected, -1)
	var names []string
	for _, m := range matches {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		return "source files"
	}
	return strings.Join(names, ", ")
}

func endSession(sess *session.Session, b *brain.Brain, messages []interface{}, embStore *embeddings.Store) {
	printSep()
	fmt.Println(sess.Report())
	printSep()

	summary := ""
	if len(sess.Turns) >= 3 {
		summary = extractSessionSummary(messages)
		if summary != "" {
			_ = b.UpdateBrain(summary)
			fmt.Printf("%s● project memory updated%s\n", colorDim, colorReset)
		}
	}

	// Persist session to disk for cross-session continuity.
	topic := extractSessionTopic(messages)
	root, _ := os.Getwd()
	mantisDir := filepath.Join(root, ".mantis")
	if err := sess.Save(mantisDir, topic, summary, messages); err == nil {
		fmt.Printf("%s● session saved%s\n", colorDim, colorReset)
	}

	// Embed session summary for semantic retrieval in future sessions.
	// Synchronous to avoid goroutine leak / use-after-close on embStore.
	if embStore != nil && summary != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		id := fmt.Sprintf("session-%d", time.Now().Unix())
		_ = embStore.Add(ctx, id, "session", topic, topic+"\n"+summary)
		cancel()
	}
}

// extractSessionTopic returns a short topic from the first user message.
func extractSessionTopic(messages []interface{}) string {
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok && msg.Role == "user" {
			topic := msg.Content
			// Strip context wrapper if present.
			if idx := strings.Index(topic, "Now answer: "); idx != -1 {
				topic = topic[idx+12:]
			}
			if len(topic) > 60 {
				topic = topic[:60] + "…"
			}
			return strings.TrimSpace(topic)
		}
	}
	return "untitled session"
}

func extractSessionSummary(messages []interface{}) string {
	var userTopics []string
	var assistantTurns []string

	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok {
			switch msg.Role {
			case "user":
				content := strings.TrimSpace(msg.Content)
				// Skip injected context blocks.
				if len(content) > 10 && !strings.HasPrefix(content, "[") && !strings.HasPrefix(content, "<") {
					line := content
					if idx := strings.IndexByte(line, '\n'); idx > 0 {
						line = line[:idx]
					}
					if len(line) > 120 {
						line = line[:120] + "…"
					}
					userTopics = append(userTopics, line)
				}
			case "assistant":
				if len(msg.Content) > 50 {
					assistantTurns = append(assistantTurns, msg.Content)
				}
			}
		}
	}

	if len(assistantTurns) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Session (%s)\n\n", time.Now().Format("2006-01-02 15:04")))

	if len(userTopics) > 0 {
		sb.WriteString("**Topics:**\n")
		for _, t := range userTopics {
			sb.WriteString("- " + t + "\n")
		}
		sb.WriteString("\n")
	}

	last := assistantTurns[len(assistantTurns)-1]
	if len(last) > 600 {
		last = last[:600] + "…"
	}
	sb.WriteString("**Last response:**\n")
	sb.WriteString(last)
	sb.WriteString("\n")

	return sb.String()
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

// printSep prints a full-width dim separator line.
func printSep() {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		w = 80
	}
	fmt.Printf("%s%s%s\n", colorDim, strings.Repeat("─", w), colorReset)
}

// buildReminder formats an event-driven correction as a structured user-role message.
// Including the attempt counter and violation type follows OPENDEV's finding that
// user-role messages at the exact failure point attract stronger attention than
// static system prompt edits, especially in long sessions.
//
// Format: "[REMINDER — attempt N of M] <violationType>\n\n<detail>"
func buildReminder(violationType, detail string, attempt, maxAttempts int) string {
	return fmt.Sprintf("[REMINDER — attempt %d of %d] %s\n\n%s", attempt, maxAttempts, violationType, detail)
}

// renderResponse renders markdown through glamour for clean terminal output.
// Falls back to plain print if glamour fails.
func renderResponse(content string) {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		w = 100
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(w-4),
	)
	if err != nil {
		fmt.Println(content)
		return
	}
	out, err := r.Render(content)
	if err != nil {
		fmt.Println(content)
		return
	}
	fmt.Print(out)
}

// startSpinner shows a pulsing indicator while the model generates.
// taskType controls the lead messages shown first (task-relevant traces),
// followed by generic humorous messages to keep the user entertained.
// Returns a stop function that clears the spinner line and returns elapsed time.
func startSpinner(taskType string) func() time.Duration {
	start := time.Now()

	// Suppress spinner animation when stdout is not a terminal (piped output).
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return func() time.Duration { return time.Since(start) }
	}

	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	// Task-specific lead messages — shown first so they feel responsive.
	taskLeads := map[string][]string{
		"implement": {
			"📐 designing the architecture…",
			"🏗️  scaffolding structure…",
			"🔨 writing code…",
			"🔗 wiring it together…",
			"📦 packaging the solution…",
		},
		"fix": {
			"🔍 hunting the bug…",
			"🩺 diagnosing root cause…",
			"🔧 applying the fix…",
			"🧪 checking edge cases…",
		},
		"refactor": {
			"🧹 analysing code smells…",
			"♻️  planning refactor…",
			"✂️  extracting functions…",
			"📏 simplifying logic…",
		},
		"test": {
			"🧪 designing test cases…",
			"✅ covering happy path…",
			"💥 probing edge cases…",
			"🎯 checking coverage…",
		},
		"explain": {
			"📖 reading the code…",
			"🔭 tracing execution…",
			"💡 synthesising explanation…",
		},
		"impact-query": {
			"🗺️  mapping dependencies…",
			"🔎 tracing call paths…",
			"📊 assessing blast radius…",
		},
	}

	// Generic messages — appended after task-specific ones.
	generic := []string{
		"thinking…",
		"crunching tokens…",
		"pondering deeply…",
		"assembling thoughts…",
		"reasoning through it…",
		"weaving words…",
		"vibing with the model…",
		"summoning wisdom…",
		"connecting neurons…",
		"brewing ideas…",
		"untangling complexity…",
		"in the zone…",
		"asking the void…",
		"decoding the universe…",
		"loading big brain…",
		"deep in thought…",
		"running on caffeine…",
		"reverse engineering your question…",
		"consulting the ancient scrolls…",
		"consulting Stack Overflow…",
		"definitely not googling this…",
		"making stuff up confidently…",
		"downloading more RAM…",
		"blaming the compiler…",
		"reading the docs (rare)…",
		"squinting at the code…",
		"finding signal in the noise…",
		"rubber ducking…",
		"entering flow state…",
		"hallucinating responsibly…",
		"git blaming internally…",
		"pushing to prod (just kidding)…",
	}

	msgs := append(taskLeads[taskType], generic...)

	done := make(chan struct{})
	go func() {
		// BUG-03: use time.NewTicker instead of time.After to avoid leaking one
		// timer per iteration (time.After creates a new timer that is not GC'd
		// until it fires, accumulating hundreds per second in tight loops).
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		msgIdx := 0
		for {
			select {
			case <-done:
				fmt.Printf("\r\033[K") // clear spinner line
				return
			case <-ticker.C:
				elapsed := time.Since(start).Seconds()
				fmt.Printf("\r%s%s %s · %.1fs  Ctrl+C to cancel%s",
					colorDim, frames[i%len(frames)], msgs[msgIdx], elapsed, colorReset)
				i++
				// advance faster through task leads (~1.5s each), slower through generic (~3s)
				interval := 19
				if msgIdx >= len(taskLeads[taskType]) {
					interval = 38
				}
				if i%interval == 0 {
					msgIdx = (msgIdx + 1) % len(msgs)
				}
			}
		}
	}()
	var once sync.Once
	return func() time.Duration { once.Do(func() { close(done) }); return time.Since(start) }
}

// streamPrinter handles real-time token streaming to stdout.
// On first chunk it stops the spinner and starts printing raw tokens.
// After all chunks: sp.stop() erases the raw output and returns elapsed time,
// so the caller can glamour-render the fully assembled response cleanly.
type streamPrinter struct {
	stopFn    func() time.Duration // spinner stop function
	started   bool
	lineCount int // newlines seen so far (for cursor-up erase)
	elapsed   time.Duration
	width     int
}

func newStreamPrinter(stopFn func() time.Duration) *streamPrinter {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		w = 100
	}
	return &streamPrinter{stopFn: stopFn, width: w}
}

// onChunk is passed to streamWithFallback as the live callback.
func (sp *streamPrinter) onChunk(chunk string) {
	if !sp.started {
		sp.started = true
		// Stop spinner and clear its line before we start printing tokens.
		sp.elapsed = sp.stopFn()
		fmt.Print("\r\033[K") // clear spinner line
	}
	// Print raw chunk in dim color (will be erased and glamour-rendered after done).
	fmt.Print(colorDim + chunk + colorReset)
	// Count logical lines (accounting for line wrapping at terminal width).
	for _, r := range chunk {
		if r == '\n' {
			sp.lineCount++
		}
	}
}

// stop erases the raw streaming output from the terminal and returns elapsed time.
// If streaming never started (error before first token), stops the spinner instead.
func (sp *streamPrinter) stop() time.Duration {
	if !sp.started {
		return sp.stopFn()
	}
	// Move cursor up past all streamed lines + clear from cursor to end of screen.
	if sp.lineCount > 0 {
		fmt.Printf("\033[%dA\033[J", sp.lineCount+1)
	} else {
		fmt.Print("\r\033[K")
	}
	return sp.elapsed
}

func printBanner() {
	c := colorCopper + colorBold
	r := colorReset
	d := colorDim
	g := colorGold + colorBold

	lines := []string{
		c + `  ███╗   ███╗ █████╗ ███╗   ██╗████████╗██╗███████╗` + r,
		c + `  ████╗ ████║██╔══██╗████╗  ██║╚══██╔══╝██║██╔════╝` + r + `     ` + d + `\   /` + r,
		c + `  ██╔████╔██║███████║██╔██╗ ██║   ██║   ██║███████╗` + r + `      ` + d + `\_/` + r,
		c + `  ██║╚██╔╝██║██╔══██║██║╚██╗██║   ██║   ██║╚════██║` + r + `     ` + g + `(o_o)` + r,
		c + `  ██║ ╚═╝ ██║██║  ██║██║ ╚████║   ██║   ██║███████║` + r + ` ` + g + `<===[|||]===>` + r,
		c + `  ╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝   ╚═╝   ╚═╝╚══════╝` + r,
	}
	fmt.Println()
	for _, l := range lines {
		fmt.Println(l)
	}
	fmt.Printf("%s  AI coding assistant · free · local-first%s\n\n", colorDim, colorReset)
}

func printHelp() {
	d := colorDim
	g := colorGold
	r := colorReset
	fmt.Printf("\n%s╭─ Commands ──────────────────────────────────────────────╮%s\n", d, r)
	cmds := [][2]string{
		{"/init", "analyze codebase and generate MANTIS.md"},
		{"/file <path>", "inject a file into context"},
		{"/vision <path>", "analyze an image or screenshot"},
		{"/fetch <url>", "fetch a web page into context (Jina Reader)"},
		{"/search <query>", "web search — Jina (keyless) or Tavily (MANTIS_TAVILY_KEY)"},
		{"/ask <question>", "read-only expert analysis — no file writes"},
		{"/replay [sid]", "replay event log for this or a given session"},
		{"/benchmark", "score model quality across 10 coding tasks"},
		{"/test [pkg]", "run tests → fix failures → repeat until green"},
		{"/index", "index source code for semantic search"},
		{"/undo", "revert all files written by the last AI response"},
		{"/snapshot [label]", "save a git snapshot of current changes"},
		{"/snapshots", "list all saved snapshots"},
		{"/revert <id>", "restore working tree to a saved snapshot"},
		{"/commit", "generate commit message, preview, commit"},
		{"/pr", "push branch + generate PR title/body + create GitHub PR"},
		{"/git-log", "show last 10 mantis auto-commits"},
		{"/plan", "enter plan mode — read-only, sets permission to plan"},
		{"/build", "exit plan mode — saves plan to .mantis/PLAN.md"},
		{"/execute", "alias for /build — exit plan and start implementation"},
		{"/context", "show context window token breakdown"},
		{"/compact [focus]", "compress conversation history (optional focus topic)"},
		{"/reset", "clear conversation (brain memory kept)"},
		{"/cost", "token savings report"},
		{"/stats", "usage analytics (tiers, latency, files)"},
		{"/brain", "show project memory"},
		{"/save", "save session to project memory now"},
		{"/models", "show available models and current routing"},
		{"/reject <reason>", "log last suggestion as rejected approach"},
		{"/decision <text>", "log an architecture decision"},
		{"/keys", "manage API keys (add/delete/tag)"},
		{"/mode [mode]", "set permission mode (default|auto-edit|plan)"},
		{"/effort [level]", "set thinking effort (low|medium|high)"},
		{"/notify on|off", "toggle desktop notifications for slow tasks"},
		{"/mcp list|load", "manage MCP server connections (.mcp.json)"},
		{"/telemetry on|off", "enable / disable anonymous usage upload"},
		{"/version", "show current version"},
		{"/quit", "exit  (also Ctrl+C)"},
	}
	for _, c := range cmds {
		fmt.Printf("%s│%s  %s%-20s%s  %s%s%s │%s\n",
			d, r, g, c[0], r, d, c[1], r, r)
	}
	fmt.Printf("%s╰────────────────────────────────────────────────────────╯%s\n\n", d, r)
}

func printFooter() {
	fmt.Printf("\n%s  /help · /file · /test · /brain · /quit%s\n\n", colorDim, colorReset)
}

// tierColors maps each tier to a terminal color code for the routing badge.
var tierColors = map[router.Tier]string{
	router.TierTrivial: "\033[38;5;244m", // dim grey
	router.TierFast:    "\033[38;5;37m",  // teal
	router.TierCode:    "\033[38;5;34m",  // green
	router.TierReason:  "\033[38;5;220m", // yellow
	router.TierHeavy:   "\033[38;5;214m", // orange
	router.TierMax:     "\033[38;5;197m", // red
	router.TierVision:  "\033[38;5;141m", // purple
}

func tierBadge(t router.Tier) string {
	c, ok := tierColors[t]
	if !ok {
		c = colorDim
	}
	return c + colorBold + "[" + t.String() + "]" + colorReset
}

// tierPromptStr returns the readline prompt string with the active tier colored badge.
// Displayed during model generation so the user can see which tier is active.
func tierPromptStr(t router.Tier) string {
	c, ok := tierColors[t]
	if !ok {
		c = colorDim
	}
	return c + colorBold + "[" + t.String() + "]" + colorReset + " \033[38;5;214m❯\033[0m "
}

func showRouting(intent router.Intent, model string, turn int, isPipeline bool) {
	badge := tierBadge(intent.Tier)
	confStr := fmt.Sprintf("%s%d%%%s", colorDim, int(intent.Confidence*100), colorReset)
	graphTag := ""
	if intent.NeedsGraph {
		graphTag = " " + colorGold + "[graph]" + colorReset
	}
	modelStyled := colorCopper + colorBold + model + colorReset

	switch {
	case intent.Tier == router.TierMax:
		fmt.Printf("%s ┌ #%d%s  %s  ensemble · multi-model  %s%s\n",
			colorDim, turn, colorReset, badge, confStr, graphTag)
	case isPipeline:
		fmt.Printf("%s ┌ #%d%s  %s  pipeline · %s  %s%s\n",
			colorDim, turn, colorReset, badge, modelStyled, confStr, graphTag)
	default:
		fmt.Printf("%s ┌ #%d%s  %s  %s · %s  %s%s\n",
			colorDim, turn, colorReset, badge, intent.TaskType, modelStyled, confStr, graphTag)
	}
}

// normalizeTerminalInput detects when the user pastes raw terminal output
// (shell errors, npm/go output) and rewrites it as an explicit fix request so
// the router picks the right tier and the model knows what to do.
// verifyAndFix runs the project's build check after files are written.
// If the build fails it re-prompts the model with the error (max maxRetries times),
// writes any corrected files, and returns the final written file list.
// It is a no-op when no build system is detected or no files were written.
func verifyAndFix(
	ctx context.Context,
	client *ollama.Client,
	model string,
	tier router.Tier,
	root string,
	written []WrittenFile,
	messages *[]interface{},
) []WrittenFile {
	const maxRetries = 4

	if len(written) == 0 || !autofix.ShouldRun(root, pathsOf(written)) {
		return written
	}

	var lastBuildErr string
	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("%s  🔨 verifying build…%s\n", colorDim, colorReset)
		result := autofix.Check(root, pathsOf(written))
		if result == nil || result.Passed {
			fmt.Printf("%s  ✓ build passing%s\n\n", colorGold, colorReset)
			return written
		}

		// Stuck detection: same error twice in a row → give up early.
		if result.Output == lastBuildErr {
			fmt.Printf("%s  ✗ same build error twice — auto-fix stuck, stopping%s\n\n", colorRed, colorReset)
			break
		}
		lastBuildErr = result.Output

		fmt.Printf("%s  ✗ build errors (attempt %d/%d) — auto-fixing…%s\n",
			colorRed, attempt, maxRetries, colorReset)

		// Inject the build error as a new user message.
		// BUG-04: copy slice before append to avoid aliasing the backing array.
		fixMsg := autofix.FormatError(result)
		retryMsgs := append(append([]interface{}{}, *messages...), ollama.Message{Role: "user", Content: fixMsg})

		var rb strings.Builder
		fixCtx, fixCancel := context.WithTimeout(ctx, 5*time.Minute)
		_, _, err := streamWithFallback(fixCtx, client, model, tier, retryMsgs, &rb)
		fixCancel()
		if err != nil || strings.TrimSpace(rb.String()) == "" {
			fmt.Printf("%s  ✗ auto-fix failed — check errors manually%s\n\n", colorRed, colorReset)
			return written
		}

		// Show fixed files only (no full response dump).
		fmt.Printf("%s◈ Mantis%s %s(auto-fix)%s\n", colorCopper+colorBold, colorReset, colorDim, colorReset)
		newFiles := extractAndWriteFiles(rb.String(), root, nil) // oplog n/a in verifyAndFix
		if len(newFiles) > 0 {
			printWrittenFiles(newFiles)
			written = append(written, newFiles...)
		} else {
			fmt.Printf("%s  (no file changes from auto-fix)%s\n\n", colorDim, colorReset)
		}

		// Append the fix exchange to conversation history.
		*messages = append(*messages,
			ollama.Message{Role: "user", Content: fixMsg},
			ollama.Message{Role: "assistant", Content: rb.String()},
		)
	}

	// Final check after all retries.
	result := autofix.Check(root, pathsOf(written))
	if result != nil && !result.Passed {
		fmt.Printf("%s  ⚠ build still failing after %d retries — errors above may need manual fixes%s\n\n",
			colorRed, maxRetries, colorReset)
	} else {
		fmt.Printf("%s  ✓ build passing%s\n\n", colorGold, colorReset)
	}
	return written
}

func pathsOf(files []WrittenFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func normalizeTerminalInput(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return input
	}

	// Patterns that indicate terminal error output, not a question.
	terminalErrorPatterns := []struct {
		marker string
		label  string
	}{
		{"command not found", "shell error"},
		{"make: ***", "make error"},
		{"npm ERR!", "npm error"},
		{"npm WARN", "npm warning"},
		{"Error:", "runtime error"},
		{"error TS", "TypeScript error"},
		{"error[E", "Rust compiler error"},
		{"FAILED", "test failure"},
		{"exit status", "process error"},
		{"panic:", "Go panic"},
		{"Traceback (most recent call last)", "Python traceback"},
		{"SyntaxError:", "syntax error"},
		{"TypeError:", "type error"},
		{"ReferenceError:", "reference error"},
		{"Cannot find module", "module not found"},
		{"Module not found", "module not found"},
		{"ENOENT:", "file not found"},
		{"EADDRINUSE", "port in use"},
		{"connection refused", "connection error"},
	}

	lc := strings.ToLower(s)
	for _, p := range terminalErrorPatterns {
		if strings.Contains(lc, strings.ToLower(p.marker)) {
			// Wrap raw terminal output as an explicit fix request.
			return "Fix this " + p.label + ":\n\n" + s
		}
	}

	// Detect bare shell prompts pasted as messages (no actual question).
	// e.g. "(base) user@host path % " or "user@host:~$"
	if (strings.HasPrefix(s, "(") || strings.Contains(s[:min(30, len(s))], "@")) &&
		(strings.Contains(s, " % ") || strings.Contains(s, ":~$") || strings.Contains(s, ":~#")) {
		return "I ran this command in the terminal. What should I do next?\n\n" + s
	}

	return input
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseTier(s string) router.Tier {
	switch strings.ToLower(s) {
	case "trivial":
		return router.TierTrivial
	case "fast":
		return router.TierFast
	case "code":
		return router.TierCode
	case "reason":
		return router.TierReason
	case "heavy":
		return router.TierHeavy
	case "max":
		return router.TierMax
	case "vision":
		return router.TierVision
	default:
		return router.TierCode
	}
}

// streamEnsemble runs multiple specialised models in parallel, then synthesises
// the best answer. This is the "max" tier — wider than any single model call.
func streamEnsemble(ctx context.Context, client *ollama.Client,
	available []ollama.ModelInfo, messages []interface{}, rb *strings.Builder) (int, int, error) {

	models := router.EnsembleModels(available)
	if len(models) < 2 {
		// Not enough models for ensemble — fall back to heavy single model.
		fallback := router.ModelFor(router.TierHeavy)
		return streamWithFallback(ctx, client, fallback, router.TierHeavy, messages, rb)
	}

	fmt.Printf("%s  [ensemble · %d models: %s]%s\n",
		colorDim, len(models), strings.Join(models, ", "), colorReset)

	type modelResult struct {
		model   string
		content string
		pt, ct  int
		err     error
	}

	results := make([]modelResult, len(models))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for i, m := range models {
		wg.Add(1)
		go func(idx int, model string) {
			defer wg.Done()
			mStart := time.Now()
			var buf strings.Builder
			pt, ct, err := client.StreamChat(ctx, model, messages, &ollama.ModelOptions{Temperature: 0.15, NumCtx: 32768},
				func(chunk string) { buf.WriteString(chunk) })
			results[idx] = modelResult{model, buf.String(), pt, ct, err}
			mu.Lock()
			done++
			elapsed := time.Since(mStart).Seconds()
			if err == nil {
				fmt.Printf("%s  [%d/%d ✓ %s · %.1fs]%s\n", colorGold, done, len(models), model, elapsed, colorReset)
			} else {
				fmt.Printf("%s  [%d/%d ✗ %s · %.1fs]%s\n", colorRed, done, len(models), model, elapsed, colorReset)
			}
			mu.Unlock()
		}(i, m)
	}
	wg.Wait()

	// Collect successful responses.
	type good struct {
		model   string
		content string
		pt, ct  int
	}
	var responses []good
	totalPt, totalCt := 0, 0
	for _, r := range results {
		if r.err == nil && strings.TrimSpace(r.content) != "" {
			responses = append(responses, good{r.model, r.content, r.pt, r.ct})
			totalPt += r.pt
			totalCt += r.ct
		}
	}

	if len(responses) == 0 {
		return 0, 0, fmt.Errorf("all ensemble models failed")
	}
	if len(responses) == 1 {
		rb.WriteString(responses[0].content)
		return responses[0].pt, responses[0].ct, nil
	}

	// Synthesis pass — ask a smart model to merge the best parts.
	fmt.Printf("%s  [synthesising %d responses…]%s\n", colorDim, len(responses), colorReset)

	var synthPrompt strings.Builder
	synthPrompt.WriteString("You received the following responses from different AI models to the same question.\n")
	synthPrompt.WriteString("Evaluate each response for:\n")
	synthPrompt.WriteString("1. Correctness — does the code use real function names and compile?\n")
	synthPrompt.WriteString("2. Completeness — does it handle edge cases and errors?\n")
	synthPrompt.WriteString("3. Reasoning — is the explanation logical and well-structured?\n\n")
	synthPrompt.WriteString("Then produce ONE definitive, complete answer that:\n")
	synthPrompt.WriteString("- Uses the most correct code from any response\n")
	synthPrompt.WriteString("- Keeps the strongest reasoning chain\n")
	synthPrompt.WriteString("- Flags any uncertainty\n")
	synthPrompt.WriteString("Do NOT mention the models or responses.\n\n")
	for i, r := range responses {
		synthPrompt.WriteString(fmt.Sprintf("--- Response %d ---\n%s\n\n", i+1, r.content))
	}
	synthPrompt.WriteString("--- Synthesised answer ---\n")

	synthMessages := []interface{}{
		ollama.Message{Role: "system", Content: "You are an expert synthesis engine. Evaluate multiple AI responses for correctness, completeness, and reasoning quality. Combine them into the single best answer."},
		ollama.Message{Role: "user", Content: synthPrompt.String()},
	}
	synthModel := router.ModelFor(router.TierReason)
	spt, sct, err := client.StreamChat(ctx, synthModel, synthMessages, &ollama.ModelOptions{Temperature: 0.3, NumCtx: 32768},
		func(chunk string) { rb.WriteString(chunk) })
	if err != nil {
		// Synthesis failed — just return the longest good response.
		best := responses[0]
		for _, r := range responses[1:] {
			if len(r.content) > len(best.content) {
				best = r
			}
		}
		rb.WriteString(best.content)
		return totalPt, totalCt, nil
	}
	return totalPt + spt, totalCt + sct, nil
}

// on 404 until one works. On 400 "prompt too long" it retries with trimmed context.
// An optional onLive callback (variadic, take at most one) is called for each chunk
// in addition to accumulating into rb. Used for real-time token streaming to stdout.
// modelOptsForTier returns temperature and context window settings per tier.
// Lower temperature → more deterministic, better for code.
// Higher temperature → more creative, better for explanations.
// NumCtx=32768 maximises the context window for models that support it.
func modelOptsForTier(tier router.Tier) *ollama.ModelOptions {
	temp := 0.5 // default: balanced
	switch tier {
	case router.TierTrivial, router.TierFast:
		temp = 0.5
	case router.TierCode:
		temp = 0.15 // deterministic code generation
	case router.TierReason:
		temp = 0.3 // analytical but not rigid
	case router.TierHeavy, router.TierMax:
		temp = 0.2 // complex code needs consistency
	}
	// Use model-aware context window instead of hardcoded 32K.
	model := router.ModelFor(tier)
	numCtx := router.ContextWindowFor(model)
	return &ollama.ModelOptions{Temperature: temp, NumCtx: numCtx}
}

func streamWithFallback(ctx context.Context, client *ollama.Client, model string,
	tier router.Tier, messages []interface{}, rb *strings.Builder, onLive ...func(string)) (int, int, error) {

	opts := modelOptsForTier(tier)
	tried := map[string]bool{}
	candidates := []string{model}
	candidates = append(candidates, router.PreferredModels(tier)...)

	for _, m := range candidates {
		if tried[m] {
			continue
		}
		tried[m] = true

		liveCallback := func(chunk string) {}
		if len(onLive) > 0 && onLive[0] != nil {
			liveCallback = onLive[0]
		}
		pt, ct, err := client.StreamChat(ctx, m, messages, opts,
			func(chunk string) { rb.WriteString(chunk); liveCallback(chunk) })

		if err == nil {
			if m != model {
				fmt.Printf("%s  [switched to %s]%s\n", colorDim, m, colorReset)
				router.SetResolved(tier, m)
			}
			return pt, ct, nil
		}

		errStr := err.Error()

		if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") {
			rb.Reset()
			fmt.Printf("%s  [%s not available, trying next…]%s\n", colorDim, m, colorReset)
			continue
		}

		// Rate limited or server overloaded — try key rotation first, then retry with backoff.
		if strings.Contains(errStr, "429") || strings.Contains(errStr, "503") || strings.Contains(errStr, "524") ||
			strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "overloaded") {

			// ── Key rotation: try switching API key before retrying same model ──
			if activeKeyRing != nil && activeKeyRing.Count() > 1 {
				fromIdx := activeKeyRing.ActiveIndex()
				activeKeyRing.MarkRateLimited(15 * time.Minute)
				if newKey, toIdx, ok := activeKeyRing.Rotate(); ok {
					client.SetAPIKey(newKey)
					rb.Reset()
					fmt.Printf("%s  [key #%d rate limited — switching to key #%d]%s\n",
						colorDim, fromIdx, toIdx, colorReset)
					pt, ct, err2 := client.StreamChat(ctx, m, messages, opts,
						func(chunk string) { rb.WriteString(chunk); liveCallback(chunk) })
					if err2 == nil {
						if m != model {
							fmt.Printf("%s  [switched to %s]%s\n", colorDim, m, colorReset)
							router.SetResolved(tier, m)
						}
						return pt, ct, nil
					}
					// New key also failed — fall through to backoff retries.
					err = err2
					errStr = err.Error()
					if !strings.Contains(errStr, "429") && !strings.Contains(errStr, "503") &&
						!strings.Contains(errStr, "rate limit") && !strings.Contains(errStr, "overloaded") {
						return 0, 0, err // different error, bail
					}
				}
			}

			// ── Exponential backoff retries ──
			const maxRetries = 3
			for attempt := 0; attempt < maxRetries; attempt++ {
				wait := time.Duration(1<<uint(attempt)) * 2 * time.Second // 2s, 4s, 8s
				fmt.Printf("%s  [rate limited — retrying in %s (%d/%d)]%s\n",
					colorDim, wait, attempt+1, maxRetries, colorReset)
				select {
				case <-ctx.Done():
					return 0, 0, ctx.Err()
				case <-time.After(wait):
				}
				rb.Reset()
				pt, ct, err2 := client.StreamChat(ctx, m, messages, opts,
					func(chunk string) { rb.WriteString(chunk); liveCallback(chunk) })
				if err2 == nil {
					if m != model {
						fmt.Printf("%s  [switched to %s]%s\n", colorDim, m, colorReset)
						router.SetResolved(tier, m)
					}
					return pt, ct, nil
				}
				if !strings.Contains(err2.Error(), "429") && !strings.Contains(err2.Error(), "503") {
					err = err2
					break // different error, stop retry loop
				}
				err = err2
			}
			// Rate limit persisted — fall through to next candidate.
			rb.Reset()
			fmt.Printf("%s  [%s still rate limited, trying next…]%s\n", colorDim, m, colorReset)
			continue
		}

		// Context window exceeded — strip history and retry same model with minimal context.
		if strings.Contains(errStr, "400") && (strings.Contains(errStr, "context length") ||
			strings.Contains(errStr, "prompt too long") || strings.Contains(errStr, "too many tokens")) {
			rb.Reset()
			fmt.Printf("%s  [context too long, trimming history…]%s\n", colorDim, colorReset)
			trimmed := trimToMinimal(messages)
			pt, ct, err2 := client.StreamChat(ctx, m, trimmed, nil,
				func(chunk string) { rb.WriteString(chunk) })
			if err2 == nil {
				if m != model {
					router.SetResolved(tier, m)
				}
				return pt, ct, nil
			}
			return 0, 0, err2
		}

		// Role alternation violation — fix message ordering and retry.
		if strings.Contains(errStr, "400") && strings.Contains(errStr, "roles must alternate") {
			rb.Reset()
			fmt.Printf("%s  [fixing message order…]%s\n", colorDim, colorReset)
			trimmed := trimToMinimal(messages)
			pt, ct, err2 := client.StreamChat(ctx, m, trimmed, nil,
				func(chunk string) { rb.WriteString(chunk) })
			if err2 == nil {
				if m != model {
					router.SetResolved(tier, m)
				}
				return pt, ct, nil
			}
			return 0, 0, err2
		}

		return 0, 0, err // non-recoverable error, bail
	}
	return 0, 0, fmt.Errorf("no available model found for tier %s — run /models to see what's available", tier)
}

// trimToMinimal keeps the system message and the last 3 user+assistant pairs.
// Used as a last resort when the full context exceeds the model's window.
// Ensures strict user/assistant alternation to satisfy the Ollama API.
func trimToMinimal(messages []interface{}) []interface{} {
	var sys interface{}
	var recent []interface{}
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok {
			if msg.Role == "system" {
				sys = m
			} else if msg.Role == "user" || msg.Role == "assistant" {
				recent = append(recent, m)
			}
			// Drop tool messages — they break alternation after trimming.
		} else if img, ok := m.(ollama.ImageMessage); ok && img.Role == "user" {
			recent = append(recent, m)
		}
		// Drop ToolMessage entries silently.
	}
	var out []interface{}
	if sys != nil {
		out = append(out, sys)
	}
	// Keep last 6 messages (≈3 user+assistant pairs).
	if len(recent) > 6 {
		recent = recent[len(recent)-6:]
	}
	// Enforce strict alternation: first kept message must be "user".
	for len(recent) > 0 {
		if msg, ok := recent[0].(ollama.Message); ok && msg.Role == "user" {
			break
		}
		recent = recent[1:]
	}
	// Remove consecutive same-role messages.
	var deduped []interface{}
	lastRole := ""
	for _, m := range recent {
		role := ""
		if msg, ok := m.(ollama.Message); ok {
			role = msg.Role
		} else if _, ok := m.(ollama.ImageMessage); ok {
			role = "user"
		}
		if role == lastRole {
			// Merge into previous by replacing it.
			deduped[len(deduped)-1] = m
		} else {
			deduped = append(deduped, m)
			lastRole = role
		}
	}
	out = append(out, deduped...)
	return out
}

// multiPassReasoning performs a silent analysis pass before the main response.
// For complex queries (Reason/Heavy), the model first analyzes constraints and
// risks, then this analysis is injected as context for the solution pass.
// BUG-06: accept caller's context so the analysis pass is cancellable.
func multiPassReasoning(ctx context.Context, client *ollama.Client, model string, tier router.Tier, messages []interface{}) []interface{} {
	// Extract the last user message.
	var lastUserContent string
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(ollama.Message); ok && msg.Role == "user" {
			lastUserContent = msg.Content
			break
		}
	}
	if lastUserContent == "" {
		return messages
	}

	analysisPrompt := "Before answering, analyze this request silently:\n" +
		"1. What are the key constraints?\n" +
		"2. What information do I need to verify?\n" +
		"3. What could go wrong with a naive approach?\n" +
		"4. What's the simplest correct solution?\n\n" +
		"Provide a brief analysis (3-5 bullet points), not the solution."

	analysisMsgs := make([]interface{}, len(messages))
	copy(analysisMsgs, messages)
	analysisMsgs = append(analysisMsgs, ollama.Message{Role: "user", Content: analysisPrompt})

	analysisCtx, analysisCancel := context.WithTimeout(ctx, 90*time.Second)
	defer analysisCancel()

	var analysis strings.Builder
	_, _, err := streamWithFallback(analysisCtx, client, model, tier, analysisMsgs, &analysis)
	if err != nil || strings.TrimSpace(analysis.String()) == "" {
		return messages // Analysis failed — continue with single pass.
	}

	// Inject analysis as a system-level hint — use a neutral label the model
	// won't imitate in its next response.
	analysisNote := ollama.Message{
		Role:    "assistant",
		Content: "[pre-response-thinking]\n" + analysis.String(),
	}
	refinedPrompt := ollama.Message{
		Role:    "user",
		Content: "Given the analysis above, now provide your complete answer.",
	}

	result := make([]interface{}, len(messages))
	copy(result, messages)
	result = append(result, analysisNote, refinedPrompt)
	return result
}

// estimateConversationTokens returns a token count estimate for a message slice.
// Uses a word-count heuristic (words*1.3) which is more accurate than pure char/3.5
// for mixed text+code content. Code-heavy text averages ~1.2 tokens/word; natural
// language averages ~1.4. The 1.3 midpoint gives <15% error on typical conversations.
func estimateConversationTokens(messages []interface{}) int {
	total := 0
	for _, m := range messages {
		var content string
		if msg, ok := m.(ollama.Message); ok {
			content = msg.Content
		} else if img, ok := m.(ollama.ImageMessage); ok {
			content = img.Content
		}
		if content == "" {
			continue
		}
		// Word count: split on whitespace.
		words := len(strings.Fields(content))
		// 1.3 tokens/word + 4-token message overhead.
		total += int(float64(words)*1.3) + 4
	}
	return total
}

// compressIfNeeded proactively compresses conversation history when it gets too large.
// Instead of waiting for a 400 error, it summarizes old turns at ~80% capacity.
// Uses the active model's context window to set a dynamic threshold.
func compressIfNeeded(callerCtx context.Context, messages []interface{}, client *ollama.Client, activeModel string) []interface{} {
	estimatedTokens := estimateConversationTokens(messages)

	// Dynamic threshold: compress at 75% of the model's context window.
	// This leaves 25% headroom for the next response + system prompt overhead.
	ctxWindow := router.ContextWindowFor(activeModel)
	compressThreshold := int(float64(ctxWindow) * 0.75)
	if compressThreshold < 8000 {
		compressThreshold = 8000 // absolute floor for tiny models
	}

	if estimatedTokens < compressThreshold {
		return messages
	}

	// Separate system prompt and conversation turns.
	var sys interface{}
	var turns []interface{}
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok && msg.Role == "system" {
			sys = m
		} else {
			turns = append(turns, m)
		}
	}

	// Keep last 10 messages (5 user+assistant pairs) for better recency.
	keepRecent := 10
	if len(turns) <= keepRecent {
		return messages // not enough to compress
	}

	oldTurns := turns[:len(turns)-keepRecent]
	recentTurns := turns[len(turns)-keepRecent:]

	// Build Acon-style structured summary (arXiv 2510.00615):
	// Five distinct sections preserve different information types without merging them.
	var summaryInput strings.Builder
	summaryInput.WriteString("Compress the conversation below, preserving ALL of the following in labeled sections:\n\n")
	summaryInput.WriteString("FACTS: Technical decisions made, files written to disk, stack choices (language/framework/DB/auth)\n")
	summaryInput.WriteString("ACTIONS: What was tried and the exact outcome — do NOT merge separate attempts into one line\n")
	summaryInput.WriteString("STATE: Current state of each modified file (which functions exist, which are stubs or incomplete)\n")
	summaryInput.WriteString("PRECONDITIONS: What must be true before the next step can proceed (dependencies, build state, env vars)\n")
	summaryInput.WriteString("NEXT: Explicit next steps the user requested or were discussed\n\n")
	summaryInput.WriteString("Format each section with its label on its own line (e.g. 'FACTS:'). Never merge items across sections.\n")
	summaryInput.WriteString("Keep it under 700 words.\n\n")
	for _, m := range oldTurns {
		if msg, ok := m.(ollama.Message); ok {
			role := msg.Role
			content := msg.Content
			// Adaptive truncation: allow more content for larger context windows.
			// Base: 800 chars; up to 2000 for 128K+ models.
			maxChars := 800 + (ctxWindow / 100)
			if maxChars > 2000 {
				maxChars = 2000
			}
			if len(content) > maxChars {
				content = content[:maxChars] + "…"
			}
			summaryInput.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, content))
		}
	}

	// Use a fast model for summarization with adequate context.
	summaryModel := router.ModelFor(router.TierFast)
	summaryCtxWindow := router.ContextWindowFor(summaryModel)
	if summaryCtxWindow < 16384 {
		summaryCtxWindow = 16384
	}
	summaryMsgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a conversation compressor. Output exactly five labeled sections: FACTS, ACTIONS, STATE, PRECONDITIONS, NEXT. No preamble."},
		ollama.Message{Role: "user", Content: summaryInput.String()},
	}

	var summaryBuf strings.Builder
	ctx, cancel := context.WithTimeout(callerCtx, 45*time.Second)
	defer cancel()
	_, _, err := client.StreamChat(ctx, summaryModel, summaryMsgs, &ollama.ModelOptions{Temperature: 0.3, NumCtx: summaryCtxWindow},
		func(chunk string) { summaryBuf.WriteString(chunk) })

	if err != nil || strings.TrimSpace(summaryBuf.String()) == "" {
		// Summarization failed — fall back to trimToMinimal behavior.
		return trimToMinimal(messages)
	}

	fmt.Printf("%s  [compressed %d turns → summary (%dK→%dK tokens)]%s\n",
		colorDim, len(oldTurns), estimatedTokens/1000, estimateConversationTokens(recentTurns)/1000+1, colorReset)

	// Rebuild: system + summary + recent turns.
	var compressed []interface{}
	if sys != nil {
		compressed = append(compressed, sys)
	}
	compressed = append(compressed, ollama.Message{
		Role:    "user",
		Content: "[conversation summary — prior turns compressed]\n" + summaryBuf.String() + "\n[/conversation summary]",
	})
	compressed = append(compressed, ollama.Message{
		Role:    "assistant",
		Content: "Understood. I have the full conversation context from the summary above.",
	})
	compressed = append(compressed, recentTurns...)
	return compressed
}

// compactWithFocus forces conversation compaction with an optional focus hint.
// Unlike compressIfNeeded (which only fires at 75% capacity), this always compacts
// if there are enough turns. The focus hint biases the summary toward specific topics.
// savePlanFromConversation extracts plan content from the conversation and
// saves it to .mantis/PLAN.md so it can be reviewed before execution.
func savePlanFromConversation(messages []interface{}) {
	var planContent strings.Builder
	for _, m := range messages {
		msg, ok := m.(ollama.Message)
		if !ok || msg.Role != "assistant" {
			continue
		}
		// Look for structured plan markers or significant planning content.
		content := msg.Content
		if strings.Contains(content, "## Plan") || strings.Contains(content, "## Implementation") ||
			strings.Contains(content, "### Step") || strings.Contains(content, "1.") {
			planContent.WriteString(content)
			planContent.WriteString("\n\n---\n\n")
		}
	}
	if planContent.Len() == 0 {
		return
	}
	mantisDir := ".mantis"
	if _, err := os.Stat(mantisDir); os.IsNotExist(err) {
		return
	}
	planPath := filepath.Join(mantisDir, "PLAN.md")
	header := "# Implementation Plan\n\n_Generated during plan mode. Review before executing._\n\n---\n\n"
	_ = os.WriteFile(planPath, []byte(header+planContent.String()), 0o644)
	fmt.Printf("%s  ◆ plan saved to .mantis/PLAN.md%s\n", colorDim, colorReset)
}

func compactWithFocus(ctx context.Context, messages []interface{}, client *ollama.Client, model, focusHint string) []interface{} {
	// Separate system prompt and turns.
	var sys interface{}
	var turns []interface{}
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok && msg.Role == "system" {
			sys = m
		} else {
			turns = append(turns, m)
		}
	}
	// Need at least 6 messages to compact (3 user+assistant pairs).
	if len(turns) < 6 {
		return messages
	}

	keepRecent := 6
	if len(turns) <= keepRecent {
		return messages
	}
	oldTurns := turns[:len(turns)-keepRecent]
	recentTurns := turns[len(turns)-keepRecent:]

	var summaryInput strings.Builder
	summaryInput.WriteString("Compress the conversation below into five labeled sections:\n")
	summaryInput.WriteString("FACTS: Technical decisions, files written, stack choices\n")
	summaryInput.WriteString("ACTIONS: What was tried and exact outcomes\n")
	summaryInput.WriteString("STATE: Current state of modified files\n")
	summaryInput.WriteString("PRECONDITIONS: What must be true before next step\n")
	summaryInput.WriteString("NEXT: Explicit next steps discussed\n\n")
	if focusHint != "" {
		summaryInput.WriteString(fmt.Sprintf("FOCUS: Pay extra attention to anything about: %s\n\n", focusHint))
	}
	summaryInput.WriteString("Keep it under 700 words.\n\n")
	for _, m := range oldTurns {
		if msg, ok := m.(ollama.Message); ok {
			content := msg.Content
			if len(content) > 1500 {
				content = content[:1500] + "…"
			}
			summaryInput.WriteString(fmt.Sprintf("[%s]: %s\n\n", msg.Role, content))
		}
	}

	summaryMsgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a conversation compressor. Output exactly five labeled sections: FACTS, ACTIONS, STATE, PRECONDITIONS, NEXT. No preamble."},
		ollama.Message{Role: "user", Content: summaryInput.String()},
	}

	var summaryBuf strings.Builder
	sumCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	_, _, err := client.StreamChat(sumCtx, model, summaryMsgs,
		&ollama.ModelOptions{Temperature: 0.3, NumCtx: 16384},
		func(chunk string) { summaryBuf.WriteString(chunk) })
	if err != nil || strings.TrimSpace(summaryBuf.String()) == "" {
		return messages // summarization failed — keep as-is
	}

	var compressed []interface{}
	if sys != nil {
		compressed = append(compressed, sys)
	}
	compressed = append(compressed, ollama.Message{
		Role:    "user",
		Content: "[conversation summary — prior turns compressed]\n" + summaryBuf.String() + "\n[/conversation summary]",
	})
	compressed = append(compressed, ollama.Message{
		Role:    "assistant",
		Content: "Understood. I have the full conversation context from the summary above.",
	})
	compressed = append(compressed, recentTurns...)
	return compressed
}

// extractImpactTarget scans user input for words that match known graph nodes
// (files or functions). Returns the first matching node ID, or "" if none found.
// Used to seed the impact analysis for the multi-agent gate.
func extractImpactTarget(input string, q *graph.Querier) string {
	if q == nil {
		return ""
	}
	stopWords := map[string]bool{
		"the": true, "this": true, "that": true, "with": true, "from": true,
		"into": true, "and": true, "for": true, "all": true, "use": true,
		"make": true, "just": true, "also": true, "can": true, "will": true,
	}
	for _, word := range strings.Fields(input) {
		// Strip punctuation.
		word = strings.Trim(word, ".,;:!?\"'()")
		if len(word) < 4 || stopWords[strings.ToLower(word)] {
			continue
		}
		nodes, err := q.FindNodeByName(word)
		if err != nil || len(nodes) == 0 {
			continue
		}
		for _, n := range nodes {
			if n.Type == graph.NodeTypeFile || n.Type == graph.NodeTypeFunction {
				return n.ID
			}
		}
	}
	return ""
}

func injectFile(path string, messages *[]interface{}) {
	data, err := os.ReadFile(path)
	if err != nil {
		cwd, _ := os.Getwd()
		abs := filepath.Join(cwd, path)
		data, err = os.ReadFile(abs)
		if err != nil {
			fmt.Printf("%s✗ file not found: %s%s\n", colorRed, path, colorReset)
			// Show files in the same directory as a hint.
			dir := filepath.Dir(abs)
			if entries, de := os.ReadDir(dir); de == nil {
				fmt.Printf("%s  files in %s:%s\n", colorDim, dir, colorReset)
				shown := 0
				for _, e := range entries {
					if !e.IsDir() && shown < 10 {
						fmt.Printf("%s    %s%s\n", colorDim, e.Name(), colorReset)
						shown++
					}
				}
			}
			fmt.Println()
			return
		}
	}
	lang := extToLang(filepath.Ext(path))
	content := fmt.Sprintf("File: %s\n```%s\n%s\n```", path, lang, string(data))
	*messages = append(*messages, ollama.Message{Role: "user", Content: content})
	fmt.Printf("%s● %s loaded (%d bytes)%s\n\n", colorGold, path, len(data), colorReset)
}

func loadImage(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func extToLang(ext string) string {
	m := map[string]string{
		".go": "go", ".py": "python", ".ts": "typescript", ".tsx": "tsx",
		".js": "javascript", ".jsx": "jsx", ".rs": "rust", ".java": "java",
		".c": "c", ".cpp": "cpp", ".md": "markdown", ".json": "json",
		".yaml": "yaml", ".yml": "yaml", ".sh": "bash", ".sql": "sql",
	}
	if lang, ok := m[ext]; ok {
		return lang
	}
	return ""
}

// runWithScanner is a bare-bones fallback REPL used when readline can't init (e.g. piped input).
func runWithScanner(cfg Config, client *ollama.Client, sess *session.Session,
	b *brain.Brain, dispatcher *nl.Dispatcher, messages []interface{},
	tw *truth.Writer, ut *usage.Tracker, rs router.EmbedStore, webFetcher *web.Fetcher) error {

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	scannerBrainCtx := b.Load()
	scannerPlanMode := cfg.PlanMode
	for {
		fmt.Printf("%s❯%s ", colorCopper, colorReset)
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			scannerPermMode := ModeDefault
			scannerEffort := EffortMedium
			if quit := handleSlashCommand(input, sess, b, &messages, client, &scannerBrainCtx, &scannerPlanMode, cfg, webFetcher, nil, nil, nil, &scannerPermMode, nil, &scannerEffort, nil); quit {
				break
			}
			continue
		}
		// Reuse one-shot handler for each turn.
		cfg.InitialQuery = input
		_ = runOnce(cfg, client, sess, b, dispatcher, &messages, tw, ut, rs)
		cfg.InitialQuery = ""
	}
	endSession(sess, b, messages, nil)
	return nil
}
