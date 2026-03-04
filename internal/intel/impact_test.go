package intel

import "testing"

func TestIsIgnoredPath(t *testing.T) {
	ignored := []string{
		"vendor/github.com/foo/bar.go",
		"/project/vendor/lib.go",
		"src/node_modules/lodash/index.js",
		"/app/node_modules/react/index.js",
		"api/types.gen.go",
		"api/types_gen.go",
		"proto/foo.generated.go",
		"proto/foo_generated.go",
		"internal/mock/handler.go",
		"internal/mocks/auth.go",
		"mock_database.go",
		"src/__generated__/schema.ts",
	}
	notIgnored := []string{
		"internal/router/router.go",
		"cmd/mantis/main.go",
		"internal/agent/toolkit.go",
		"internal/embeddings/embeddings.go",
		"pkg/util/helper.go",
	}

	for _, path := range ignored {
		if !isIgnoredPath(path) {
			t.Errorf("isIgnoredPath(%q) = false, want true", path)
		}
	}
	for _, path := range notIgnored {
		if isIgnoredPath(path) {
			t.Errorf("isIgnoredPath(%q) = true, want false", path)
		}
	}
}
