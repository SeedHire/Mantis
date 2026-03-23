package chatjson

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/brain"
	"github.com/seedhire/mantis/internal/embeddings"
	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
	"github.com/seedhire/mantis/internal/setup"
	"github.com/seedhire/mantis/internal/verify"
)

// Config holds startup options for the JSON chat mode.
type Config struct {
	// Offline skips GitHub auth gate.
	Offline bool
}

// session holds running state for a JSON chat session.
type session struct {
	client      *ollama.Client
	brain       *brain.Brain
	brainCtx    string
	conventions []verify.Convention
	messages    []interface{}
	mu          sync.Mutex // protects messages
	writeMu     sync.Mutex // protects stdout writes
	out         *json.Encoder
	cancelFn    context.CancelFunc // cancel current request
}

func (s *session) emit(r Response) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.out.Encode(r)
}

// Run starts the JSON Lines chat loop on stdin/stdout.
func Run(cfg Config) error {
	root, _ := os.Getwd()
	mantisDir := filepath.Join(root, ".mantis")

	// ── Auth ────────────────────────────────────────────────────────────
	if !cfg.Offline {
		if creds, _ := setup.Load(); creds != nil {
			setup.ApplyToEnv(creds)
		}
	}

	// ── Client ──────────────────────────────────────────────────────────
	client := ollama.NewFromEnv()

	// ── Model resolution ────────────────────────────────────────────────
	{
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := client.ListModels(ctx)
		cancel()
		if err == nil && len(models) > 0 {
			router.ResolveAll(models)
		} else if err != nil {
			// Write a status so the extension knows what happened.
			enc := json.NewEncoder(os.Stdout)
			_ = enc.Encode(Response{Type: "error", Error: fmt.Sprintf("cannot connect to Ollama: %v", err)})
			// Continue anyway — some commands don't need models.
		}
	}

	// ── Brain ───────────────────────────────────────────────────────────
	b := brain.New(root)
	if !b.Exists() {
		_ = b.Init()
	}
	brainCtx := b.LoadHot()
	conventions := verify.ParseConventions(b.ReadFile("CONVENTIONS.md"))

	// ── Embeddings (optional) ───────────────────────────────────────────
	if es, err := embeddings.Open(mantisDir, client); err == nil {
		defer es.Close()
		adapter := &routerEmbedAdapter{store: es}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_ = es.IndexBrainFiles(ctx, mantisDir)
			router.IndexRouterExamples(adapter)
		}()
	}

	// ── Session state ───────────────────────────────────────────────────
	systemPrompt := buildSystemPrompt(brainCtx)
	sess := &session{
		client:      client,
		brain:       b,
		brainCtx:    brainCtx,
		conventions: conventions,
		messages: []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt},
		},
		out: json.NewEncoder(os.Stdout),
	}

	// Signal ready.
	sess.emit(Response{Type: "status", Text: "ready"})

	// ── Main loop ───────────────────────────────────────────────────────
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			sess.emit(Response{Type: "error", Error: "invalid JSON: " + err.Error()})
			continue
		}

		switch req.Method {
		case "chat":
			handleChat(sess, req)
		case "command":
			handleCommand(sess, req)
		case "cancel":
			handleCancel(sess)
		default:
			sess.emit(Response{ID: req.ID, Type: "error", Error: "unknown method: " + req.Method})
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("stdin read: %w", err)
	}
	return nil
}

func handleChat(sess *session, req Request) {
	msg := strings.TrimSpace(req.Params.Message)
	if msg == "" {
		sess.emit(Response{ID: req.ID, Type: "error", Error: "empty message"})
		return
	}

	// Classify intent.
	intent := router.Classify(msg, false)
	model := router.ModelFor(intent.Tier)
	if model == "" {
		sess.emit(Response{ID: req.ID, Type: "error", Error: "no model available for tier " + intent.Tier.String()})
		return
	}

	sess.emit(Response{
		ID:    req.ID,
		Type:  "routing",
		Tier:  intent.Tier.String(),
		Model: model,
	})

	// Add user message.
	sess.mu.Lock()
	sess.messages = append(sess.messages, ollama.Message{Role: "user", Content: msg})
	msgs := make([]interface{}, len(sess.messages))
	copy(msgs, sess.messages)
	sess.mu.Unlock()

	// Create cancellable context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	sess.mu.Lock()
	sess.cancelFn = cancel
	sess.mu.Unlock()
	defer cancel()

	// Stream response.
	var fullResponse strings.Builder
	onChunk := func(token string) {
		fullResponse.WriteString(token)
		sess.emit(Response{ID: req.ID, Type: "token", Text: token})
	}

	promptTok, completionTok, err := sess.client.StreamChat(ctx, model, msgs, nil, onChunk)
	if err != nil {
		if ctx.Err() == context.Canceled {
			sess.emit(Response{ID: req.ID, Type: "done", Text: "[cancelled]"})
		} else {
			sess.emit(Response{ID: req.ID, Type: "error", Error: err.Error()})
		}
		return
	}

	// Record assistant message.
	sess.mu.Lock()
	sess.messages = append(sess.messages, ollama.Message{Role: "assistant", Content: fullResponse.String()})
	sess.mu.Unlock()

	sess.emit(Response{
		ID:     req.ID,
		Type:   "done",
		Tokens: promptTok + completionTok,
		Model:  model,
		Tier:   intent.Tier.String(),
	})
}

func handleCommand(sess *session, req Request) {
	name := req.Params.Name
	switch name {
	case "reset":
		sess.mu.Lock()
		sess.messages = []interface{}{
			ollama.Message{Role: "system", Content: buildSystemPrompt(sess.brainCtx)},
		}
		sess.mu.Unlock()
		sess.emit(Response{ID: req.ID, Type: "status", Text: "conversation reset"})

	case "brain":
		content := sess.brain.ReadFile("BRAIN.md")
		if content == "" {
			content = "(no brain file found — run /init in CLI)"
		}
		sess.emit(Response{ID: req.ID, Type: "done", Text: content})

	case "conventions":
		content := sess.brain.ReadFile("CONVENTIONS.md")
		if content == "" {
			content = "(no conventions file)"
		}
		sess.emit(Response{ID: req.ID, Type: "done", Text: content})

	default:
		sess.emit(Response{ID: req.ID, Type: "error", Error: "unknown command: " + name})
	}
}

func handleCancel(sess *session) {
	sess.mu.Lock()
	if sess.cancelFn != nil {
		sess.cancelFn()
	}
	sess.mu.Unlock()
}

// buildSystemPrompt builds a compact system prompt for the JSON chat mode.
func buildSystemPrompt(brainCtx string) string {
	var sb strings.Builder
	sb.WriteString(`You are Mantis, an expert AI coding assistant working inside the user's project directory.
- You have full knowledge of the project files.
- Be concise. Show code first, then explain briefly.
- Format code with correct language tags.
- Never invent function names — use exact signatures if known.
- NEVER start with "Sure!", "Of course!", "I'd be happy to".
- Do NOT add improvements beyond what was asked.
`)
	if brainCtx != "" {
		sb.WriteString("\n## Project Context\n")
		sb.WriteString(brainCtx)
		sb.WriteString("\n")
	}
	return sb.String()
}

// routerEmbedAdapter adapts *embeddings.Store to satisfy router.EmbedStore.
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
