// Package brain manages the persistent project memory stored in .mantis/
// BRAIN.md       — rolling project summary, auto-updated after each session
// CONVENTIONS.md — architecture rules + style decisions, hand-editable
// DECISIONS.log  — timestamped append-only "chose X because Y" log
// REJECTED.md    — failed approaches + reasons, prevents the AI repeating them
// GROUND_TRUTH.json — live function signatures + file hashes from tree-sitter
package brain

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const skillsDirName = "skills"

//go:embed skills/*.md
var embeddedSkills embed.FS

const dirName = ".mantis"

// taskSkillPriority maps task types to the skill files that should be loaded
// first, ensuring the most relevant expertise appears within the token budget.
var taskSkillPriority = map[string][]string{
	"implement": {
		"senior-software-developer", "api-designer", "database-architect",
		"security-analyst", "devops-engineer", "code-reviewer",
	},
	"fix": {
		"debugging-detective", "senior-software-developer", "code-reviewer",
		"security-analyst",
	},
	"refactor": {
		"senior-software-developer", "code-reviewer", "debugging-detective",
	},
	"test": {
		"senior-software-developer", "code-reviewer", "debugging-detective",
	},
	"explain": {
		"concept-explainer", "technical-writer", "senior-software-developer",
		"documentation-writer",
	},
	"impact-query": {
		"senior-software-developer", "code-reviewer", "project-planner",
	},
	"general": {
		"senior-software-developer", "concept-explainer", "technical-writer",
	},
}

