# Mantis

**Free, local-first AI coding assistant.** Uses Ollama (local or cloud) to deliver Claude Code-class capabilities at zero cost. Designed for engineers who want full control over their tools.

Mantis combines:
- An interactive AI REPL with 7-tier model routing, semantic memory, and convention enforcement
- A multi-stage code generation pipeline with parallel task execution and iterative build verification
- Multi-agent orchestration with parallel workers and 7 built-in tools
- Graph-aware repo analysis (`init`, `impact`, `find`, `lint`, `workspace`)
- Runtime + git intelligence (`trace`, `hotspots`, `risky`, `coupling`, `intent`)
- An LSP server and MCP server for IDE/tool integration
- Persistent project memory that survives across sessions (no cold starts)

---

## What Makes It Different

| Capability | Claude Code | Cursor | Copilot | **Mantis** |
|---|---|---|---|---|
| Cost | $20/mo | $20/mo | $10/mo | **$0** |
| Runs locally | ❌ | ❌ | ❌ | ✅ |
| Context amnesia prevention | ⚠ lossy | ⚠ re-index | ❌ | ✅ BRAIN.md + embeddings |
| Hallucination detection | ❌ | ⚠ LSP only | ❌ | ✅ Ground Truth Engine |
| Decision memory | ❌ | ❌ | ❌ | ✅ DECISIONS.log + REJECTED.md |
| Convention enforcement | ❌ | ❌ | ❌ | ✅ CONVENTIONS.md gate + re-prompt |
| Multi-agent orchestration | ⚠ internal | ❌ | ❌ | ✅ decompose → fan-out → synthesize |
| Iterative test loop | ⚠ manual | ❌ | ❌ | ✅ auto-detect runner, parse, fix |
| Temporal/blame intelligence | ❌ | ❌ | ❌ | ✅ git history analysis |
| Runtime trace analysis | ❌ | ❌ | ❌ | ✅ OTLP + pprof |
| Diff-based code generation | ✅ | ⚠ partial | ❌ | ✅ SEARCH/REPLACE edit blocks |
| Per-file diff approval | ❌ | ✅ | ❌ | ✅ colored diff preview + [Y/n] |
| LSP + MCP integration | N/A | N/A | ⚠ LSP | ✅ both |
| Cross-repo workspace | ❌ | ❌ | ❌ | ✅ unified graph |

---

## Current Status

- **v0.7.6** — active development, daily releases
- **28,000+ LOC** across 28 packages, 243+ tests
- **30 CLI commands**, **22 REPL slash commands**
- All tests pass: `go test ./...`
- Deployed via GitHub Actions + GoReleaser (macOS arm64/amd64)

---

## Install

### Homebrew
```bash
brew install seedhire/tap/mantis
```

### Install script
```bash
curl -fsSL https://raw.githubusercontent.com/seedhire/mantis/main/install.sh | sh
```

### Go install
```bash
go install github.com/seedhire/mantis/cmd/mantis@latest
```

### Build from source
```bash
git clone https://github.com/seedhire/mantis
cd Mantis
go build -o mantis ./cmd/mantis
```

---

## Quick Start

```bash
# 1) index your repo (creates .mantis/ + graph DB + ground truth)
mantis init

# 2) open interactive AI assistant
mantis

# 3) ask a one-shot question
mantis "why is checkout timing out?"

# 4) offline / no GitHub auth needed
mantis --offline
```

First interactive run triggers setup:
- GitHub login (optional with `--offline`)
- Ollama Cloud API key (optional — local Ollama works without it)

---

## Core Workflows

### 1. Chat + code in terminal

```bash
mantis                                           # interactive REPL
mantis --image error.png "what is wrong here?"  # multimodal
mantis --continue                                # resume last session
mantis --plan "build auth + session management" # plan-first mode
mantis --offline                                 # skip GitHub auth
```

### 2. Build entire projects

```bash
# Pipeline auto-triggers on complex build requests
mantis "build a REST API with express, typescript, JWT auth, and postgres"

# Plan mode: review architecture before implementation
mantis --plan "build a todo app with react and node"
```

The multi-stage pipeline:
- **PLAN** — TierReason decomposes into 6-10 tasks with file-level specs
- **CODE** — tasks run with live TUI progress
  - Task 0: project setup (config, manifests) → sequential
  - Task 1: data models + sealed types manifest → sequential
  - Tasks 2-N: implementation layers → parallel batches (max 3)
  - Each task receives actual file content from prior tasks
  - Build check (`autofix.Check`) + up to 3 fix retries
  - Stuck detection (same error twice → move on)
  - Content validation (rejects TODOs, stubs, placeholders)
  - Sealed types manifest prevents type redefinition across parallel workers
- **TESTS** — generate test files per task
- **VERIFY** — TestLoop runs tests and iteratively fixes failures

