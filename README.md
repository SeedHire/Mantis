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

Mantis runs a **3-tier model router** — every message is classified and sent to the right model:

| Tier | Models | Used for |
|---|---|---|
| Fast | qwen2.5-coder:1.5b | simple questions, completions |
| Smart | qwen2.5-coder:7b | bug fixes, refactors (default) |
| Heavy | llama3.3:70b | system design, multi-file changes |
| Vision | llava:34b | screenshots, diagrams |

### Project memory
On first run, Mantis creates `.mantis/` in your project:
```
.mantis/
├── BRAIN.md          ← rolling project summary, updated each session
├── CONVENTIONS.md    ← your architecture rules
├── DECISIONS.log     ← timestamped decisions
├── REJECTED.md       ← approaches tried and failed (AI won't repeat them)
└── GROUND_TRUTH.json ← live function signatures — prevents hallucination
```

All plain text. Human-editable. Committable.

### Codebase intelligence (optional, unlocks automatically)
If you run `mantis init` in a project, Mantis builds a live AST dependency graph.
The AI then automatically:
- Bundles only the relevant files for each question (not your whole repo)
- Runs impact analysis before proposing any edit
- Checks AI output against your real function signatures
- Enforces your architecture rules on every response

```bash
mantis init    # index the project (run once)
```

---

## Token savings report

At the end of every session:
```
╭──────────────────────────────────────────────╮
│  SESSION SUMMARY — mantis                    │
├──────────────────────────────────────────────┤
│  Total tokens      14,832                    │
│  Route  fast×3  smart×7  heavy×1             │
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
cmd/mantis/          entry point
internal/
  ollama/            Ollama Cloud + local client (streaming)
  router/            3-tier intent classifier + model selector
  repl/              interactive AI session (the main product)
  brain/             persistent project memory (.mantis/)
  truth/             live function signature index (GROUND_TRUTH.json)
  verify/            hallucination checker
  session/           token tracking + cost report
  usage/             free-tier usage tracking
  nl/                NLP dispatcher → codebase intelligence tools
  graph/             AST dependency graph (SQLite)
  intel/             impact, find, dead code, circular deps
  parser/            tree-sitter parsers (Go, TypeScript, Python)
  linter/            architecture rule enforcement
  tui/               Bubbletea dashboard
  context/           surgical context bundler
  viz/               D3 graph visualizer
```

---

## GitHub Education

If you have GitHub Education, you already have Claude Pro and Copilot in the terminal.
Mantis is the missing piece — it gives you the **API layer** those tools don't expose,
for free, with better codebase understanding than either.

---

## License

MIT

