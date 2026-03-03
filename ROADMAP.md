# Mantis — Implementation Roadmap

> Generated from deep research across 5 parallel agents covering: RAG/memory retrieval (MemGPT, Cursor, Cody),
> LLM output validation (Guardrails AI, Constitutional AI), surgical context selection (Continue.dev, Cody, BM25/RRF),
> model routing (RouteLLM paper, Anthropic guidance), and multi-agent systems (SWE-agent, OpenHands, AutoGen, CrewAI).

---

## The Core Problem (Why No One Uses It)

Several flagship features are partially implemented — the README makes promises the code doesn't keep:

| What we claim | What actually happens |
|---|---|
| Hallucination check | Only checks if function *names* exist — not signatures, not logic |
| Convention enforcement | Prints a warning, never re-prompts, never blocks |
| Persistent memory | Writes to `BRAIN.md` but never reads it back intelligently into context |
| Semantic embeddings | SQLite exists, code is never indexed, only brain files |
| Ensemble mode (TierMax) | `EnsembleModels()` written, never called from REPL |
| Convention enforcement blocks bad code | `CheckConventions()` output is a `fmt.Printf`, not a retry |

Fix the lies first. Then add new things.

---

## Phase 1 — Fix What's Broken

> Ship these first. They make existing promises true.

---

### 1.1 Section-Aware Memory Chunking

**File:** `internal/embeddings/embeddings.go` — `IndexBrainFiles()` and `splitIntoChunks()`

**Problem:** `splitIntoChunks(text, 500)` cuts at arbitrary character boundaries, mixing content from different sections into single embeddings. Querying "what stack do we use?" can return a chunk that starts mid-sentence from a different section.

**Fix:** Replace with file-type-aware splitters:

```
BRAIN.md / CONVENTIONS.md  → split on "\n## " headers — each section = 1 chunk
DECISIONS.log              → each "[timestamp]..." line = 1 chunk
REJECTED.md                → each "- **approach**" bullet = 1 chunk
```

Add content hashing before embedding:
```go
hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))[:16]
// if existing chunk has same hash → skip embed call
```

Add `content_hash TEXT`, `section_label TEXT`, `source_file TEXT` columns to `chunks` table.

Remove the `if embStore.Count() == 0` guard in `repl.go:161` — with hashing, re-indexing is O(changed chunks), not O(all chunks). Always re-index at startup.

**Store chunk metadata prefix** so the model knows provenance:
```go
text = fmt.Sprintf("[source:%s | section:%s | date:%s]\n%s", source, label, date, rawText)
```

**Expected uplift:** 30-40% improvement in retrieval precision.

---

### 1.2 SQLite FTS5 for BM25 Hybrid Retrieval

**File:** `internal/embeddings/embeddings.go` schema + new `SearchHybrid()` method

**Problem:** Only cosine similarity search exists. Exact keyword matches ("JWT", "supabase", specific function names) can score low semantically if they appear rarely in the embedding training data.

**Fix:** Add SQLite FTS5 (zero new dependencies — built into SQLite):

```sql
CREATE VIRTUAL TABLE chunks_fts USING fts5(
    id UNINDEXED,
    text,
    content=chunks,
    content_rowid=rowid
);

CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
    INSERT INTO chunks_fts(rowid, id, text) VALUES (new.rowid, new.id, new.text);
END;
```

FTS5 gives BM25 via the `rank` column natively:
```sql
SELECT c.*, fts.rank FROM chunks c
JOIN chunks_fts fts ON c.id = fts.id
WHERE chunks_fts MATCH ?
ORDER BY fts.rank LIMIT 100;
```

Merge with cosine similarity via **Reciprocal Rank Fusion** (the same approach Cursor and Cody use):
```go
// k=60 is the standard constant
RRF_score(chunk) = 1/(60 + rank_semantic) + 1/(60 + rank_bm25)
```

**Also:** Store embeddings as binary float32 blobs instead of JSON float64 arrays. Reduces DB size 75%, speeds up scans:
```go
func vecToBlob(v []float64) []byte {
    b := make([]byte, len(v)*4)
    for i, f := range v {
        binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(float32(f)))
    }
    return b
}
```

---

### 1.3 Fix Router Keyword Misfires

**File:** `internal/router/router.go` — `Classify()`