### 3. Graph-aware change safety

```bash
mantis init
mantis find processPayment
mantis impact processPayment --risk
mantis context processPayment --depth 3 --tokens 8000
mantis lint --strict --ci
```

### 4. Git history intelligence

```bash
mantis hotspots --days 90
mantis risky --days 90
mantis coupling src/checkout/service.ts
mantis intent src/checkout/service.ts
mantis spec-gaps
mantis todos
```

### 5. Runtime trace intelligence

```bash
mantis trace ingest traces.json
mantis trace hotpaths
mantis trace cold
mantis trace weight processPayment
```

### 6. Multi-repo workspace analysis

```bash
mantis workspace init ~/api ~/frontend ~/shared
mantis workspace find UserService
mantis workspace impact processPayment
mantis workspace stats
```

---

## Interactive Slash Commands

In the `mantis` REPL:

| Command | Description |
|---|---|
| `/help` | Command list |
| `/init` | Generate `MANTIS.md` from codebase scan |
| `/file <path>` | Inject file content (with nearby-file suggestions on error) |
| `/vision <path>` | Attach image for multimodal prompt |
| `/fetch <url>` | Fetch webpage into context |
| `/search <query>` | Web search (Tavily or DuckDuckGo fallback) |
| `/plan` | Toggle plan-before-code mode |
| `/context` | Show token budget breakdown + injected files |
| `/brain` | Show stored memory |
| `/save` | Save current session summary to memory |
| `/decision <text>` | Append architecture decision |
| `/reject <reason>` | Log rejected approach (never suggested again) |
| `/test [pkg]` | Iterative test-fix loop — auto-detects runner, parses failures, fixes |
| `/commit` | AI-assisted commit message generation + preview |
| `/pr` | Create GitHub PR from current branch |
| `/cost` | Show token usage |
| `/stats` | Session statistics |
| `/models` | List available models |
| `/telemetry on\|off` | Toggle anonymous telemetry |
| `/version` | Show version |
| `/quit` | Exit |

### Smart REPL Features

- **Dynamic file reading** — mention a file path and Mantis auto-reads it
- **Graph context injection** — dependency graph neighbors auto-included per message
- **Memory retrieval** — hybrid BM25+cosine RRF search surfaces relevant past context
- **Convention enforcement** — responses re-prompted up to 2× against `CONVENTIONS.md` rules with stuck detection
- **Hallucination detection** — function references verified against live ground truth; re-prompt loop on violations
- **Per-file diff approval** — shows colored diff preview + `[Y/n]` confirmation before writing any file
- **Edit block enforcement** — SEARCH/REPLACE edit blocks always win over whole-file rewrites
- **Test-fix routing** — "fix failing tests" auto-routes to the iterative TestLoop
- **Token tracking** — per-turn token display `◈ Mantis [+N tok · session: M]`
- **Model degradation warning** — shows per-tier degradation at startup with `ollama pull` hint
- **Offline mode** — `--offline` flag skips GitHub auth for local/air-gapped use

---

## Agent System

For high-impact changes (4+ files across 2+ packages), the multi-agent orchestrator activates:

- **Orchestrator** decomposes task into per-package sub-tasks using TierReason
- **Workers** run in parallel (max 5 iterations each) with full tool access:
  - `read_file`, `write_file`, `edit_file`, `run_bash`, `search_codebase`, `find_symbol`, `run_tests`
- **Synthesizer** combines worker summaries, prepends code blocks
- Workers communicate via `.mantis/AGENT_SCRATCH.json`
- `run_bash` uses an allowlist (go/npm/cargo/pytest/git prefixes) with 8,000-char output cap

---

## CLI Commands

```
init, watch, context
find, impact, dead, circular, graph, lint, tui
hotspots, risky, coupling, intent, spec-gaps, todos
workspace (init, find, impact, stats)
trace (ingest, hotpaths, cold, weight)
handoff, lsp, mcp
```

Global flags:

| Flag | Description |
|---|---|
| `--model <tier>` | Force routing tier (`trivial\|fast\|code\|reason\|heavy\|max\|vision`) |
| `--budget <tokens>` | Max session token budget |
| `--image <path>` | Attach image to query |
| `--plan` | Pause after plan stage before implementation |
| `--continue` | Resume most recent session |
| `--offline` | Skip GitHub auth gate (local/air-gapped use) |

---

## 7-Tier Model Routing

The router classifies every message into one of 7 tiers using accumulated keyword scoring + question-form dampeners + embedding-based kNN (97.7% accuracy):

