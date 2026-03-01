// Package repl implements the interactive AI coding assistant.
// Invoked by running `mantis` with no subcommand.
package repl

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"golang.org/x/term"
	"github.com/charmbracelet/glamour"
	"github.com/seedhire/mantis/internal/brain"
	"github.com/seedhire/mantis/internal/embeddings"
	"github.com/seedhire/mantis/internal/nl"
	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/pipeline"
	"github.com/seedhire/mantis/internal/router"
	"github.com/seedhire/mantis/internal/session"
	"github.com/seedhire/mantis/internal/setup"
	"github.com/seedhire/mantis/internal/telemetry"
	"github.com/seedhire/mantis/internal/truth"
	"github.com/seedhire/mantis/internal/usage"
	"github.com/seedhire/mantis/internal/verify"
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
}

// Run starts the interactive REPL. Blocks until the user quits.
func Run(cfg Config) error {
	root, _ := os.Getwd()

	// First-run setup runs WITHOUT the banner so the wizard has full screen.
	// After it completes we clear and show the clean start screen.
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
		return fmt.Errorf("not authenticated")
	}

	// ── Clean start screen ───────────────────────────────────────────────────
	clearScreen()
	printBanner()

	// Status dots — one line each, only shown if relevant.
	creds, _ := setup.Load()
	if creds != nil && creds.GitHubUser != "" {
		fmt.Printf("%s● %s%s\n", colorGreen, creds.GitHubUser, colorReset)
	}

	client := ollama.NewFromEnv()
	if client.IsCloud() {
		fmt.Printf("%s● Ollama Cloud%s\n", colorGreen, colorReset)
	} else {
		fmt.Printf("%s● local Ollama%s\n", colorDim, colorReset)
	}

	// Resolve available models silently; keep the list for ensemble use.
	var availableModels []ollama.ModelInfo
	{
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := client.ListModels(ctx)
		cancel()
		if err == nil && len(models) > 0 {
			availableModels = models
			router.ResolveAll(models)
			for tier, model := range router.ResolvedSummary() {
				fmt.Printf("  %s%-8s%s → %s\n", colorGold, tier, colorReset, model)
			}
		}
	}

	// Load project brain.
	b := brain.New(root)
	if !b.Exists() {
		_ = b.Init()
	}
	brainContext := b.Load()
	conventions := verify.ParseConventions(b.ReadFile("CONVENTIONS.md"))
	if brainContext != "" {
		fmt.Printf("%s● project memory ready%s\n", colorGold, colorReset)
	}

	// Load skills — size budget scales with intent tier (applied per-turn below).
	// We only keep the skill count here; actual content is loaded per-turn
	// using LoadSkillsForTask so the most relevant skills come first.
	if n := b.SkillCount(); n > 0 {
		fmt.Printf("%s● %d skills loaded%s\n", colorGold, n, colorReset)
	}

	// Semantic embeddings — optional, used for memory retrieval.
	var embStore *embeddings.Store
	mantisDir := filepath.Join(root, ".mantis")
	if es, err := embeddings.Open(mantisDir, client); err == nil {
		embStore = es
		defer embStore.Close()
		// Background index brain files on first run.
		if embStore.Count() == 0 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_ = embStore.IndexBrainFiles(ctx, mantisDir)
			}()
		}
	}

	// NL dispatcher — codebase intelligence tools, called automatically.
	dispatcher := nl.New(root)
	if dispatcher != nil {
		defer dispatcher.Close()
	}

	// Session tracker.
	sess := session.New()
	usageTracker := usage.New()
	tlog := telemetry.New()
	sessID := fmt.Sprintf("%d", time.Now().UnixMilli())
	// Attach GitHub user and show telemetry notice once.
	if creds != nil && creds.GitHubUser != "" {
		tlog.SetUser(creds.GitHubUser, "v0.3.0")
	}
	if !tlog.IsDisabled() {
		fmt.Printf("%s● telemetry on · /telemetry off to disable%s\n", colorDim, colorReset)
	}

	// Ground truth — auto-index in background on first run.
	truthWriter := truth.New(root)
	if truthWriter.FileCount() > 0 {
		fmt.Printf("%s● %d files indexed%s\n", colorGold, truthWriter.FileCount(), colorReset)
	} else {
		go func() { _ = truthWriter.BuildFull(root) }()
	}

	// Conversation history — start with a default system prompt (will be rebuilt per-turn with tier context).
	systemPrompt := buildSystemPrompt(brainContext, b.LoadSkillsForTask("implement", 20000), router.TierCode)
	messages := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
	}

	printFooter()

	// One-shot mode: mantis "question"
	if cfg.InitialQuery != "" {
		return runOnce(cfg, client, sess, b, dispatcher, messages, truthWriter, usageTracker)
	}

	// Interactive REPL with readline (history, arrows, Ctrl+R, tab completion).
	slashCompleter := readline.NewPrefixCompleter(
		readline.PcItem("/help"),
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
		readline.PcItem("/quit"),
	)

	histFile := filepath.Join(os.Getenv("HOME"), ".mantis", "history")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "\033[38;5;214m❯\033[0m ",
		HistoryFile:       histFile,
		AutoComplete:      slashCompleter,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		// Fallback to plain scanner if readline init fails (e.g. non-TTY).
		return runWithScanner(cfg, client, sess, b, dispatcher, messages, truthWriter, usageTracker)
	}
	defer rl.Close()

	// Ctrl+C → graceful shutdown with session report.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		tlog.Flush()
		endSession(sess, b, messages, embStore)
		rl.Close()
		os.Exit(0)
	}()

	turn := 0
	for {
		printSep()
		fmt.Printf("%s  /help  /cost  /brain  /quit%s\n", colorDim, colorReset)
		line, err := rl.Readline()
		if err == readline.ErrInterrupt {
			if len(line) == 0 {
				break
			}
			continue
		} else if err == io.EOF {
			break
		}
		input := strings.TrimSpace(line)
		if input == "" {
			fmt.Printf("%s  (nothing to send — type a message or /help)%s\n", colorDim, colorReset)
			continue
		}

		// Slash commands.
		if strings.HasPrefix(input, "/") {
			if quit := handleSlashCommand(input, sess, b, &messages, client); quit {
				break
			}
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
		intent := router.Classify(input, hasImage)
		if cfg.ForceTier != "" {
			intent.Tier = parseTier(cfg.ForceTier)
		}
		model := router.ModelFor(intent.Tier)
		turn++
		turnStart := time.Now()
		showRouting(intent, model, turn, pipeline.ShouldRun(intent, input))
		printSep()

		// Update system prompt for this tier's reasoning depth.
		// Load skills relevant to the detected task type; budget scales with tier.
		skillsBudget := 20000 // chars: ~4–5 priority skills
		if intent.Tier <= router.TierFast {
			skillsBudget = 4000 // ~1 skill for trivial/fast turns
		}
		tierSkills := b.LoadSkillsForTask(string(intent.TaskType), skillsBudget)
		tierPrompt := buildSystemPrompt(brainContext, tierSkills, intent.Tier)
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

		// Semantic memory retrieval — find relevant past context.
		if embStore != nil && embStore.Count() > 0 {
			embCtx, embCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if chunks, err := embStore.Search(embCtx, input, 2); err == nil && len(chunks) > 0 {
				var memBuf strings.Builder
				memBuf.WriteString("\n[retrieved_memory]\n")
				for _, c := range chunks {
					if c.Score > 0.5 {
						memBuf.WriteString(fmt.Sprintf("(%s, relevance %.0f%%): %s\n", c.Source, c.Score*100, c.Text))
					}
				}
				if mem := memBuf.String(); len(mem) > 25 {
					userContent = userContent + "\n" + mem
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
			if ctxMsg := contextMessageFor(input, root, brainContext, truthWriter); ctxMsg != nil {
				userMsg = ctxMsg
			} else {
				userMsg = ollama.Message{Role: "user", Content: userContent}
			}
		} else {
			userMsg = ollama.Message{Role: "user", Content: userContent}
		}

		messages = append(messages, userMsg)

		// Proactive context compression — compress old turns before they cause overflow.
		messages = compressIfNeeded(messages, client)

		// ── Multi-stage SWE pipeline for complex build/implement requests ─────
		// Triggered before the single-model path so complex tasks get:
		//   plan (reason model) → code + tests (code model, parallel)
		if pipeline.ShouldRun(intent, input) {
			pipelineCtx, pipelineCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			pRes, pErr := pipeline.Run(
				pipelineCtx, client, input,
				buildSystemPrompt(brainContext, tierSkills, intent.Tier),
				pipeline.Options{AvailableModels: availableModels},
			)
			pipelineCancel()

			if pErr != nil {
				fmt.Printf("%s  [pipeline failed: %v — falling back to single model]%s\n\n",
					colorRed, pErr, colorReset)
				// Fall through to single-model path below.
			} else {
				totalTok := pRes.PromptTok + pRes.ComplTok
				fmt.Printf("%s◈ Mantis%s %s[pipeline · plan→code+tests · %d tokens]%s\n",
					colorCopper+colorBold, colorReset, colorDim, totalTok, colorReset)
				renderResponse(stripInternalBlocks(stripFileBlocks(pRes.Combined)))
				if wf := extractAndWriteFiles(pRes.Combined, root); len(wf) > 0 {
					printWrittenFiles(wf)
				}
				messages = append(messages, ollama.Message{Role: "assistant", Content: pRes.Combined})
				sess.Add(model, intent.Tier, pRes.PromptTok, pRes.ComplTok, hasImage)
				if warn := usageTracker.Add(totalTok, true, hasImage); warn != "" {
					fmt.Printf("%s%s%s\n\n", colorRed, warn, colorReset)
				}
				if pRes.PromptTok > 8000 {
					sess.WarnWaste("large prompt — use /file for specific files, not full directories")
				}
				continue
			}
		}

		// ── Single-model path (simple queries + pipeline fallback) ────────────

		// Multi-pass reasoning for complex queries (Reason/Heavy tiers).
		// Pass 1: silent analysis. Pass 2: solution informed by analysis.
		if intent.Tier == router.TierReason || intent.Tier == router.TierHeavy {
			messages = multiPassReasoning(client, model, intent.Tier, messages)
		}

		// Show spinner while model generates, then render formatted output.
		stopSpin := startSpinner(string(intent.TaskType))
		streamCtx, streamCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var rb strings.Builder
		var promptTok, completionTok int
		var streamErr error

		if intent.Tier == router.TierMax {
			promptTok, completionTok, streamErr = streamEnsemble(streamCtx, client, availableModels, messages, &rb)
		} else {
			promptTok, completionTok, streamErr = streamWithFallback(streamCtx, client, model, intent.Tier, messages, &rb)
		}
		streamCancel()
		stopSpin()

		if streamErr != nil {
			fmt.Printf("\n%s⚠ %v%s\n\n", colorRed, streamErr, colorReset)
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
			messages = messages[:len(messages)-1]
			continue
		}

		// Render the full response as formatted markdown.
		fmt.Printf("%s◈ Mantis%s\n", colorCopper+colorBold, colorReset)
		renderResponse(stripInternalBlocks(stripFileBlocks(rb.String())))

		// Write any file code blocks from the response to disk.
		// Append a system note so future turns know what's already written.
		wf := extractAndWriteFiles(rb.String(), root)
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

		if warn := usageTracker.Add(promptTok+completionTok,
			intent.Tier == router.TierHeavy, hasImage); warn != "" {
			fmt.Printf("%s%s%s\n\n", colorRed, warn, colorReset)
		}

		// Verify response against ground truth — re-prompt once on hallucination.
		if vr := verify.Check(rb.String(), truthWriter); !vr.Clean {
			// Attempt self-healing: re-prompt with corrections.
			corrections := verify.SuggestCorrections(vr.UnknownSymbols, truthWriter)
			if corrections != "" {
				fmt.Printf("%s  [verifying symbols… re-prompting for accuracy]%s\n", colorDim, colorReset)
				correctionMsg := fmt.Sprintf(
					"Your previous answer referenced symbols that don't exist in this project: %s\n"+
						"The actual symbols in this codebase are:\n%s\n"+
						"Please correct your answer using only real symbols.",
					strings.Join(vr.UnknownSymbols, ", "), corrections)

				retryMsgs := append(messages, ollama.Message{Role: "user", Content: correctionMsg})
				var rb2 strings.Builder
				retryCtx, retryCancel := context.WithTimeout(context.Background(), 3*time.Minute)
				pt2, ct2, err2 := streamWithFallback(retryCtx, client, model, intent.Tier, retryMsgs, &rb2)
				retryCancel()

				if err2 == nil && strings.TrimSpace(rb2.String()) != "" {
					// Replace original response with corrected one.
					messages[len(messages)-1] = ollama.Message{Role: "assistant", Content: rb2.String()}
					sess.Add(model, intent.Tier, pt2, ct2, false)
					fmt.Printf("%s◈ Mantis%s %s(corrected)%s\n", colorCopper+colorBold, colorReset, colorDim, colorReset)
					renderResponse(stripInternalBlocks(stripFileBlocks(rb2.String())))
					if wf := extractAndWriteFiles(rb2.String(), root); len(wf) > 0 {
						printWrittenFiles(wf)
					}

					if vr2 := verify.Check(rb2.String(), truthWriter); !vr2.Clean {
						fmt.Printf("%s%s%s\n\n", colorRed, vr2.Warning, colorReset)
					}
				} else {
					fmt.Printf("%s%s%s\n\n", colorRed, vr.Warning, colorReset)
				}
			} else {
				fmt.Printf("%s%s%s\n\n", colorRed, vr.Warning, colorReset)
			}
		}

		// Check conventions on AI output.
		if cr := verify.CheckConventions(rb.String(), conventions); !cr.Clean {
			fmt.Printf("%s%s%s\n", colorRed, cr.Warning, colorReset)
		}

		if promptTok > 8000 {
			sess.WarnWaste("large prompt — use /file for specific files, not full directories")
		}
	}

	tlog.Flush()
	endSession(sess, b, messages, nil)
	return nil
}

// runOnce handles a single non-interactive query: mantis "question"
func runOnce(cfg Config, client *ollama.Client, sess *session.Session,
	b *brain.Brain, dispatcher *nl.Dispatcher, messages []interface{},
	tw *truth.Writer, ut *usage.Tracker) error {

	// Normalize terminal error pastes into actionable fix requests.
	cfg.InitialQuery = normalizeTerminalInput(cfg.InitialQuery)

	hasImage := cfg.ImagePath != ""
	intent := router.Classify(cfg.InitialQuery, hasImage)
	if cfg.ForceTier != "" {
		intent.Tier = parseTier(cfg.ForceTier)
	}
	model := router.ModelFor(intent.Tier)
	showRouting(intent, model, 1, pipeline.ShouldRun(intent, cfg.InitialQuery))

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

	messages = append(messages, userMsg)

	fmt.Println()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var rb strings.Builder
	promptTok, completionTok, err := streamWithFallback(ctx, client, model, intent.Tier, messages, &rb)
	fmt.Println()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Printf("%s\n⚠ Ollama is not running locally.\n\nFix: start Ollama  →  ollama serve\n  or use cloud   →  export OLLAMA_API_KEY=<your_key>\n\nGet a free Ollama Cloud key at: https://ollama.com/cloud%s\n\n",
				colorRed, colorReset)
			return nil
		}
		return err
	}
	response := rb.String()
	sess.Add(model, intent.Tier, promptTok, completionTok, hasImage)
	_ = ut.Add(promptTok+completionTok, intent.Tier == router.TierHeavy, hasImage)
	if vr := verify.Check(response, tw); !vr.Clean {
		fmt.Printf("\n%s%s%s\n", colorRed, vr.Warning, colorReset)
	}
	fmt.Println(sess.Report())
	return nil
}

