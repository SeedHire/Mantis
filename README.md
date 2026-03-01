# Mantis

**Free AI coding assistant. Better than Claude Code.**

No API key. No monthly bill. Runs on [Ollama Cloud](https://ollama.com/cloud) free tier or your local machine.
Built for students with GitHub Education — you already have the tools, now you have the API.

```bash
$ mantis
```

That's it. You're in.

---

## Why Mantis

| | Claude Code | Copilot | Mantis |
|---|---|---|---|
| Cost | ~$30/mo API | $10/mo | **$0** |
| Multimodal | ✓ | ✗ | ✓ |
| Persistent memory | ✗ | ✗ | ✓ |
| Knows your codebase | Partial | Partial | **Yes** |
| Cross-repo graph | ✗ | ✗ | ✓ |
| Hallucination check | ✗ | ✗ | ✓ |
| Convention enforcement | ✗ | ✗ | ✓ |
| Semantic embeddings | ✗ | ✗ | ✓ |
| Token waste tracker | ✗ | ✗ | ✓ |
| Local / offline | ✗ | ✗ | ✓ |

---

## Install

```bash
go install github.com/seedhire/mantis/cmd/mantis@latest
```

Or build from source:
```bash
git clone https://github.com/seedhire/mantis
cd mantis && make install
```

Set your Ollama Cloud key (optional — falls back to local Ollama without it):
```bash
export OLLAMA_API_KEY=your_key_here
```

---

## Usage

```bash
mantis                          # open interactive AI session
mantis "why does auth break?"   # one-shot question
mantis --model heavy "redesign the payments module"
mantis --image screenshot.png   # multimodal — paste a screenshot
```

### Slash commands inside the session
```
/file src/auth.go    inject a file into context
/vision error.png    analyze a screenshot or diagram
/reset               clear context (brain memory kept)
/cost                token savings report
/brain               view project memory
/save                save session to memory now
/reject <reason>     log last suggestion as rejected
/decision <text>     log an architecture decision
/quit                exit
```

---

## How it works

Mantis runs a **7-tier model router** — every message is classified by intent and sent to the best available model:

| Tier | Used for | Example models |
|---|---|---|
| Trivial | one-liners, definitions | gemma3:4b |
| Fast | short code questions | gemma3:12b |
| Code | implement, debug, refactor | devstral-small-2:24b |
| Reason | architecture, deep analysis | kimi-k2-thinking |
| Heavy | multi-file, complex design | devstral-2:123b |
| Max | ensemble: 3 specialists + synthesis | (auto-selected trio) |
| Vision | screenshots, diagrams | qwen3-vl |

Models are auto-resolved from your Ollama model list — no manual config needed. Quantized variants are preferred for speed tiers.

### Project memory
On first run, Mantis creates `.mantis/` in your project:
```
.mantis/
├── BRAIN.md          ← rolling project summary, updated each session
├── CONVENTIONS.md    ← your architecture rules (auto-enforced)
├── DECISIONS.log     ← timestamped decisions
├── REJECTED.md       ← approaches tried and failed (AI won't repeat them)
├── GROUND_TRUTH.json ← live function signatures — prevents hallucination
└── embeddings.db     ← semantic memory (Ollama + SQLite vector search)
```

All plain text (except embeddings.db). Human-editable. Committable.

### Codebase intelligence
Run `mantis init` once to build a live AST dependency graph. The AI then:
- Bundles only the relevant files for each question (multi-signal scoring)
- Runs impact analysis before proposing edits
- Checks AI output against your real function signatures
- Enforces your architecture rules on every response
- Uses multi-pass reasoning for complex questions (analysis → solution)
- Retrieves semantically relevant context from past sessions

```bash
mantis init    # index the project (run once)
```

---

## CLI Commands

### Intelligence
```bash
mantis hotspots            # files with highest churn (change frequency)
mantis risky               # high-risk files: churn × many authors
mantis coupling [path]     # files that always change together
mantis intent <path>       # commit intent timeline (feat/fix/refactor)
mantis todos               # scan for TODO/FIXME/HACK across codebase
mantis spec-gaps           # detect mismatches between commit intent and code
```

### Graph analysis
```bash
mantis init                # build dependency graph
mantis find <symbol>       # locate a function/class/type
mantis impact <symbol>     # trace what depends on a symbol
mantis dead                # find unreferenced code
mantis circular            # detect circular dependencies
mantis graph               # visualize dependency graph
mantis lint                # check architecture rules
```

### Cross-repo workspace
```bash
mantis workspace init ~/api ~/frontend ~/shared-lib
mantis workspace find <symbol>    # search across all repos
mantis workspace impact <symbol>  # cross-repo impact analysis
mantis workspace stats            # per-repo statistics
```

### Session management
```bash
mantis handoff             # generate HANDOFF.md for async collaboration
```

---

## Token savings report

At the end of every session:
```
╭──────────────────────────────────────────────╮
│  SESSION SUMMARY — mantis                    │
├──────────────────────────────────────────────┤
│  Total tokens      14,832                    │
│  Route  fast×3  code×7  heavy×1              │
├──────────────────────────────────────────────┤
│  WHAT THIS WOULD HAVE COST                   │
│  GPT-4o             $0.22                    │
│  Claude Sonnet      $0.18                    │
│  Claude Opus        $2.40                    │
│  Mantis cost        $0.00 ✓                  │
╰──────────────────────────────────────────────╯
```

---

## Project structure

```
cmd/mantis/          entry point + all CLI commands
internal/
  ollama/            Ollama Cloud + local client (streaming + embeddings)
  router/            7-tier intent classifier + model selector + task templates
  repl/              interactive AI session (multi-pass reasoning, compression)
  brain/             persistent project memory (.mantis/)
  truth/             live function signature index (GROUND_TRUTH.json)
  verify/            hallucination checker + convention enforcement
  session/           token tracking + cost report
  usage/             free-tier usage tracking
  nl/                NLP dispatcher → codebase intelligence tools
  graph/             AST dependency graph (SQLite) + cross-repo workspace
  intel/             temporal analysis, intent gaps, impact, dead code
  parser/            tree-sitter parsers (Go, TypeScript, Python)
  linter/            architecture rule enforcement
  tui/               Bubbletea dashboard
  context/           surgical context bundler (multi-signal scoring)
  viz/               D3 graph visualizer
  embeddings/        semantic memory (Ollama embed + SQLite cosine search)
```

---

## GitHub Education

If you have GitHub Education, you already have Claude Pro and Copilot in the terminal.
Mantis is the missing piece — it gives you the **API layer** those tools don't expose,
for free, with better codebase understanding than either.

---

## License

MIT

