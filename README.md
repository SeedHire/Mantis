# Mantis

Free, local-first AI coding assistant with codebase intelligence, persistent project memory, and agentic code generation.

Mantis combines:
- an interactive AI REPL with 7-tier model routing and multi-pass reasoning
- a multi-stage code generation pipeline with iterative build verification
- agentic task execution with parallel workers and tool use
- graph-aware repo analysis (`init`, `impact`, `find`, `lint`, `workspace`)
- runtime + git intelligence (`trace`, `hotspots`, `risky`, `coupling`, `intent`)
- an LSP server and MCP server for IDE/tool integration

## Current Status

- Active Go CLI project (`github.com/seedhire/mantis`)
- 23,800+ LOC across 27 packages, 243 tests across 24 test files
- 30 CLI commands, 22 REPL slash commands
- Test suite passes locally with `go test ./...`

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

## Quick Start

```bash
# 1) index your repo (creates .mantis/ + graph DB)
mantis init

# 2) open interactive assistant
mantis

# 3) ask one-shot question
mantis "why is checkout timing out?"
```

First interactive run triggers setup:
- GitHub login is required
- Ollama Cloud API key is optional (local Ollama is supported)

## Core Workflows

### 1) Chat + code in terminal
```bash
mantis
mantis --image error.png "what is wrong in this UI?"
mantis --continue
mantis --plan "build auth + session management"
```

### 2) Build entire projects
```bash
# Pipeline auto-triggers on complex requests
mantis "build a REST API with express, typescript, JWT auth, and postgres"

# Plan mode: review architecture before implementation
mantis --plan "build a todo app with react and node"
```

The pipeline:
- **PLAN** stage decomposes into tasks with a reasoning model
- **CODE** stage executes tasks in parallel with live progress
- Each task gets actual file content from prior tasks (not just filenames)
- `npm install` / `go mod tidy` runs automatically after setup
- Build verification + up to 3 fix retries per task
- Content validation rejects placeholder/stub code
- **TEST** stage generates and verifies tests

### 3) Graph-aware change safety
```bash
mantis init
mantis find processPayment
mantis impact processPayment --risk
mantis context processPayment --depth 3 --tokens 8000
mantis lint --strict --ci
```

### 4) Git history intelligence
```bash
mantis hotspots --days 90
mantis risky --days 90
mantis coupling src/checkout/service.ts
mantis intent src/checkout/service.ts
mantis spec-gaps
mantis todos
```

### 5) Runtime trace intelligence
```bash
mantis trace ingest traces.json
mantis trace hotpaths
mantis trace cold
mantis trace weight processPayment
```

### 6) Multi-repo workspace analysis
```bash
mantis workspace init ~/api ~/frontend ~/shared
mantis workspace find UserService
mantis workspace impact processPayment
mantis workspace stats
```

## Interactive Slash Commands

In `mantis` REPL:

| Command | Description |
|---------|-------------|
| `/help` | Command list |
| `/init` | Generate `MANTIS.md` from codebase scan |
| `/file <path>` | Inject file content |
| `/vision <path>` | Attach image for multimodal prompt |
| `/fetch <url>` | Fetch webpage into context |
| `/search <query>` | Web search (Tavily or DuckDuckGo fallback) |
| `/plan` | Toggle plan-before-code mode |
| `/context` | Show token budget breakdown |
| `/brain` | Show stored memory |
| `/save` | Save current session summary to memory |
| `/decision <text>` | Append architecture decision |
| `/reject <reason>` | Log rejected approach |
| `/test [pkg]` | Iterative test-fix loop |
| `/commit` | Generate + preview commit message flow |
| `/pr` | Create GitHub PR from current branch |
| `/cost` | Show token usage |
| `/stats` | Session statistics |
| `/models` | List available models |
| `/telemetry on\|off` | Toggle anonymous telemetry |
| `/version` | Show version |
| `/quit` | Exit |

### Smart Features

- **Dynamic file reading** — mention a file path in your message, Mantis auto-reads it
- **Graph context injection** — related files from the dependency graph are automatically included
- **Memory retrieval** — semantic search surfaces relevant past decisions and brain context
- **Convention enforcement** — responses checked against `CONVENTIONS.md` rules
- **Hallucination detection** — function references verified against live ground truth
- **Test-fix routing** — "fix failing tests" auto-routes to the iterative test loop

## CLI Commands

Top-level commands:

- `init`, `watch`, `context`
- `find`, `impact`, `dead`, `circular`, `graph`, `lint`, `tui`
- `hotspots`, `risky`, `coupling`, `intent`, `spec-gaps`, `todos`
- `workspace` (`init`, `find`, `impact`, `stats`)
- `trace` (`ingest`, `hotpaths`, `cold`, `weight`)
- `handoff`, `lsp`, `mcp`