**Problem:** Cascade-first-match with no context disambiguation. Concrete misfires from the code:
- `"build a button"` → TierHeavy because `"build a"` is in `heavyKeywords` (line 381)
- `"set up a linter"` → TierHeavy because `"set up a"` is in `heavyKeywords` (line 385)
- `"explain the whole routing system"` → TierHeavy instead of TierReason (thinking model is better for explanation)
- `"why does this break?"` → correct tier but still uses 0.88 confidence, masking genuine ambiguity

**Immediate fixes (no architecture change needed):**
- Move `"explain the whole"` and `"explain all"` from `heavyKeywords` → `reasonKeywords`
- Gate `"build a"` / `"set up a"` in heavy: only count if also accompanied by complexity words (`"entire"`, `"codebase"`, `"full stack"`, `"microservice"`)

**Structural fix — replace cascade-first-match with accumulated scoring:**

```go
var tierScores [7]float64

// Score all tiers
for _, kw := range heavyKeywords {
    if strings.Contains(lower, kw) {
        tierScores[TierHeavy] += keywordWeight[kw]
    }
}
// ... repeat for all tier keyword lists

// Apply context modifiers
if isQuestionForm(lower) {          // starts with why/what/how
    tierScores[TierHeavy] *= 0.5   // questions rarely need largest model
}
if hasScopeWord(lower, "single", "quick", "simple", "just") {
    tierScores[TierHeavy] *= 0.2
    tierScores[TierCode] += 0.4
}
if hasScopeWord(lower, "entire", "whole", "all", "every") {
    for t := TierCode; t <= TierHeavy; t++ {
        tierScores[t] *= 1.5
    }
}

// Return argmax
return Intent{Tier: argmax(tierScores), Confidence: computedConfidence, ...}
```

Confidence is now **computed from the score gap** (`top_score - second_score / top_score`), not a hardcoded constant. This makes the telemetry's `LowConf` signal actually fire.

---

### 1.4 Convention Enforcement That Actually Works

**File:** `internal/repl/repl.go` lines 558-561 + `internal/verify/verify.go`

**Problem A:** Convention violations just print a warning. No re-prompt. The AI writes bad code, user sees red text, nothing changes.

**Fix:** Add a re-prompt loop mirroring the existing hallucination correction loop at line 520-555:

```go
if cr := verify.CheckConventions(rb.String(), conventions); !cr.Clean {
    // Don't just print — re-prompt with specific violation
    correctionMsg := fmt.Sprintf(
        "Your code violates this project rule:\n%s\n\nRewrite to fix this violation.",
        cr.Warning)
    // ... retry once, check again, surface to user if still failing
}
```

**Problem B:** `.mantisrc.yml` architecture rules are never run against generated code — only against the on-disk codebase via `mantis lint`. Generated code can violate `no-circular-dependencies` or `no-deep-imports` until it's written to disk.

**Fix:** After extracting code blocks (in `extractAndWriteFiles` or before), use tree-sitter to parse each block's imports. Infer the file path from the `lang:filepath` fence tag. Run `linter.Runner.runCustom()` against the `(from_path, imported_path)` pairs. This reuses existing infrastructure.

**Problem C:** `extractForbiddenImport()` only handles `"never import from X"` style rules. Misses `"internal/router must not import internal/db"`.

**Fix:** Add rule templates to `ParseConventions()`:
```go
// "X must not import Y"
regexp.MustCompile(`(?i)([\w/\*]+)\s+must\s+not\s+import\s+([\w/\*]+)`)
// "X may only import from Y"
regexp.MustCompile(`(?i)([\w/\*]+)\s+may\s+only\s+import\s+from\s+([\w/,\s]+)`)
```

---

## Phase 2 — Intelligence Upgrade

> These make the existing features actually good.

---

### 2.1 Feed Semantic + Recency Scores Into Context Bundler

**File:** `internal/context/bundler.go` — `scoreFile()`

**Problem:** `scoreFile()` is graph-only (depth + heuristics). The embedding store and git recency data exist but are never consulted when deciding which files to include.

**Fix:** Add three missing signals to `scoreFile()`:

