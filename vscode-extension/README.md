# Mantis — Graph-Powered Code Intelligence

Mantis brings dependency graph analysis, hotspot detection, dead code identification, and impact analysis directly into VS Code.

## Features

### Hotspot Detection
Identifies files with high churn and low bus factor — the riskiest parts of your codebase. Ranked in the sidebar for quick access.

### Dead Code Detection
Finds exported symbols with no internal consumers. Highlights unused functions, types, and constants so you can clean up safely.

### Impact Analysis
Select any symbol and see its full blast radius — every file and function that depends on it, scored by coupling strength.

### Coupling Analysis
Surfaces files that frequently change together in git history. Reveals hidden dependencies that the import graph doesn't show.

### LSP Integration
Runs as a language server, providing diagnostics inline as you edit. Supports Go, TypeScript, and Python.

## Requirements

- [Mantis CLI](https://github.com/SeedHire/Mantis) installed and on your PATH
- Run `mantis init` in your project root to build the dependency graph

## Getting Started

1. Install the Mantis CLI: `brew install seedhire/tap/mantis`
2. Open your project in VS Code
3. Run `mantis init` from the terminal (or use the command palette: **Mantis: Initialize Project**)
4. The Mantis sidebar panel appears with hotspots, dead code, and impact views

## Commands

| Command | Description |
|---------|-------------|
| `Mantis: Initialize Project` | Run `mantis init` to build the graph |
| `Mantis: Analyze Impact` | Show blast radius for current symbol |
| `Mantis: Find Symbol` | Jump to a symbol definition |
| `Mantis: Show Coupling` | View temporal coupling for current file |
| `Mantis: Refresh Diagnostics` | Force-refresh all diagnostics |

## Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `mantis.binaryPath` | `mantis` | Path to the mantis binary |
| `mantis.autoRefresh` | `true` | Auto-refresh diagnostics on file save |

## Supported Languages

- Go
- TypeScript / TSX
- Python