func handleSlashCommand(input string, sess *session.Session, b *brain.Brain,
	messages *[]interface{}, client *ollama.Client) (quit bool) {

	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help":
		printHelp()
	case "/reset":
		*messages = (*messages)[:1]
		fmt.Printf("%s● context cleared (brain memory kept)%s\n\n", colorGold, colorReset)
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
	default:
		fmt.Printf("%sunknown command — /help%s\n\n", colorDim, colorReset)
	}
	return false
}

// buildSystemPrompt returns the base system prompt (tier-independent).
// Brain context and tier-specific guidance are appended separately.
func buildSystemPrompt(brainCtx, skillsCtx string, tier router.Tier) string {
	var sb strings.Builder

	// ── Core identity & reasoning guidance ────────────────────────────────
	sb.WriteString(`You are Mantis, an expert AI coding assistant with deep knowledge of this project.

## How to think
- Break complex problems into steps before answering.
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

## File generation
When writing files, ALWAYS tag the opening fence with the filename using a colon:

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
- Use ` + "`lang:filename`" + ` for EVERY file — this is how Mantis writes them to disk.
- Extensionless files (Makefile, Dockerfile, Procfile) use ` + "`makefile:Makefile`" + ` format.
- Dot-files (.env, .gitignore) use ` + "`bash:.env`" + ` or ` + "`text:.gitignore`" + `.
- NEVER use indented code blocks (4-space indent) for files — they won't be written to disk.
- After all files, confirm: "✓ Created X files: Makefile, src/app.py, ..."
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
	if brainCtx != "" {
		sb.WriteString("\n## Project context (from persistent memory)\n")
		sb.WriteString(brainCtx)
		sb.WriteString("\n")
	}

	return sb.String()
}

// contextMessageFor returns a context injection message for this turn, or nil.
// Injects README for project questions, ContextSnippet for code questions.
func contextMessageFor(input, root string, brainContext string, tw *truth.Writer) interface{} {
	lower := strings.ToLower(input)
	isProjectQ := strings.Contains(lower, "project") || strings.Contains(lower, "what is") ||
		strings.Contains(lower, "what does") || strings.Contains(lower, "what are") ||
		strings.Contains(lower, "overview") || strings.Contains(lower, "purpose") ||
		strings.Contains(lower, "about this") || strings.Contains(lower, "describe")
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
		if brainContext != "" {
			parts = append(parts, "Project memory:\n"+brainContext)
		}
	}

	if isCodeQ && tw != nil {
		if snippet := tw.ContextSnippetN(8, 800); snippet != "" {
			parts = append(parts, "Codebase symbols (verified):\n"+snippet)
		}
	}

	if len(parts) == 0 {
		return nil
	}
	return ollama.Message{Role: "user", Content: "[context]\n" + strings.Join(parts, "\n\n") + "\n[/context]\n\nNow answer: " + input}
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
	if err := sess.Save(mantisDir, topic, summary); err == nil {
		fmt.Printf("%s● session saved%s\n", colorDim, colorReset)
	}

	// Embed session summary for semantic retrieval in future sessions.
	if embStore != nil && summary != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			id := fmt.Sprintf("session-%d", time.Now().Unix())
			_ = embStore.Add(ctx, id, "session", topic+"\n"+summary)
		}()
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
	var assistantTurns []string
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok && msg.Role == "assistant" && len(msg.Content) > 50 {
			assistantTurns = append(assistantTurns, msg.Content)
		}
	}
	if len(assistantTurns) == 0 {
		return ""
	}
	last := assistantTurns[len(assistantTurns)-1]
	if len(last) > 400 {
		last = last[:400] + "..."
	}
	return fmt.Sprintf("## Session (%s)\n\n%s\n", time.Now().Format("2006-01-02 15:04"), last)
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

// renderResponse renders markdown through glamour for clean terminal output.
// Falls back to plain print if glamour fails.
func renderResponse(content string) {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		w = 100
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dracula"),
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
// Returns a stop function that clears the spinner line.
func startSpinner(taskType string) func() {
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
		i := 0
		msgIdx := 0
		for {
			select {
			case <-done:
				fmt.Printf("\r\033[K") // clear spinner line
				return
			case <-time.After(80 * time.Millisecond):
				fmt.Printf("\r%s%s %s%s",
					colorDim, frames[i%len(frames)], msgs[msgIdx], colorReset)
				i++
				// advance faster through task leads (~1.5s each), slower through generic (~3s)
				interval := 19
				if msgIdx < len(taskLeads[taskType]) {
					interval = 19
				} else {
					interval = 38
				}
				if i%interval == 0 {
					msgIdx = (msgIdx + 1) % len(msgs)
				}
			}
		}
	}()
	return func() { close(done) }
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
	fmt.Printf(`%sSlash commands:%s
  /file <path>       inject a file into context
  /vision <path>     load an image (screenshot, diagram, error)
  /reset             clear conversation (keeps brain memory)
  /cost              token savings report
  /stats             usage analytics (tiers, latency, files)
  /telemetry on|off  enable / disable anonymous usage upload
  /brain             show project memory
  /save              save session to project memory now
  /model <tier>      switch tier: trivial · fast · code · reason · heavy · max
  /reject <reason>   log last suggestion as rejected approach
  /decision <text>   log an architecture decision
  /quit              exit (also Ctrl+C)

`, colorDim, colorReset)
}

func printFooter() {
	fmt.Printf("\n%s  /help  /cost  /brain  /quit%s\n\n", colorDim, colorReset)
}

func showRouting(intent router.Intent, model string, turn int, isPipeline bool) {
	graphTag := ""
	if intent.NeedsGraph {
		graphTag = fmt.Sprintf(" %s[graph]%s", colorGold, colorDim)
	}
	modelStyled := colorCopper + colorBold + model + colorReset + colorDim
	turnLabel := fmt.Sprintf("turn %d", turn)
	switch {
	case intent.Tier == router.TierMax:
		fmt.Printf("%s[%s · max · ensemble · multi-model%s]%s\n", colorDim, turnLabel, graphTag, colorReset)
	case isPipeline:
		fmt.Printf("%s[%s · pipeline · plan→code+tests · %s%s]%s\n",
			colorDim, turnLabel, modelStyled, graphTag, colorReset)
	default:
		fmt.Printf("%s[%s · %s · %s · %s%s]%s\n",
			colorDim, turnLabel, intent.Tier, intent.TaskType, modelStyled, graphTag, colorReset)
	}
}

// normalizeTerminalInput detects when the user pastes raw terminal output
// (shell errors, npm/go output) and rewrites it as an explicit fix request so
// the router picks the right tier and the model knows what to do.
func normalizeTerminalInput(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return input
	}

	// Patterns that indicate terminal error output, not a question.
	terminalErrorPatterns := []struct {
		marker  string
		label   string
	}{
		{"command not found", "shell error"},
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
			var buf strings.Builder
			pt, ct, err := client.StreamChat(ctx, model, messages, nil,
				func(chunk string) { buf.WriteString(chunk) })
			results[idx] = modelResult{model, buf.String(), pt, ct, err}
			mu.Lock()
			done++
			if err == nil {
				fmt.Printf("%s  [%d/%d ✓ %s]%s\n", colorGold, done, len(models), model, colorReset)
			} else {
				fmt.Printf("%s  [%d/%d ✗ %s]%s\n", colorRed, done, len(models), model, colorReset)
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
	spt, sct, err := client.StreamChat(ctx, synthModel, synthMessages, nil,
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
func streamWithFallback(ctx context.Context, client *ollama.Client, model string,
	tier router.Tier, messages []interface{}, rb *strings.Builder) (int, int, error) {

	tried := map[string]bool{}
	candidates := []string{model}
	candidates = append(candidates, router.PreferredModels(tier)...)

	for _, m := range candidates {
		if tried[m] {
			continue
		}
		tried[m] = true

		pt, ct, err := client.StreamChat(ctx, m, messages, nil,
			func(chunk string) { rb.WriteString(chunk) })

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

		return 0, 0, err // non-recoverable error, bail
	}
	return 0, 0, fmt.Errorf("no available model found for tier %s — run /models to see what's available", tier)
}

// trimToMinimal keeps the system message and the last 3 user+assistant messages.
// Used as a last resort when the full context exceeds the model's window.
func trimToMinimal(messages []interface{}) []interface{} {
	var sys interface{}
	var recent []interface{}
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok {
			if msg.Role == "system" {
				sys = m
			} else {
				recent = append(recent, m)
			}
		} else if img, ok := m.(ollama.ImageMessage); ok && img.Role == "user" {
			recent = append(recent, m)
		}
	}
	var out []interface{}
	if sys != nil {
		out = append(out, sys)
	}
	// Keep last 3 messages (user+assistant pairs) instead of just 1
	if len(recent) > 6 {
		recent = recent[len(recent)-6:]
	}
	out = append(out, recent...)
	return out
}

// multiPassReasoning performs a silent analysis pass before the main response.
// For complex queries (Reason/Heavy), the model first analyzes constraints and
// risks, then this analysis is injected as context for the solution pass.
func multiPassReasoning(client *ollama.Client, model string, tier router.Tier, messages []interface{}) []interface{} {
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

	analysisCtx, analysisCancel := context.WithTimeout(context.Background(), 90*time.Second)
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

// compressIfNeeded proactively compresses conversation history when it gets too large.
// Instead of waiting for a 400 error, it summarizes old turns at ~80% capacity.
// Estimates tokens as len/3.5 (LLaMA-family average for code).
func compressIfNeeded(messages []interface{}, client *ollama.Client) []interface{} {
	// Estimate total tokens in conversation.
	totalChars := 0
	for _, m := range messages {
		if msg, ok := m.(ollama.Message); ok {
			totalChars += len(msg.Content)
		} else if img, ok := m.(ollama.ImageMessage); ok {
			totalChars += len(img.Content)
		}
	}
	estimatedTokens := int(float64(totalChars) / 3.5)

	// Threshold: compress when conversation exceeds ~24K tokens (80% of 32K context).
	// Most Ollama models have 32K-128K context; this is conservative.
	const compressThreshold = 24000
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

	// Keep last 6 messages (3 user+assistant pairs), summarize the rest.
	if len(turns) <= 6 {
		return messages // not enough to compress
	}

	oldTurns := turns[:len(turns)-6]
	recentTurns := turns[len(turns)-6:]

	// Build summary of old turns.
	var summaryInput strings.Builder
	summaryInput.WriteString("Summarize this conversation in concise bullet points.\n")
	summaryInput.WriteString("Focus on: decisions made, code changes discussed, open questions, key findings.\n")
	summaryInput.WriteString("Keep it under 500 words.\n\n")
	for _, m := range oldTurns {
		if msg, ok := m.(ollama.Message); ok {
			role := msg.Role
			content := msg.Content
			if len(content) > 500 {
				content = content[:500] + "…"
			}
			summaryInput.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, content))
		}
	}

	// Use a fast model for summarization.
	summaryModel := router.ModelFor(router.TierFast)
	summaryMsgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a conversation summarizer. Output only bullet points."},
		ollama.Message{Role: "user", Content: summaryInput.String()},
	}

	var summaryBuf strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, _, err := client.StreamChat(ctx, summaryModel, summaryMsgs, nil,
		func(chunk string) { summaryBuf.WriteString(chunk) })
	cancel()

	if err != nil || strings.TrimSpace(summaryBuf.String()) == "" {
		// Summarization failed — fall back to trimToMinimal behavior.
		return trimToMinimal(messages)
	}

	fmt.Printf("%s  [compressed %d turns → summary]%s\n", colorDim, len(oldTurns), colorReset)

	// Rebuild: system + summary + recent turns.
	var compressed []interface{}
	if sys != nil {
		compressed = append(compressed, sys)
	}
	compressed = append(compressed, ollama.Message{
		Role:    "user",
		Content: "[conversation summary]\n" + summaryBuf.String() + "\n[/conversation summary]",
	})
	compressed = append(compressed, ollama.Message{
		Role:    "assistant",
		Content: "Understood. I have the conversation context. How can I help?",
	})
	compressed = append(compressed, recentTurns...)
	return compressed
}

func injectFile(path string, messages *[]interface{}) {
	data, err := os.ReadFile(path)
	if err != nil {
		cwd, _ := os.Getwd()
		data, err = os.ReadFile(filepath.Join(cwd, path))
		if err != nil {
			fmt.Printf("%sError reading %s: %v%s\n\n", colorRed, path, err, colorReset)
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
	tw *truth.Writer, ut *usage.Tracker) error {

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
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
			if quit := handleSlashCommand(input, sess, b, &messages, client); quit {
				break
			}
			continue
		}
		// Reuse one-shot handler for each turn.
		cfg.InitialQuery = input
		_ = runOnce(cfg, client, sess, b, dispatcher, messages, tw, ut)
		cfg.InitialQuery = ""
	}
	endSession(sess, b, messages, nil)
	return nil
}
