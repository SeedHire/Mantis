package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LintRule defines a single lint rule from the config file.
type LintRule struct {
	Name            string      `yaml:"name"`
	Description     string      `yaml:"description"`
	From            string      `yaml:"from"`
	DisallowImport  interface{} `yaml:"disallow_import"`
	Severity        string      `yaml:"severity"`
	Type            string      `yaml:"type"`
	Threshold       int         `yaml:"threshold"`
}

// Config holds all lint rules.
type Config struct {
	Version int        `yaml:"version"`
	Rules   []LintRule `yaml:"rules"`
}

// ProjectConfig holds resolved project-level settings.
type ProjectConfig struct {
	Root     string
	Language string
	DBPath   string
	Config   *Config
}

// Load reads .mantisrc.yml from root. Returns empty config if not found.
func Load(root string) (*Config, error) {
	path := filepath.Join(root, ".mantisrc.yml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DefaultDBPath returns the path to the SQLite graph database.
func DefaultDBPath(root string) string {
	return filepath.Join(root, ".mantis", "graph.db")
}
