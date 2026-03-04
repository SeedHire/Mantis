# Mantis

AI coding assistant with codebase intelligence and persistent project memory.

Mantis combines:
- an interactive AI REPL (`mantis`)
- graph-aware repo analysis (`init`, `impact`, `find`, `lint`, `workspace`)
- runtime + git intelligence (`trace`, `hotspots`, `risky`, `coupling`, `intent`)

## Current Status

- Active Go CLI project (`github.com/seedhire/mantis`)
- Commands and examples in this README are aligned with current code in `cmd/mantis/main.go`
- Test suite currently passes locally with `go test ./...` (run on 2026-03-04)

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

### 2) Graph-aware change safety
```bash
mantis init
mantis find processPayment
mantis impact processPayment --risk
mantis context processPayment --depth 3 --tokens 8000
mantis lint --strict --ci
```

### 3) Git history intelligence
```bash
mantis hotspots --days 90
mantis risky --days 90
mantis coupling src/checkout/service.ts
mantis intent src/checkout/service.ts
mantis spec-gaps
mantis todos
```

### 4) Runtime trace intelligence
```bash
mantis trace ingest traces.json
mantis trace hotpaths
mantis trace cold
mantis trace weight processPayment
```

### 5) Multi-repo workspace analysis
```bash
mantis workspace init ~/api ~/frontend ~/shared
mantis workspace find UserService
mantis workspace impact processPayment
mantis workspace stats
```

## Interactive Slash Commands

In `mantis` REPL:

- `/help` command list
- `/init` generate `MANTIS.md` from codebase scan
- `/file <path>` inject file content
- `/vision <path>` attach image for multimodal prompt
- `/fetch <url>` fetch webpage into context
- `/search <query>` web search (requires `MANTIS_TAVILY_KEY`)
- `/plan` toggle plan-before-code mode
- `/context` show token budget breakdown
- `/brain` show stored memory
- `/save` save current session summary to memory
- `/decision <text>` append architecture decision
- `/reject <reason>` log rejected approach
- `/test [pkg]` iterative test-fix loop
- `/commit` generate + preview commit message flow
- `/cost`, `/stats`, `/models`, `/telemetry on|off`, `/version`, `/quit`

## CLI Commands

Top-level commands:

- `init`, `watch`, `context`
- `find`, `impact`, `dead`, `circular`, `graph`, `lint`, `tui`
- `hotspots`, `risky`, `coupling`, `intent`, `spec-gaps`, `todos`
- `workspace` (`init`, `find`, `impact`, `stats`)
- `trace` (`ingest`, `hotpaths`, `cold`, `weight`)
- `handoff`

Global flags:

- `--model <tier>` force routing tier (`trivial|fast|code|reason|heavy|max|vision`)
- `--budget <tokens>` max session token budget
- `--image <path>` attach image to query
- `--plan` pause after plan stage before implementation
- `--continue` resume most recent session

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
- `MANTIS_TAVILY_KEY` enables `/search` web search
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

- `cmd/mantis` Cobra CLI and command wiring
- `internal/repl` interactive runtime, slash commands, routing, streaming
- `internal/router` 7-tier intent routing + model resolution + ensemble pools
- `internal/brain` persistent memory files in `.mantis/`
- `internal/graph` AST graph builder/query + workspace graph
- `internal/intel` impact, dead code, circular, temporal, runtime trace analysis
- `internal/context` token-budget context bundling
- `internal/verify` symbol/convention verification and correction loop
- `internal/ollama` model client for local/cloud inference and embeddings
- `internal/tui` Bubble Tea dashboard

## License

MIT