Global flags:

- `--model <tier>` force routing tier (`trivial|fast|code|reason|heavy|max|vision`)
- `--budget <tokens>` max session token budget
- `--image <path>` attach image to query
- `--plan` pause after plan stage before implementation
- `--continue` resume most recent session

## Pipeline Architecture

The multi-stage pipeline handles complex build requests ("build an app", "create a REST API"):

```
User request
  → PLAN (TierReason): decompose into 6-10 tasks, identify files + architecture
  → CODE (TierCode): execute tasks with live TUI progress
      Task 0: project setup (config, manifests) → sequential
      Task 1: data models & types → sequential
      Tasks 2-N: implementation layers → parallel batches (max 3)
      Per task:
        - Receives actual file content from prior tasks (types, interfaces, exports)
        - Build check (autofix.Check) after each task
        - Up to 3 fix retries with stuck detection (same error twice → move on)
        - Content validation (rejects TODOs, stubs, placeholders)
      After task 0: auto-installs dependencies (npm/go/pip)
  → TESTS (TierCode): generate test files
  → VERIFY (TestLoop): run tests, iteratively fix failures
```

## Agent System

For high-impact changes (4+ files across 2+ packages), the multi-agent orchestrator activates:

- **Orchestrator** decomposes task into per-package sub-tasks
- **Workers** execute in parallel with tool access (read_file, write_file, edit_file, run_bash, search_codebase, find_symbol, run_tests)
- **Synthesizer** combines worker outputs into a coherent result
- Workers communicate via `.mantis/AGENT_SCRATCH.json`

## LSP Server

```bash
mantis lsp
```

Provides IDE integration via Language Server Protocol:
- Hover information with dependency context
- Document symbols from the AST graph
- Diagnostics from architecture lint rules
- Code lens for impact analysis

## MCP Server

```bash
mantis mcp
```

Model Context Protocol server for integration with other AI tools.

## Project Memory Files

Running `mantis init` creates `.mantis/` in your repo. Common files:

- `BRAIN.md` rolling context summary
- `DECISIONS.log` architecture decisions
- `REJECTED.md` rejected approaches
- `CONVENTIONS.md` project rules used in response checks
- `GROUND_TRUTH.json` symbol/signature snapshot for verification
- `graph.db` dependency graph database
- `embeddings.db` semantic memory index
- `sessions/` saved session data
- `AGENT_SCRATCH.json` multi-agent communication

## Configuration

### `.mantisrc.yml`

Used by `mantis lint` for architecture rules.

Example:

```yaml
version: 1
rules:
  - name: no-circular-dependencies
    type: built_in
    severity: error
```

### Environment variables

- `OLLAMA_API_KEY` optional Ollama Cloud key (local Ollama works without it)
- `MANTIS_TAVILY_KEY` enables enhanced `/search` web search (DuckDuckGo fallback works without it)
- `SUPABASE_ANON_KEY` optional telemetry/setup tracking key at build/runtime

## Development

```bash
make build      # build ./bin/mantis
make run        # build + run
make test       # go test ./...
make lint       # go vet ./...
make install    # go install with ldflags
```

Direct command:

```bash
go test ./...
```

## Architecture Overview

```
cmd/mantis         CLI entry point, 30 Cobra commands
internal/
  repl/            Interactive runtime, slash commands, streaming (4,272 LOC)
  tui/             Bubble Tea dashboard, 12 screens (2,615 LOC)
  agent/           Multi-agent orchestrator, toolkit, test loop (1,831 LOC)
  pipeline/        Multi-stage code generation pipeline (1,721 LOC)
  intel/           Temporal, intent, trace analysis (1,548 LOC)
  graph/           AST dependency graph + workspace (1,133 LOC)
  lsp/             Language Server Protocol server (1,089 LOC)
  router/          7-tier intent routing + embedding classifier (946 LOC)
  verify/          Hallucination detection + convention gate (723 LOC)
  mcp/             Model Context Protocol server (696 LOC)
  brain/           Persistent project memory (679 LOC)
  parser/          Tree-sitter multi-language parser (653 LOC)
  embeddings/      Semantic memory with hybrid search (571 LOC)
  context/         Token-budget context bundling (396 LOC)
  ollama/          Streaming client + tool calling (392 LOC)
  web/             Web search + fetch (334 LOC)
  autofix/         Build verification (243 LOC)
  viz/             D3 graph visualization (178 LOC)
  + 9 more support packages
```

## License

MIT
