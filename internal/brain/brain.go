// Package brain manages the persistent project memory stored in .mantis/
// BRAIN.md       — rolling project summary, auto-updated after each session
// CONVENTIONS.md — architecture rules + style decisions, hand-editable
// DECISIONS.log  — timestamped append-only "chose X because Y" log
// REJECTED.md    — failed approaches + reasons, prevents the AI repeating them
// GROUND_TRUTH.json — live function signatures + file hashes from tree-sitter
package brain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const dirName = ".mantis"

// Brain holds the path to the project brain directory.
type Brain struct {
	root    string
	dir     string
}

// New returns a Brain rooted at the given project directory.
func New(projectRoot string) *Brain {
	return &Brain{
		root: projectRoot,
		dir:  filepath.Join(projectRoot, dirName),
	}
}

// Init creates the .mantis/ brain directory and seed files if they don't exist.
func (b *Brain) Init() error {
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return err
	}
	if err := b.seedBrainMD(); err != nil {
		return err
	}
	if err := b.seedConventions(); err != nil {
		return err
	}
	return nil
}

func (b *Brain) seedBrainMD() error {
	path := filepath.Join(b.dir, "BRAIN.md")
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	content := fmt.Sprintf(`# BRAIN.md — Mantis Project Memory
# Auto-maintained. Edit freely. Do not delete.
# Last updated: %s

## Project
(Mantis will fill this in after your first session)

## Stack
(unknown — tell Mantis your stack or it will detect it)

## Current Phase
(not set)

## Active Context
(nothing yet)

## Recent Decisions
(none yet)

## Rejected Approaches
(none yet — they will be logged here automatically)

## Conventions
(add your project conventions here, or they will be inferred)
`, time.Now().Format("2006-01-02"))
	return os.WriteFile(path, []byte(content), 0o644)
}

func (b *Brain) seedConventions() error {
	path := filepath.Join(b.dir, "CONVENTIONS.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	content := `# CONVENTIONS.md — Architecture Rules
# Mantis enforces these on every AI response.
# Add your own. Format: plain English or bullet points.

## Naming
(not set)

## Architecture
(not set)

## Testing
(not set)
`
	return os.WriteFile(path, []byte(content), 0o644)
}

// Load reads all brain files and returns a SystemPrompt fragment
// to inject into the AI's context at the start of every session.
func (b *Brain) Load() string {
	var parts []string

	if brain := b.readFile("BRAIN.md"); brain != "" {
		parts = append(parts, "## Project Memory (BRAIN.md)\n"+brain)
	}
	if conv := b.readFile("CONVENTIONS.md"); conv != "" {
		parts = append(parts, "## Project Conventions\n"+conv)
	}
	if rejected := b.readFile("REJECTED.md"); rejected != "" {
		parts = append(parts, "## Previously Rejected Approaches (do NOT suggest these again)\n"+rejected)
	}
	if gt := b.loadGroundTruthN(50, 8000); gt != "" {
		parts = append(parts, "## Live Code State (GROUND_TRUTH)\n"+gt)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// LogDecision appends a timestamped decision to DECISIONS.log.
func (b *Brain) LogDecision(decision string) error {
	path := filepath.Join(b.dir, "DECISIONS.log")
	entry := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04"), decision)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry)
	return err
}

// LogRejected appends a rejected approach to REJECTED.md.
func (b *Brain) LogRejected(approach, reason string) error {
	path := filepath.Join(b.dir, "REJECTED.md")
	entry := fmt.Sprintf("- **%s** — %s (%s)\n",
		approach, reason, time.Now().Format("2006-01-02"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry)
	return err
}

// UpdateBrain rewrites BRAIN.md with the provided summary content.
// Called at the end of each session with an AI-generated summary.
func (b *Brain) UpdateBrain(summary string) error {
	path := filepath.Join(b.dir, "BRAIN.md")
	content := fmt.Sprintf("# BRAIN.md — Mantis Project Memory\n# Last updated: %s\n\n%s\n",
		time.Now().Format("2006-01-02 15:04"), summary)
	return os.WriteFile(path, []byte(content), 0o644)
}

// ReadBrain returns the raw contents of BRAIN.md.
func (b *Brain) ReadBrain() string {
	return b.readFile("BRAIN.md")
}

// GroundTruthEntry is a file entry in GROUND_TRUTH.json.
type GroundTruthEntry struct {
	Hash            string     `json:"hash"`
	LastModified    string     `json:"last_modified"`
	Functions       []FuncSig  `json:"functions"`
	Imports         []string   `json:"imports"`
	ExportedSymbols []string   `json:"exported_symbols"`
}

// FuncSig is a function signature extracted by tree-sitter.
type FuncSig struct {
	Name    string `json:"name"`
	Params  string `json:"params"`
	Returns string `json:"returns"`
}

// loadGroundTruthN returns a compact text summary of GROUND_TRUTH.json
// capped at maxFiles files and maxChars characters.
func (b *Brain) loadGroundTruthN(maxFiles, maxChars int) string {
	path := filepath.Join(b.dir, "GROUND_TRUTH.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var gt map[string]GroundTruthEntry
	if err := json.Unmarshal(data, &gt); err != nil {
		return ""
	}
	if len(gt) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Verified live symbols in this codebase:\n")
	count := 0
	for file, entry := range gt {
		if count >= maxFiles || sb.Len() >= maxChars {
			sb.WriteString("... (truncated, full data in GROUND_TRUTH.json)\n")
			break
		}
		if len(entry.Functions) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("  %s:\n", file))
		for _, fn := range entry.Functions {
			if sb.Len() >= maxChars {
				break
			}
			sb.WriteString(fmt.Sprintf("    func %s(%s) %s\n", fn.Name, fn.Params, fn.Returns))
		}
		count++
	}
	return sb.String()
}

// LoadForTier returns brain context sized for the given model tier.
func (b *Brain) LoadForTier(tier string) string {
	var parts []string

	if brain := b.readFile("BRAIN.md"); brain != "" {
		parts = append(parts, "## Project Memory (BRAIN.md)\n"+brain)
	}
	if conv := b.readFile("CONVENTIONS.md"); conv != "" {
		parts = append(parts, "## Project Conventions\n"+conv)
	}
	if rejected := b.readFile("REJECTED.md"); rejected != "" {
		parts = append(parts, "## Previously Rejected Approaches (do NOT suggest these again)\n"+rejected)
	}

	var maxFiles, maxChars int
	switch tier {
	case "trivial", "fast":
		maxFiles, maxChars = 15, 2000
	case "code":
		maxFiles, maxChars = 30, 4000
	case "reason":
		maxFiles, maxChars = 50, 8000
	case "heavy", "max":
		maxFiles, maxChars = 80, 16000
	default:
		maxFiles, maxChars = 15, 2000
	}

	if gt := b.loadGroundTruthN(maxFiles, maxChars); gt != "" {
		parts = append(parts, "## Live Code State (GROUND_TRUTH)\n"+gt)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// Exists reports whether a brain directory exists for this project.
func (b *Brain) Exists() bool {
	_, err := os.Stat(b.dir)
	return err == nil
}

func (b *Brain) readFile(name string) string {
	data, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}
