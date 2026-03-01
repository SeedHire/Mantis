package linter

import (
	"fmt"
	"strings"

	"github.com/seedhire/mantis/internal/config"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// Runner executes lint rules against the dependency graph.
type Runner struct {
	querier *graph.Querier
	root    string
}

// NewRunner creates a new Runner.
func NewRunner(querier *graph.Querier, root string) *Runner {
	return &Runner{querier: querier, root: root}
}

// Run evaluates all rules in the config and returns any violations found.
func (r *Runner) Run(cfg *config.Config) ([]Violation, error) {
	var violations []Violation

	for _, rule := range cfg.Rules {
		sev := rule.Severity
		if sev == "" {
			sev = "error"
		}

		switch {
		case rule.Type == "built_in":
			vs, err := r.runBuiltIn(rule, sev)
			if err != nil {
				return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
			}
			violations = append(violations, vs...)

		default:
			// Custom import restriction rule
			if rule.From == "" {
				continue
			}
			vs, err := r.runCustom(rule, sev)
			if err != nil {
				return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
			}
			violations = append(violations, vs...)
		}
	}

	return violations, nil
}

func (r *Runner) runBuiltIn(rule config.LintRule, sev string) ([]Violation, error) {
	switch rule.Name {
	case "no-circular-dependencies":
		return r.ruleNoCircular(rule, sev)
	case "max-cyclomatic-complexity":
		return r.ruleMaxComplexity(rule, sev)
	case "max-file-dependencies":
		return r.ruleMaxFileDeps(rule, sev)
	case "no-deep-imports":
		return r.ruleNoDeepImports(rule, sev)
	default:
		return nil, nil
	}
}

func (r *Runner) runCustom(rule config.LintRule, sev string) ([]Violation, error) {
	patterns := DisallowPatterns(rule)
	if len(patterns) == 0 {
		return nil, nil
	}

	files, err := r.querier.GetAllFiles()
	if err != nil {
		return nil, err
	}

	var violations []Violation
	for _, file := range files {
		relFrom := r.rel(file.FilePath)
		if !matchesGlob(rule.From, relFrom) {
			continue
		}

		deps, err := r.querier.GetImportDeps(file.ID)
		if err != nil {
			return nil, err
		}

		for _, dep := range deps {
			relDep := r.rel(dep.FilePath)
			for _, pat := range patterns {
				if matchesGlob(pat, relDep) {
					violations = append(violations, Violation{
						Rule:     rule.Name,
						Severity: sev,
						File:     relFrom,
						Line:     0,
						Message: fmt.Sprintf("'%s' must not import from '%s' (rule: %s)",
							relFrom, relDep, rule.Name),
					})
					break
				}
			}
		}
	}
	return violations, nil
}

func (r *Runner) ruleNoCircular(rule config.LintRule, sev string) ([]Violation, error) {
	result, err := intel.FindCircular(r.querier)
	if err != nil {
		return nil, err
	}

	var violations []Violation
	for _, cycle := range result.Cycles {
		chain := strings.Join(cycle.Nodes, " → ")
		file := ""
		if len(cycle.Nodes) > 0 {
			file = r.rel(cycle.Nodes[0])
		}
		for _, fp := range cycle.Nodes {
			violations = append(violations, Violation{
				Rule:     rule.Name,
				Severity: sev,
				File:     r.rel(fp),
				Line:     0,
				Message:  fmt.Sprintf("circular import chain: %s", chain),
			})
		}
		_ = file
	}
	return violations, nil
}

func (r *Runner) ruleMaxComplexity(rule config.LintRule, sev string) ([]Violation, error) {
	threshold := rule.Threshold
	if threshold == 0 {
		threshold = 10
	}

	nodes, err := r.querier.FindAllNodes(graph.NodeTypeFunction)
	if err != nil {
		return nil, err
	}
	// Also check methods
	methods, err := r.querier.FindAllNodes(graph.NodeTypeMethod)
	if err != nil {
		return nil, err
	}
	nodes = append(nodes, methods...)

	var violations []Violation
	for _, node := range nodes {
		if node.Complexity > threshold {
			violations = append(violations, Violation{
				Rule:     rule.Name,
				Severity: sev,
				File:     r.rel(node.FilePath),
				Line:     node.LineStart,
				Message:  fmt.Sprintf("function '%s' has complexity %d (max: %d)", node.Name, node.Complexity, threshold),
			})
		}
	}
	return violations, nil
}

func (r *Runner) ruleMaxFileDeps(rule config.LintRule, sev string) ([]Violation, error) {
	threshold := rule.Threshold
	if threshold == 0 {
		threshold = 15
	}

	files, err := r.querier.GetAllFiles()
	if err != nil {
		return nil, err
	}

	var violations []Violation
	for _, file := range files {
		deps, err := r.querier.GetImportDeps(file.ID)
		if err != nil {
			return nil, err
		}
		if len(deps) > threshold {
			violations = append(violations, Violation{
				Rule:     rule.Name,
				Severity: sev,
				File:     r.rel(file.FilePath),
				Line:     0,
				Message:  fmt.Sprintf("file has %d dependencies (max: %d)", len(deps), threshold),
			})
		}
	}
	return violations, nil
}

func (r *Runner) ruleNoDeepImports(rule config.LintRule, sev string) ([]Violation, error) {
	files, err := r.querier.GetAllFiles()
	if err != nil {
		return nil, err
	}

	var violations []Violation
	for _, file := range files {
		deps, err := r.querier.GetImportDeps(file.ID)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if isDeepInternal(dep.FilePath) && !isDeepInternal(file.FilePath) {
				violations = append(violations, Violation{
					Rule:     rule.Name,
					Severity: sev,
					File:     r.rel(file.FilePath),
					Line:     0,
					Message:  fmt.Sprintf("deep import of internal path '%s'", r.rel(dep.FilePath)),
				})
			}
		}
	}
	return violations, nil
}

func isDeepInternal(path string) bool {
	return strings.Contains(path, "/internal/") || strings.Contains(path, "/private/")
}

func (r *Runner) rel(path string) string {
	trimmed := strings.TrimPrefix(path, r.root+"/")
	if trimmed == path {
		trimmed = strings.TrimPrefix(path, r.root+string('/'))
	}
	return trimmed
}
