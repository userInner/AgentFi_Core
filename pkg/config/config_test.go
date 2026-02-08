package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// No YAML file, no env vars → should return defaults
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("expected default log level 'info', got %q", cfg.Log.Level)
	}
	if cfg.LLM.Model != "gpt-4o" {
		t.Errorf("expected default model 'gpt-4o', got %q", cfg.LLM.Model)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "test.yaml")
	yamlContent := `
server:
  port: 9090
database:
  dsn: "postgres://test:test@db:5432/testdb"
redis:
  url: "redis://redis:6379"
llm:
  api_key: "yaml-key"
  api_url: "https://yaml.example.com"
  model: "gpt-3.5-turbo"
auth:
  jwt_secret: "yaml-secret"
log:
  level: "debug"
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Database.DSN != "postgres://test:test@db:5432/testdb" {
		t.Errorf("unexpected DSN: %q", cfg.Database.DSN)
	}
	if cfg.LLM.APIKey != "yaml-key" {
		t.Errorf("unexpected api_key: %q", cfg.LLM.APIKey)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("expected log level 'debug', got %q", cfg.Log.Level)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	// Write a YAML with known values, then override via env vars
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "test.yaml")
	yamlContent := `
server:
  port: 3000
llm:
  api_key: "yaml-key"
  model: "gpt-3.5-turbo"
log:
  level: "debug"
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Set env vars that should override YAML
	t.Setenv("AGENTFI_SERVER_PORT", "4000")
	t.Setenv("AGENTFI_LLM_API_KEY", "env-key")
	t.Setenv("AGENTFI_LLM_MODEL", "claude-3-sonnet")
	t.Setenv("AGENTFI_LOG_LEVEL", "WARN")

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Env should win over YAML
	if cfg.Server.Port != 4000 {
		t.Errorf("env should override port: expected 4000, got %d", cfg.Server.Port)
	}
	if cfg.LLM.APIKey != "env-key" {
		t.Errorf("env should override api_key: expected 'env-key', got %q", cfg.LLM.APIKey)
	}
	if cfg.LLM.Model != "claude-3-sonnet" {
		t.Errorf("env should override model: expected 'claude-3-sonnet', got %q", cfg.LLM.Model)
	}
	// AGENTFI_LOG_LEVEL=WARN should be lowercased to "warn"
	if cfg.Log.Level != "warn" {
		t.Errorf("env log level should be lowercased: expected 'warn', got %q", cfg.Log.Level)
	}
}

func TestMissingYAMLFileUsesDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port, got %d", cfg.Server.Port)
	}
}
