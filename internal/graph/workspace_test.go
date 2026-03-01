package graph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitWorkspaceConfig(t *testing.T) {
	dir := t.TempDir()
	repos := []RepoEntry{
		{Path: "/tmp/api", Alias: "api"},
		{Path: "/tmp/frontend", Alias: "frontend"},
	}

	err := InitWorkspaceConfig(dir, repos)
	if err != nil {
		t.Fatalf("InitWorkspaceConfig: %v", err)
	}

	configPath := filepath.Join(dir, "mantis.workspace.yml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	content := string(data)
	if !contains(content, "/tmp/api") {
		t.Error("config should contain /tmp/api")
	}
	if !contains(content, "frontend") {
		t.Error("config should contain frontend alias")
	}
}

func TestLoadWorkspaceNoFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadWorkspace(dir)
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadWorkspaceValid(t *testing.T) {
	dir := t.TempDir()
	repos := []RepoEntry{
		{Path: "/tmp/repo1", Alias: "r1"},
	}
	if err := InitWorkspaceConfig(dir, repos); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWorkspace(dir)
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	if len(cfg.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(cfg.Repos))
	}
	if cfg.Repos[0].Alias != "r1" {
		t.Errorf("alias = %q, want r1", cfg.Repos[0].Alias)
	}
}

func TestContainsRepoRef(t *testing.T) {
	tests := []struct {
		toID, metadata, repoPath, repoAlias string
		want                                 bool
	}{
		{"file:/tmp/api/main.go", "", "/tmp/api", "api", true},
		{"file:src/main.go", "import api/handler", "", "api", true},
		{"file:local/handler.go", "", "/other", "other", false},
		{"func:Handler:src/api.go", "", "", "api", true},
	}
	for _, tt := range tests {
		got := containsRepoRef(tt.toID, tt.metadata, tt.repoPath, tt.repoAlias)
		if got != tt.want {
			t.Errorf("containsRepoRef(%q, %q, %q, %q) = %v, want %v",
				tt.toID, tt.metadata, tt.repoPath, tt.repoAlias, got, tt.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
