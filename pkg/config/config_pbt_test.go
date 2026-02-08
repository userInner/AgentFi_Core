package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Feature: agentfi-go-backend, Property 24: Config precedence
// For any configuration key set in both the YAML file and an environment variable,
// the environment variable value SHALL take precedence.
// Validates: Requirements 11.2
func TestPropertyConfigPrecedence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random config values for YAML
		yamlPort := rapid.IntRange(1024, 65535).Draw(rt, "yaml_port")
		yamlDSN := rapid.StringMatching(`postgres://[a-z]{3,8}:[a-z]{3,8}@[a-z]{3,8}:5432/[a-z]{3,8}`).Draw(rt, "yaml_dsn")
		yamlRedis := rapid.StringMatching(`redis://[a-z]{3,8}:6379`).Draw(rt, "yaml_redis")
		yamlAPIKey := rapid.StringMatching(`yaml-[a-z]{4,10}`).Draw(rt, "yaml_api_key")
		yamlAPIURL := rapid.StringMatching(`https://[a-z]{3,10}\.example\.com`).Draw(rt, "yaml_api_url")
		yamlModel := rapid.SampledFrom([]string{"gpt-3.5-turbo", "gpt-4", "claude-2", "llama-3"}).Draw(rt, "yaml_model")
		yamlJWTSecret := rapid.StringMatching(`yaml-secret-[a-z]{4,10}`).Draw(rt, "yaml_jwt_secret")
		yamlLogLevel := rapid.SampledFrom([]string{"debug", "info", "warn", "error"}).Draw(rt, "yaml_log_level")

		// Generate different env var values to verify precedence
		envPort := rapid.IntRange(1024, 65535).Filter(func(v int) bool { return v != yamlPort }).Draw(rt, "env_port")
		envDSN := rapid.StringMatching(`postgres://[a-z]{3,8}:[a-z]{3,8}@[a-z]{3,8}:5432/[a-z]{3,8}`).Filter(func(v string) bool { return v != yamlDSN }).Draw(rt, "env_dsn")
		envRedis := rapid.StringMatching(`redis://[a-z]{3,8}:6379`).Filter(func(v string) bool { return v != yamlRedis }).Draw(rt, "env_redis")
		envAPIKey := rapid.StringMatching(`env-[a-z]{4,10}`).Draw(rt, "env_api_key")
		envAPIURL := rapid.StringMatching(`https://[a-z]{3,10}\.env\.com`).Draw(rt, "env_api_url")
		envModel := rapid.SampledFrom([]string{"gpt-4o", "claude-3-sonnet", "mistral-7b", "gemini-pro"}).Draw(rt, "env_model")
		envJWTSecret := rapid.StringMatching(`env-secret-[a-z]{4,10}`).Draw(rt, "env_jwt_secret")
		envLogLevel := rapid.SampledFrom([]string{"DEBUG", "INFO", "WARN", "ERROR"}).Draw(rt, "env_log_level")

		// Write YAML config to temp file
		dir := t.TempDir()
		yamlPath := filepath.Join(dir, "config.yaml")
		yamlContent := fmt.Sprintf(`server:
  port: %d
database:
  dsn: %q
redis:
  url: %q
llm:
  api_key: %q
  api_url: %q
  model: %q
auth:
  jwt_secret: %q
log:
  level: %q
`, yamlPort, yamlDSN, yamlRedis, yamlAPIKey, yamlAPIURL, yamlModel, yamlJWTSecret, yamlLogLevel)

		if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}

		// Set env vars that should override YAML
		envVars := map[string]string{
			"AGENTFI_SERVER_PORT":    fmt.Sprintf("%d", envPort),
			"AGENTFI_DATABASE_DSN":   envDSN,
			"AGENTFI_REDIS_URL":      envRedis,
			"AGENTFI_LLM_API_KEY":    envAPIKey,
			"AGENTFI_LLM_API_URL":    envAPIURL,
			"AGENTFI_LLM_MODEL":      envModel,
			"AGENTFI_AUTH_JWT_SECRET": envJWTSecret,
			"AGENTFI_LOG_LEVEL":      envLogLevel,
		}
		for k, v := range envVars {
			os.Setenv(k, v)
		}
		defer func() {
			for k := range envVars {
				os.Unsetenv(k)
			}
		}()

		// Load config — env vars should override YAML
		cfg, err := Load(yamlPath)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		// Assert: every field matches the env var value, not the YAML value
		if cfg.Server.Port != envPort {
			t.Errorf("Server.Port: env should win: got %d, want %d (yaml was %d)", cfg.Server.Port, envPort, yamlPort)
		}
		if cfg.Database.DSN != envDSN {
			t.Errorf("Database.DSN: env should win: got %q, want %q", cfg.Database.DSN, envDSN)
		}
		if cfg.Redis.URL != envRedis {
			t.Errorf("Redis.URL: env should win: got %q, want %q", cfg.Redis.URL, envRedis)
		}
		if cfg.LLM.APIKey != envAPIKey {
			t.Errorf("LLM.APIKey: env should win: got %q, want %q", cfg.LLM.APIKey, envAPIKey)
		}
		if cfg.LLM.APIURL != envAPIURL {
			t.Errorf("LLM.APIURL: env should win: got %q, want %q", cfg.LLM.APIURL, envAPIURL)
		}
		if cfg.LLM.Model != envModel {
			t.Errorf("LLM.Model: env should win: got %q, want %q", cfg.LLM.Model, envModel)
		}
		if cfg.Auth.JWTSecret != envJWTSecret {
			t.Errorf("Auth.JWTSecret: env should win: got %q, want %q", cfg.Auth.JWTSecret, envJWTSecret)
		}
		// Log level env is uppercase, applyEnv lowercases it
		if cfg.Log.Level != strings.ToLower(envLogLevel) {
			t.Errorf("Log.Level: env should win (lowercased): got %q, want %q", cfg.Log.Level, strings.ToLower(envLogLevel))
		}
	})
}
