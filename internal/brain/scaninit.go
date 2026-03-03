package brain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Querier is the minimal interface brain needs to call an LLM.
// Implemented by *ollama.Client — defined here to avoid an import cycle.
type Querier interface {
	// QuickChat sends a one-shot non-streaming message and returns the response.
	QuickChat(ctx context.Context, model, systemPrompt, userMsg string) (string, error)
}

// ScanInit analyzes the project at root, generates a MANTIS.md file using the
// provided model, writes it to <root>/MANTIS.md, and seeds .mantis/BRAIN.md.
// Returns the generated content so the caller can display it.
func (b *Brain) ScanInit(ctx context.Context, querier Querier, model string) (string, error) {
	snapshot := collectProjectSnapshot(b.root)
	if snapshot == "" {
		return "", fmt.Errorf("no recognisable project files found in %s", b.root)
	}

	prompt := buildInitPrompt(snapshot)
	content, err := querier.QuickChat(ctx, model, initSystemPrompt, prompt)
	if err != nil {
		return "", fmt.Errorf("model call failed: %w", err)
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("model returned empty response")
	}

	// Ensure the file starts with the standard header.
	if !strings.HasPrefix(content, "# MANTIS.md") {
		content = "# MANTIS.md\n\nThis file provides guidance to Mantis when working with code in this repository.\n\n" + content
	}

	// Write MANTIS.md to project root (visible, committable).
	mantisPath := filepath.Join(b.root, "MANTIS.md")
	if err := os.WriteFile(mantisPath, []byte(content+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write MANTIS.md: %w", err)
	}

	// Also seed .mantis/BRAIN.md so the retrieval system benefits immediately.
	_ = os.MkdirAll(b.dir, 0o755)
	brainPath := filepath.Join(b.dir, "BRAIN.md")
	brainContent := fmt.Sprintf("# BRAIN.md — Mantis Project Memory\n# Auto-seeded from /init on %s\n# Edit freely.\n\n%s\n",
		time.Now().Format("2006-01-02"), content)
	_ = os.WriteFile(brainPath, []byte(brainContent), 0o644)

	return content, nil
}

// HasMantisFile returns true if MANTIS.md exists in the project root.
func (b *Brain) HasMantisFile() bool {
	_, err := os.Stat(filepath.Join(b.root, "MANTIS.md"))
	return err == nil
}

// ReadMantisFile returns the contents of the project-root MANTIS.md.
func (b *Brain) ReadMantisFile() string {
	data, err := os.ReadFile(filepath.Join(b.root, "MANTIS.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// ── Project snapshot collector ────────────────────────────────────────────────

// analysisCandidates are files read in order to build the project snapshot.
// Content is capped per-file to keep the total prompt size manageable.
var analysisCandidates = []struct {
	name    string
	maxChar int
}{
	{"README.md", 3000},
	{"readme.md", 3000},
	{"package.json", 2000},
	{"go.mod", 1000},
	{"Cargo.toml", 1000},
	{"requirements.txt", 800},
	{"pyproject.toml", 1000},
	{"Makefile", 1500},
	{"makefile", 1500},
	{".env.example", 600},
	{".env.sample", 600},
	{"tsconfig.json", 600},
	{"vite.config.ts", 600},
	{"vite.config.js", 600},
	{"docker-compose.yml", 600},
	{"docker-compose.yaml", 600},
}

func collectProjectSnapshot(root string) string {
	var sb strings.Builder

	// Top-level directory tree (2 levels).
	sb.WriteString("## Directory structure\n```\n")
	writeTree(&sb, root, "", 0, 2)
	sb.WriteString("```\n\n")

	// Key files.
	for _, c := range analysisCandidates {
		data, err := os.ReadFile(filepath.Join(root, c.name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		if len(content) > c.maxChar {
			content = content[:c.maxChar] + "\n… (truncated)"
		}
		ext := filepath.Ext(c.name)
		lang := extToLang(ext)
		sb.WriteString(fmt.Sprintf("## %s\n```%s\n%s\n```\n\n", c.name, lang, content))
	}

	return sb.String()
}

// writeTree writes a compact directory listing up to maxDepth levels deep.
func writeTree(sb *strings.Builder, dir, prefix string, depth, maxDepth int) {
	if depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	skip := map[string]bool{
		"node_modules": true, ".git": true, "vendor": true, "dist": true,
		"build": true, "target": true, ".mantis": true, "__pycache__": true,
		".venv": true, "venv": true, "bin": true, "obj": true,
	}
	for _, e := range entries {
		if skip[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sb.WriteString(prefix + e.Name())
		if e.IsDir() {
			sb.WriteString("/\n")
			writeTree(sb, filepath.Join(dir, e.Name()), prefix+"  ", depth+1, maxDepth)
		} else {
			sb.WriteString("\n")
		}
	}
}

func extToLang(ext string) string {
	switch ext {
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	case ".yaml", ".yml":
		return "yaml"
	case ".ts", ".tsx":
		return "typescript"
	case ".js":
		return "javascript"
	case ".md":
		return "markdown"
	default:
		return ""
	}
}

// ── Prompt ────────────────────────────────────────────────────────────────────

const initSystemPrompt = `You are a senior software engineer creating a MANTIS.md file for a project.
MANTIS.md is read by an AI coding assistant at the start of every session to understand the project.

Rules:
- Be concise and specific. Every line should save the AI from having to re-read code.
- Focus on information that requires reading MULTIPLE files to understand.
- Do NOT list every file — only the ones that are architecturally significant.
- Do NOT include obvious advice like "write tests" or "handle errors".
- Do NOT pad with generic boilerplate.

Required sections (use exactly these headers):
## Commands
## Architecture
## Key Conventions

The Commands section is the most important — list exact shell commands to build, run, test, and lint.`

func buildInitPrompt(snapshot string) string {
	return fmt.Sprintf(`Analyze this project and write a MANTIS.md file.

Begin with exactly:
# MANTIS.md

This file provides guidance to Mantis when working with code in this repository.

Then write the three required sections.

---

## Project files

%s`, snapshot)
}
