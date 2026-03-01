package router

// TaskTemplate returns a task-specific instruction wrapper for the user's message.
// This guides the model toward a structured response format appropriate for the
// detected task type, significantly improving answer quality from smaller models.
func TaskTemplate(taskType string) string {
	switch taskType {
	case "explain":
		return `[Task: Explain]
Start with a one-line summary. Then walk through the logic step by step.
Reference actual function names and file paths from the project.`

	case "fix":
		return `[Task: Fix Bug]
1. Show the broken code and explain WHY it fails.
2. Show the fix.
3. Explain why the fix works.
4. Note any files that might need related changes.`

	case "refactor":
		return `[Task: Refactor]
1. Show the current code.
2. Show the refactored version.
3. List every file that needs to change.
4. Flag any breaking changes or behavioral differences.`

	case "implement":
		return `[Task: Implement]
1. Start with the interface or function signature.
2. Then the implementation.
3. Then a usage example.
4. Note any imports or dependencies needed.`

	case "test":
		return `[Task: Write Tests]
Cover these cases:
- Happy path (expected input → expected output)
- Edge cases (empty, nil, boundary values)
- Error cases (invalid input, failures)
Use the project's existing test patterns and conventions.`

	case "impact-query":
		return `[Task: Impact Analysis]
1. List direct dependencies (what this code calls).
2. List reverse dependencies (what calls this code).
3. Assess blast radius: what breaks if this changes?
4. Recommend a safe change strategy.`

	default:
		return ""
	}
}
