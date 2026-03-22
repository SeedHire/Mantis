# Changelog

## 0.1.0 — 2026-03-22

### Added
- Activity bar panel with Hotspots, Dead Code, and Impact Analysis tree views
- LSP integration via `mantis lsp` subprocess (stdio transport)
- Five commands: impact analysis, find symbol, coupling, refresh diagnostics, init
- Auto-detection of `.mantis/graph.db` with prompt to run `mantis init`
- File system watcher for lazy LSP startup on graph creation
- Status bar indicator showing LSP connection state
- Configuration: binary path, auto-refresh toggle
- Support for Go, TypeScript/TSX, and Python