| Tier | Default Model | Use Case |
|---|---|---|
| Trivial | `gemma3:4b` | Definitions, yes/no |
| Fast | `devstral-small-2:24b-cloud` | Quick code edits, 68% SWE-bench |
| Code | `glm-5:cloud` | Code generation, #1 open model 77.8% SWE-bench |
| Reason | `kimi-k2-thinking:cloud` | Multi-step reasoning, 99% HumanEval |
| Heavy | `glm-4.7:cloud` | Architecture analysis, 73.8% SWE-bench |
| Max | `glm-5:cloud` | Best all-round, complex tasks |
| Vision | `gemini-3-flash-preview:cloud` | Images, 1M context, 90.4% GPQA-Diamond |

---

## LSP Server

```bash
mantis lsp
```

JSON-RPC over stdio. Provides:
- Hover information with dependency context
- Document symbols from the AST graph
- Diagnostics from architecture lint rules
- Code lens for impact analysis

---

## MCP Server

```bash
mantis mcp
```

Model Context Protocol server for integration with other AI tools (Claude Desktop, etc.).

---

## Project Memory (`.mantis/`)

Running `mantis init` creates `.mantis/` in your repo:

| File | Purpose |
|---|---|
| `BRAIN.md` | Rolling context summary, updated each session |
| `DECISIONS.log` | Timestamped architecture decisions |
| `REJECTED.md` | Failed approaches the AI won't repeat |
| `CONVENTIONS.md` | Architecture rules auto-enforced on every response |
| `GROUND_TRUTH.json` | Live function signature snapshot for hallucination detection |
| `graph.db` | SQLite AST dependency graph |
| `embeddings.db` | SQLite semantic memory (FTS5 + cosine, hybrid BM25+RRF) |
| `sessions/` | Saved session data |
| `AGENT_SCRATCH.json` | Multi-agent worker communication |

---

## Configuration

### `.mantisrc.yml`

Used by `mantis lint` for architecture rules.

```yaml
version: 1
rules:
  - name: no-circular-dependencies
    type: built_in
    severity: error

  - name: max-file-dependencies
    type: built_in
    threshold: 15
    severity: warning

  - name: no-controller-db-access
    description: "Controllers must not import from db/ directly"
    from: "src/controllers/**"
    disallow_import: "src/db/**"
    severity: error
```

### Environment variables

| Variable | Purpose |
|---|---|
| `OLLAMA_API_KEY` | Ollama Cloud key (local Ollama works without it) |
| `MANTIS_TAVILY_KEY` | Enhanced `/search` via Tavily (DuckDuckGo fallback without it) |

---

## Development

```bash
make build      # build ./bin/mantis
make run        # build + run
make test       # go test ./...
make lint       # go vet ./...
make install    # go install with ldflags

go test ./internal/<package>/...         # single package
go test -run TestName ./internal/<pkg>/  # single test
```

---

## Architecture Overview

```
cmd/mantis         CLI entry point — 30 Cobra commands
internal/
  repl/            Interactive runtime, 22 slash commands, streaming (~4,300 LOC)
  pipeline/        Multi-stage code generation pipeline (~1,700 LOC)
  agent/           Multi-agent orchestrator, toolkit, test loop, test parser (~1,800 LOC)
  tui/             Bubble Tea dashboard, 12 screens (~2,600 LOC)
  intel/           Temporal, intent, trace analysis (~1,550 LOC)
  graph/           AST dependency graph + workspace (~1,150 LOC)
  lsp/             Language Server Protocol server (~1,100 LOC)
  router/          7-tier intent routing + embedding classifier (~950 LOC)
  verify/          Hallucination detection + convention gate (~730 LOC)
  mcp/             Model Context Protocol server (~700 LOC)
  brain/           Persistent project memory (~470 LOC)
  embeddings/      Hybrid semantic memory (FTS5 + cosine + BM25 + RRF) (~580 LOC)
  parser/          Tree-sitter multi-language AST parser (~650 LOC)
  context/         Token-budget context bundling + graph scoring (~330 LOC)
  ollama/          Streaming client + tool calling (~390 LOC)
  truth/           Ground truth index builder + querier (~300 LOC)
  web/             Web search + fetch (~330 LOC)
  autofix/         Build verification (~240 LOC)
  + 9 more support packages (openai, nl, session, setup, telemetry, usage, viz, config, termid)
```

### Request flow (interactive session)

```
User input
  → repl/          reads input, manages context, dispatches slash commands
  → router/        classifies into 1 of 7 tiers, selects best available model
  → context/       builds token-budgeted bundle (graph scoring + churn coupling + recency)
  → pipeline/      processes message, strips thinking preambles, compresses responses
  → verify/        checks output against GROUND_TRUTH.json + CONVENTIONS.md
  → repl/          re-prompts on violations (up to 2× hallucination, up to 2× convention)
  → files.go       per-file diff approval before any file write
  → session/       tracks tokens, prints cost-comparison report on exit
```

---

## License

MIT