// Brain holds the path to the project brain directory.
type Brain struct {
	root string
	dir  string
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
	if err := b.seedSkillsDir(); err != nil {
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

// DiscoverConventions analyzes the project at b.root and fills CONVENTIONS.md
// with auto-detected conventions. Only runs if CONVENTIONS.md still has placeholder
// "(not set)" sections. Returns the number of conventions discovered.
func (b *Brain) DiscoverConventions() int {
	path := filepath.Join(b.dir, "CONVENTIONS.md")
	existing, err := os.ReadFile(path)
	if err == nil && !strings.Contains(string(existing), "(not set)") {
		return 0 // user already filled it in
	}

	var naming, arch, testing []string

	// Detect language and naming from project files.
	if _, err := os.Stat(filepath.Join(b.root, "go.mod")); err == nil {
		naming = append(naming, "Go: exported names are PascalCase, unexported are camelCase")
		naming = append(naming, "Go: error variables use `err` prefix, error types use `Error` suffix")
		arch = append(arch, "Go modules with `internal/` for private packages")
		testing = append(testing, "Go: tests in `_test.go` files in the same package")
		testing = append(testing, "Go: use table-driven tests where applicable")
		// Check for cmd/ directory pattern
		if info, err := os.Stat(filepath.Join(b.root, "cmd")); err == nil && info.IsDir() {
			arch = append(arch, "`cmd/` for CLI entry points, `internal/` for library code")
		}
	}
	if _, err := os.Stat(filepath.Join(b.root, "package.json")); err == nil {
		naming = append(naming, "JavaScript/TypeScript: camelCase for variables/functions, PascalCase for classes/components")
		testing = append(testing, "Node: tests in `__tests__/` or `*.test.{ts,js}` files")
		// Check for src/ pattern
		if info, err := os.Stat(filepath.Join(b.root, "src")); err == nil && info.IsDir() {
			arch = append(arch, "`src/` for source code")
		}
	}
	if _, err := os.Stat(filepath.Join(b.root, "pyproject.toml")); err == nil {
		naming = append(naming, "Python: snake_case for functions/variables, PascalCase for classes")
		testing = append(testing, "Python: tests in `tests/` directory using pytest")
	}
	if _, err := os.Stat(filepath.Join(b.root, "requirements.txt")); err == nil && len(naming) == 0 {
		naming = append(naming, "Python: snake_case for functions/variables, PascalCase for classes")
		testing = append(testing, "Python: tests in `tests/` directory using pytest")
	}

	// Check for common config files that imply conventions
	if _, err := os.Stat(filepath.Join(b.root, ".eslintrc.json")); err == nil {
		naming = append(naming, "ESLint enforced — follow existing lint rules")
	}
	if _, err := os.Stat(filepath.Join(b.root, ".prettierrc")); err == nil {
		naming = append(naming, "Prettier formatting — auto-formatted on save")
	}
	if _, err := os.Stat(filepath.Join(b.root, "Makefile")); err == nil {
		arch = append(arch, "Makefile-based build system — use `make` targets for build/test/lint")
	}
	if _, err := os.Stat(filepath.Join(b.root, "docker-compose.yml")); err == nil {
		arch = append(arch, "Docker Compose for local services")
	}

	total := len(naming) + len(arch) + len(testing)
	if total == 0 {
		return 0
	}

	// Build the conventions file.
	var sb strings.Builder
	sb.WriteString("# CONVENTIONS.md — Architecture Rules\n")
	sb.WriteString("# Auto-discovered by `mantis init`. Edit freely.\n")
	sb.WriteString("# Mantis enforces these on every AI response.\n\n")

	sb.WriteString("## Naming\n")
	if len(naming) > 0 {
		for _, n := range naming {
			sb.WriteString("- " + n + "\n")
		}
	} else {
		sb.WriteString("(not set)\n")
	}

	sb.WriteString("\n## Architecture\n")
	if len(arch) > 0 {
		for _, a := range arch {
			sb.WriteString("- " + a + "\n")
		}
	} else {
		sb.WriteString("(not set)\n")
	}

	sb.WriteString("\n## Testing\n")
	if len(testing) > 0 {
		for _, t := range testing {
			sb.WriteString("- " + t + "\n")
		}
	} else {
		sb.WriteString("(not set)\n")
	}

	_ = os.WriteFile(path, []byte(sb.String()), 0o644)
	return total
}

// Load reads all brain files and returns a SystemPrompt fragment
// to inject into the AI's context at the start of every session.
func (b *Brain) Load() string {
	var parts []string

	// MANTIS.md in the project root takes priority — it's the human-curated guide.
	if mantis := b.ReadMantisFile(); mantis != "" {
		parts = append(parts, "## Project Guide (MANTIS.md)\n"+mantis)
	}

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

// LoadHot returns only the "hot" memory tier: MANTIS.md, CONVENTIONS.md,
// and the last 3 decisions from DECISIONS.log. This keeps the always-injected
// system prompt small (~800 tokens). BRAIN.md and REJECTED.md are "cold" memory
// retrieved on-demand via embeddings search. (7.5: Tiered Cold Memory)
func (b *Brain) LoadHot() string {
	var parts []string

	if mantis := b.ReadMantisFile(); mantis != "" {
		parts = append(parts, "## Project Guide (MANTIS.md)\n"+mantis)
	}
	if conv := b.readFile("CONVENTIONS.md"); conv != "" {
		parts = append(parts, "## Project Conventions\n"+conv)
	}
	// Last 3 decisions only.
	if decisions := b.lastDecisions(3); decisions != "" {
		parts = append(parts, "## Recent Decisions\n"+decisions)
	}
	if gt := b.loadGroundTruthN(30, 4000); gt != "" {
		parts = append(parts, "## Live Code State (GROUND_TRUTH)\n"+gt)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// lastDecisions returns the last N entries from DECISIONS.log.
func (b *Brain) lastDecisions(n int) string {
	content := b.readFile("DECISIONS.log")
	if content == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) <= n {
		return content
	}
	return strings.Join(lines[len(lines)-n:], "\n")
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

// UpdateBrain appends a dated session entry to BRAIN.md (ACE-style incremental memory).
// Instead of rewriting the whole file each session (which causes brevity-bias drift),
// each session contributes a new dated section. Every 10 sections a consolidation header
// is inserted to keep the file scannable without losing any prior entries.
//
// Source: "Agentic Context Engineering" (arXiv 2510.04618)
func (b *Brain) UpdateBrain(summary string) error {
	path := filepath.Join(b.dir, "BRAIN.md")

	existing, _ := os.ReadFile(path)
	existingStr := string(existing)

	// Count existing dated session sections to decide when to add a consolidation marker.
	sectionCount := strings.Count(existingStr, "\n## Session ")
	needsConsolidation := sectionCount > 0 && sectionCount%10 == 0

	var sb strings.Builder
	if existingStr == "" {
		sb.WriteString("# BRAIN.md — Mantis Project Memory\n\n")
	} else {
		sb.WriteString(strings.TrimRight(existingStr, "\n"))
		sb.WriteByte('\n')
	}

	if needsConsolidation {
		sb.WriteString(fmt.Sprintf("\n---\n## Consolidated checkpoint (%d sessions)\n\n", sectionCount))
	}

	sb.WriteString(fmt.Sprintf("\n## Session %s\n\n%s\n",
		time.Now().Format("2006-01-02 15:04"), strings.TrimSpace(summary)))

	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// ReadBrain returns the raw contents of BRAIN.md.
func (b *Brain) ReadBrain() string {
	return b.readFile("BRAIN.md")
}

// IsBrainEmpty returns true if BRAIN.md is still the placeholder template.
func (b *Brain) IsBrainEmpty() bool {
	content := b.ReadBrain()
	return content == "" ||
		strings.Contains(content, "(Mantis will fill this in after your first session)") ||
		strings.Contains(content, "(unknown — tell Mantis your stack")
}

// AutoPopulateBrain fills in BRAIN.md with detected project info if it's still
// the placeholder template. Called on session start so the model always has context.
func (b *Brain) AutoPopulateBrain(lang, framework, entryPoint, runCmd string) {
	if !b.IsBrainEmpty() {
		return
	}
	if lang == "" {
		return // nothing detected — leave as-is
	}

	var sb strings.Builder
	sb.WriteString("# BRAIN.md — Mantis Project Memory\n")
	sb.WriteString(fmt.Sprintf("# Auto-populated on %s\n\n", time.Now().Format("2006-01-02")))
	sb.WriteString("## Project\n")
	sb.WriteString(fmt.Sprintf("Language: %s\n", lang))
	if framework != "" {
		sb.WriteString(fmt.Sprintf("Framework: %s\n", framework))
	}
	if entryPoint != "" {
		sb.WriteString(fmt.Sprintf("Entry point: %s\n", entryPoint))
	}
	if runCmd != "" {
		sb.WriteString(fmt.Sprintf("Run command: %s\n", runCmd))
	}

	sb.WriteString("\n## Stack\n")
	sb.WriteString(fmt.Sprintf("%s", lang))
	if framework != "" {
		sb.WriteString(fmt.Sprintf(" + %s", framework))
	}
	sb.WriteString("\n")

	sb.WriteString("\n## Current Phase\n(active development)\n")
	sb.WriteString("\n## Active Context\n(session start)\n")
	sb.WriteString("\n## Recent Decisions\n(none yet)\n")
	sb.WriteString("\n## Rejected Approaches\n(none yet)\n")

	path := filepath.Join(b.dir, "BRAIN.md")
	_ = os.WriteFile(path, []byte(sb.String()), 0o644)
}

// GroundTruthEntry is a file entry in GROUND_TRUTH.json.
type GroundTruthEntry struct {
	Hash            string    `json:"hash"`
	LastModified    string    `json:"last_modified"`
	Functions       []FuncSig `json:"functions"`
	Imports         []string  `json:"imports"`
	ExportedSymbols []string  `json:"exported_symbols"`
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

	if mantis := b.ReadMantisFile(); mantis != "" {
		parts = append(parts, "## Project Guide (MANTIS.md)\n"+mantis)
	}

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

// seedSkillsDir creates .mantis/skills/ and seeds all 25 built-in skills from the
// embedded FS. Existing files are never overwritten so user edits are preserved.
func (b *Brain) seedSkillsDir() error {
	skillsDir := filepath.Join(b.dir, skillsDirName)
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return err
	}

	entries, err := embeddedSkills.ReadDir("skills")
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		dest := filepath.Join(skillsDir, e.Name())
		if _, err := os.Stat(dest); err == nil {
			continue // already exists — never overwrite user modifications
		}
		data, err := embeddedSkills.ReadFile("skills/" + e.Name())
		if err != nil {
			continue
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SkillCount returns the number of skill files present in .mantis/skills/.
func (b *Brain) SkillCount() int {
	entries, err := os.ReadDir(filepath.Join(b.dir, skillsDirName))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

// LoadSkills reads all *.md files from .mantis/skills/, strips YAML frontmatter,
// and returns their combined content capped at maxChars characters.
// Returns empty string if no skills directory or no skill files exist.
func (b *Brain) LoadSkills(maxChars int) string {
	return b.loadSkillsOrdered(nil, maxChars)
}

// LoadSkillsForTask loads skills most relevant to the given task type first,
// then fills remaining budget with other skills. This ensures the model gets
// the right expertise without blowing the context window.
//
// taskType values match router.detectTaskType: "implement", "fix", "refactor",
// "explain", "test", "impact-query", "general".
func (b *Brain) LoadSkillsForTask(taskType string, maxChars int) string {
	priority := taskSkillPriority[taskType]
	return b.loadSkillsOrdered(priority, maxChars)
}

// loadSkillsOrdered loads skills with priority names first, then the rest.
func (b *Brain) loadSkillsOrdered(priority []string, maxChars int) string {
	skillsDir := filepath.Join(b.dir, skillsDirName)
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return ""
	}

	// Build a map of filename → content.
	available := make(map[string]string, len(entries))
	var allNames []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue
		}
		content := stripYAMLFrontmatter(string(data))
		if content == "" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		available[name] = content
		allNames = append(allNames, name)
	}

	// Build ordered list: priority names first, then the rest.
	seen := make(map[string]bool)
	var ordered []string
	for _, p := range priority {
		if _, ok := available[p]; ok {
			ordered = append(ordered, p)
			seen[p] = true
		}
	}
	for _, n := range allNames {
		if !seen[n] {
			ordered = append(ordered, n)
		}
	}

	// Collect until maxChars.
	var parts []string
	total := 0
	for _, name := range ordered {
		content := available[name]
		if total+len(content) > maxChars {
			remaining := maxChars - total
			if remaining > 300 {
				// Truncate at a valid UTF-8 rune boundary.
				runes := []rune(content)
				if remaining < len(content) {
					// Walk runes until byte length exceeds remaining.
					byteLen := 0
					runeIdx := 0
					for runeIdx < len(runes) {
						rl := len(string(runes[runeIdx]))
						if byteLen+rl > remaining {
							break
						}
						byteLen += rl
						runeIdx++
					}
					parts = append(parts, string(runes[:runeIdx])+"…")
				} else {
					parts = append(parts, content+"…")
				}
			}
			break
		}
		parts = append(parts, content)
		total += len(content)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// stripYAMLFrontmatter removes a leading --- ... --- YAML block from markdown.
func stripYAMLFrontmatter(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "---") {
		return s
	}
	// Find closing ---
	rest := s[3:]
	idx := strings.Index(rest, "\n---")
	if idx == -1 {
		return s
	}
	return strings.TrimSpace(rest[idx+4:])
}

// ReadFile returns the raw contents of a brain file by name.
func (b *Brain) ReadFile(name string) string {
	return b.readFile(name)
}

func (b *Brain) readFile(name string) string {
	data, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}