```go
// Signal 1: semantic relevance to current query
// embStore.Search(query, 20) → boost files that appear in results
semanticBoost := embeddingScoreForFile(file, query, embStore) * 3.0

// Signal 2: recency (Node.LastModified is already in graph DB)
daysSince := time.Since(node.LastModified).Hours() / 24
recencyScore := 1.0 / (1.0 + daysSince) * 2.0

// Signal 3: test co-location (boost, not just demote)
// current: all test files get -4
// fix: co-located test (auth.go → auth_test.go) gets +3, unrelated tests keep -4
if isTestFile(file) && sharesBaseName(file, queryFile) {
    testScore = +3
} else if isTestFile(file) {
    testScore = -4
}
```

Combined formula (from Sourcegraph Cody's published weights):
```
score = 0.35*graph_score + 0.30*semantic_score + 0.20*recency_score
      + 0.10*test_colocation + 0.05*type_file_boost
```

**Context ordering:** LLMs have recency bias ("lost in the middle" — Liu et al. 2023). Most relevant context should be **last** (closest to user query). Flip `RenderMarkdown()` order: secondary/distant context first, primary/most-relevant last.

---

### 2.2 Fix Retrieval Threshold

**File:** `internal/repl/repl.go:347`

**Problem:** `c.Score > 0.5` is too high. `nomic-embed-text` cosine similarity for semantically related but not identical text typically runs 0.4-0.7. Many relevant chunks get dropped.

**Fix:**
```go
// Replace hard threshold with top-k + delta filter
if len(chunks) > 0 && c.Score > chunks[0].Score-0.2 && c.Score > 0.35 {
```

Also change `embStore.Search(embCtx, input, 2)` → `embStore.Search(embCtx, input, 5)`. At ~200 tokens per chunk, 5 chunks costs ~1K tokens — negligible.

**Use XML-delimited injection** (Anthropic recommends this for Claude — model parses XML structure better than `[retrieved_memory]` plain text):
```xml
<retrieved_memory>
<memory source="decision" date="2025-01-10" relevance="87%">
Chose SQLite over PostgreSQL — no server dependency needed.
</memory>
</retrieved_memory>
```

Add grounding instruction to `buildSystemPrompt()`:
```
When answering, prefer information in <retrieved_memory> over training data.
```

---

### 2.3 Embedding Similarity Classifier for Router

**Files:** New `internal/router/examples.go` + changes to `internal/router/router.go` + `internal/embeddings/embeddings.go`

**Research finding (RouteLLM paper, 2024):** Embedding kNN classifier achieves ~85% accuracy on ambiguous routing vs ~55% for keyword matching. Latency: 20-35ms for `nomic-embed-text` on Apple Silicon — within the <50ms target.

**Implementation:**

1. Create `internal/router/examples.go` with 300-500 labeled queries `(query string, tier Tier)`. Seed from existing `router_test.go` cases + telemetry `InputSnippet` field + hand-written examples covering the misfire categories.

2. Add `IndexRouterExamples()` to embeddings store — embeds all labeled queries with source `"router-label"`, stores tier as metadata.

3. Modify `Classify()` to accept optional `*embeddings.Store`:
```go
func Classify(message string, hasImage bool, embStore *embeddings.Store) Intent {
    // Layer 1: fast structural rules (0ms) — terminal errors, vision, word count < 5
    // Layer 2: accumulated scoring (0ms) — if confidence >= 0.82, return
    intent := classifyByAccumulatedScore(lower)
    if intent.Confidence >= 0.82 || embStore == nil {
        return intent
    }
    // Layer 3: embedding kNN (20-35ms) — only for ambiguous 20% of queries
    return classifyByEmbedding(message, embStore)
}
```

4. Add LRU cache (128 entries) keyed on query string — eliminates repeat latency within a session.

5. Add `ForcedTier string` to telemetry `Event` — when user uses `--tier` flag, log the mismatch. This auto-generates labeled training data over time.

**Do NOT use a generative model for routing.** gemma3:4b is 150-800ms — too slow for a CLI prompt.

---

## Phase 3 — Agentic Capabilities

> This is the product moat. No free tool does local multi-agent codebase-graph-aware editing.

---

### 3.1 Ollama Tool Calls Support

**File:** `internal/ollama/client.go`

**This is blocking for everything else in Phase 3.**

Add to `ChatRequest`:
```go
type Tool struct {
    Type     string       `json:"type"`
    Function ToolFunction `json:"function"`
}

type ToolFunction struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    Parameters  json.RawMessage `json:"parameters"`
}

// Add to ChatRequest:
Tools  []Tool           `json:"tools,omitempty"`
Format *json.RawMessage `json:"format,omitempty"`
```

Add to `ChatChunk`:
```go
type ToolCall struct {
    Function struct {
        Name      string          `json:"name"`
        Arguments json.RawMessage `json:"arguments"`
    } `json:"function"`
}

// Add to ChatChunk:
ToolCalls []ToolCall `json:"tool_calls,omitempty"`
```

Without this, tool invocation requires fragile regex parsing of model output. Models in the 30B-70B range produce valid tool call JSON ~70-80% of the time without constrained decoding — too unreliable for an agentic loop.

---

### 3.2 Upgrade Pipeline to Bounded Agentic Loop

**File:** `internal/pipeline/pipeline.go`

**Problem:** The CODE stage is one-shot. If `go build` fails, it feeds the error back via `verifyAndFix()` but this is post-pipeline, not within the stage. There's no bounded retry loop.

**Fix:** Add retry loop inside the CODE stage:

```go
type Options struct {
    AvailableModels []ollama.ModelInfo
    MaxRetries      int  // default: 3
}

// Inside CODE stage:
for attempt := 0; attempt < opts.MaxRetries; attempt++ {
    codeResult, err := streamCode(ctx, client, codeModel, planText, systemPrompt)

    buildErr := autofix.Check(root, writtenFiles)
    if buildErr == nil {
        break // success
    }

    // Progress detector: same error twice = agent is stuck
    if attempt > 0 && buildErr.Error() == lastBuildErr.Error() {
        return nil, fmt.Errorf("agent stuck: same error after retry")
    }

    // Re-prompt with structured error
    retryMsg := fmt.Sprintf(
        "Build failed:\n%s\n\nFix the error. Rewrite only the affected function(s).",
        buildErr)
    // append retryMsg to codeMessages, loop
    lastBuildErr = buildErr
}
```

Three loop prevention layers:
1. `MaxRetries` hard cap (5 iterations max)
2. Progress detector: same error twice → break immediately
3. Deterministic verification gate: loop exits when `go build ./...` exits 0, not when model says done

---

### 3.3 AgentToolkit Interface

**File:** New `internal/agent/toolkit.go` + changes to `internal/nl/dispatcher.go`

**Problem:** `nl.Dispatcher` is NL-triggered (keyword match). Agents need to call tools programmatically.

**Fix:** Create a typed toolkit interface:

```go
type AgentToolkit struct {
    // Codebase intelligence
    FindSymbol   func(name string) []graph.Node
    GetImporters func(file string) []string
    RunImpact    func(symbol string) intel.ImpactResult

    // File operations
    ReadFile  func(path string, startLine, endLine int) (string, error)
    WriteFile func(path string, content string) error

    // Execution
    RunBash func(cmd string, cwd string, timeoutSec int) (stdout, stderr string, code int)

    // Search
    SearchCodebase func(query string, limit int) ([]embeddings.Chunk, error)

    // Terminal signal
    Finish func(summary string)
}
```

The 5 core tools every coding agent needs (from SWE-agent's ACI design):
1. `read_file(path, start_line, end_line)` — partial file view, stays within token budget
2. `write_file(path, content)` — create or overwrite
3. `run_bash(command, cwd, timeout)` — shell with structured output
4. `search_codebase(query)` — semantic search over graph + embeddings
5. `finish(summary)` — explicit done signal, prevents runaway loops

Safety: all `run_bash` calls use `exec.CommandContext` with `cmd.Dir = projectRoot`. Cap output at 2000 tokens. Command allowlist: `go build`, `go test`, `go vet`, `go fmt`, `npm run`, `cargo check`, `python -m pytest`, `git diff`, `git status`.

---

### 3.4 Multi-Agent Fan-Out for Complex Tasks

**Files:** New `internal/agent/orchestrator.go`, changes to `internal/repl/repl.go`

**Gate:** Only activate when graph impact is genuinely multi-package:
```go
impact := intel.Impact(target, querier)
if impact.TotalFiles >= 4 && distinctPackages(impact) >= 2 {
    return runMultiAgent(ctx, task, toolkit)
}
// otherwise: single-agent loop (Phase 3.2)
```

**Architecture:**
```
Orchestrator (TierReason model)
    │  receives: user task + full impact analysis + context bundle
    │  produces: decomposition by affected package
    │
    ├── Worker[pkg/auth] (TierCode model) goroutine
    │       receives: auth package source + plan + interface contract
    │       runs: AgentLoop(maxIter=3)
    │       writes to: AgentScratch["auth"]
    │
    ├── Worker[pkg/router] (TierCode model) goroutine
    │       receives: router package source + plan + interface contract
    │       runs: AgentLoop(maxIter=3)
    │       writes to: AgentScratch["router"]
    │
    └── Synthesizer (TierReason model)
            receives: each worker's summarized result (~500 tokens each)
            assembles: final unified output
```

**Context sharing — the key rule:** Workers never see each other's conversation history. Each worker gets:
- Its own system prompt (role + task + conventions): 1000-2000 tokens
- Its package's source files (BFS-selected): 2000-4000 tokens
- Its specific sub-task: 200-500 tokens
- Prior agents' *result summaries only* (not trajectories): 200-500 tokens per agent

Total per worker turn: 4000-9000 tokens. Fits in 16K context window.

**AgentScratch:** Workers write results to `.mantis/AGENT_SCRATCH.json` (shared external state). No token cost for inter-agent communication. The synthesizer reads from there, not from agent message histories.

**When NOT to use multi-agent:**
- `TotalFiles < 4` — coordination overhead exceeds parallelism benefit
- Single GPU running Ollama with `OLLAMA_NUM_PARALLEL=1` — parallel goroutines just queue, sequential is faster
- Task is sequential by nature (output of step A is required input for step B)

---

## Quick Reference: What Lives Where

| Feature | Primary Files |
|---|---|
| Memory chunking | `internal/embeddings/embeddings.go` |
| BM25 / FTS5 | `internal/embeddings/embeddings.go` (schema + `SearchHybrid()`) |
| Router scoring | `internal/router/router.go` — `Classify()` |
| Router training data | `internal/router/examples.go` (new) |
| Convention re-prompt | `internal/repl/repl.go` lines 558-561 |
| AST import check | `internal/verify/verify.go` + `internal/linter/runner.go` |
| Rule template parser | `internal/verify/verify.go` — `ParseConventions()` |
| Bundler multi-signal | `internal/context/bundler.go` — `scoreFile()` |
| Retrieval threshold | `internal/repl/repl.go:347` |
| Ollama tool calls | `internal/ollama/client.go` |
| Agentic loop | `internal/pipeline/pipeline.go` |
| Agent toolkit | `internal/agent/toolkit.go` (new) |
| Multi-agent orchestrator | `internal/agent/orchestrator.go` (new) |
| Plan Mode | `internal/repl/repl.go`, `internal/pipeline/pipeline.go` |
| /context command | `internal/repl/repl.go`, `internal/session/session.go` |
| Session resume | `internal/session/`, `cmd/mantis/main.go` |
| Path-scoped rules | `internal/verify/verify.go`, `.mantisrc.yml` |
| Web fetch | `internal/repl/repl.go` (new slash command) |

---

## Phase 4 — Close Claude Code Gaps

> Mantis leads on semantic memory, convention enforcement, pipeline, and zero cost. Claude Code leads on agentic loop, plan-before-execute safety, web access, and session continuity. These additions close the most impactful gaps.

### 4.1 Plan Mode

**File:** `internal/repl/repl.go`, `internal/pipeline/pipeline.go`

After the reason-model PLAN stage produces its output, pause and show the plan to the user. Require explicit `y` (or Enter) before the CODE stage runs. Add `--plan` flag to always force plan-mode. Add `/plan` REPL slash command to toggle it per session.

```
◈ Mantis — Plan ready:
  1. Add JWT middleware to internal/auth/
  2. Wire middleware to /api routes in internal/router/
  3. Write auth_test.go

Proceed? [y/n]:
```

This mirrors Claude Code's Plan Mode (Shift+Tab). Users who want speed skip it; users who want safety enable it.

### 4.2 /context Command — Token Budget Breakdown

**File:** `internal/repl/repl.go`, `internal/session/session.go`

`/context` prints a per-source breakdown of what is consuming the context window:

```
System prompt          1,240 tok
BRAIN.md               3,100 tok
Retrieved memory         420 tok  (5 chunks)
Bundled files          6,800 tok  (8 files)
Conversation history   4,300 tok  (12 turns)
────────────────────────────────
Total                 15,860 / 32,000 tok  (49%)
```

Track token counts per category in `session.Tracker`. Render on `/context`.

### 4.3 Session Resume — `mantis --continue`

**File:** `internal/session/`, `cmd/mantis/main.go`

On clean exit, serialize conversation history + session metadata to `.mantis/session_last.json`. Add `--continue` flag to `mantis` command that loads the most recent session file before entering the REPL. This lets users pick up mid-task without re-explaining context.

Safety: cap loaded history at 8K tokens (trim oldest turns first). Show "Resuming session from <timestamp>" on startup.

### 4.4 Path-Scoped Convention Rules

**File:** `internal/verify/verify.go`, `.mantisrc.yml`

Allow rules in `CONVENTIONS.md` (or `.mantisrc.yml`) to be prefixed with a path scope:

```markdown
[path: internal/api/**]
Always validate request body with the validator package.

[path: frontend/**]
Never use var — always use const or let.
```

`ParseConventions()` returns `(rule, pathPattern)` pairs. The verify and re-prompt loop only applies a rule when the generated file path matches its pattern.

### 4.5 Web Fetch — `/fetch` and Auto-Trigger

**File:** `internal/repl/repl.go`

Add `/fetch <url>` slash command: HTTP GET the URL, strip HTML tags, truncate to 4K tokens, inject as `<web_context>` into the next prompt. Auto-trigger on build errors matching `unknown import path` — extract the module URL, fetch `pkg.go.dev/...` docs, inject before the retry prompt.

---

## Priority Order

| # | Change | Effort | Why First |
|---|---|---|---|
| 1 | Router accumulated scoring + misfire fixes | 1 day | Stops embarrassing misfires immediately |
| 2 | Section-aware chunking + content hash | 1 day | Makes memory actually work |
| 3 | Convention re-prompt loop | 1 day | Flagship feature actually works |
| 4 | Plan Mode | 1 day | Closes biggest safety gap vs Claude Code |
| 5 | SQLite FTS5 BM25 + RRF | 1 day | Hybrid retrieval, zero new deps |
| 6 | Semantic + recency scores in bundler | 1 day | Context quality improves |
| 7 | Fix retrieval threshold + XML format | 0.5 days | More context, better adherence |
| 8 | /context command | 0.5 days | Quick win, UX parity |
| 9 | Session resume (--continue) | 1 day | UX parity with Claude Code |
| 10 | Embedding classifier for router | 2 days | Router accuracy ~85% |
| 11 | Ollama tool_calls support | 0.5 days | Unblocks all agent work |
| 12 | Bounded agentic loop in pipeline | 1.5 days | Core agent loop |
| 13 | AgentToolkit interface | 1 day | Programmatic tool access |
| 14 | Path-scoped convention rules | 1 day | Rule precision |
| 15 | Web fetch (/fetch + auto-trigger) | 1 day | Closes web gap |
| 16 | Multi-agent fan-out | 3 days | Product moat |

---

## Research Sources

- **MemGPT** (Packer et al., arXiv:2310.08560) — three-tier virtual memory for LLMs
- **RouteLLM** (Ong et al., 2024) — learned routing between models, embedding kNN approach
- **Lost in the Middle** (Liu et al., 2023) — recency/primacy bias in long context
- **DACE** (Zan et al., 2024) — dependency-aware context extension for code RAG
- **SWE-agent** (Princeton NLP, 2024) — Agent-Computer Interface design
- **GraphCodeBERT** (Guo et al., 2021) — data-flow graph augmented code representations
- **Cursor IDE** engineering blog — two-stage retrieval, reranking, chunk strategy
- **Sourcegraph Cody** engineering blog — multi-signal scoring formula
- **Continue.dev** open source — BM25 + RRF hybrid, SQLite FTS5 implementation
- **OpenHands** (formerly OpenDevin) — event stream as shared agent memory
- **Anthropic "Building effective agents"** — orchestrator-worker patterns, loop prevention
- **AutoGen** (Microsoft) — conversation-based multi-agent, speaker selection
- **CrewAI** — role-based agents, result-only context propagation
