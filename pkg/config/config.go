// Package config provides configuration loading from YAML files and environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all application configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	LLM      LLMConfig      `yaml:"llm"`
	Auth     AuthConfig     `yaml:"auth"`
	Log      LogConfig      `yaml:"log"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

type RedisConfig struct {
	URL string `yaml:"url"`
}

type LLMConfig struct {
	APIKey string `yaml:"api_key"`
	APIURL string `yaml:"api_url"`
	Model  string `yaml:"model"`
}

type AuthConfig struct {
	JWTSecret string `yaml:"jwt_secret"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

// Load reads configuration from a YAML file, then applies environment variable
// overrides. Environment variables take precedence over YAML values (Req 11.2).
// Env var format: AGENTFI_SERVER_PORT, AGENTFI_DATABASE_DSN, etc.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		if err := loadYAML(path, cfg); err != nil {
			return nil, fmt.Errorf("load yaml config: %w", err)
		}
	}

	applyEnv(cfg)
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server:   ServerConfig{Port: 8080},
		Database: DatabaseConfig{DSN: "postgres://postgres:postgres@localhost:5432/agentfi?sslmode=disable"},
		Redis:    RedisConfig{URL: "redis://localhost:6379"},
		LLM:      LLMConfig{Model: "gpt-4o"},
		Auth:     AuthConfig{JWTSecret: "change-me"},
		Log:      LogConfig{Level: "info"},
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no config file is fine, use defaults + env
		}
		return err
	}
	return yaml.Unmarshal(data, cfg)
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("AGENTFI_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("AGENTFI_DATABASE_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("AGENTFI_REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}
	if v := os.Getenv("AGENTFI_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("AGENTFI_LLM_API_URL"); v != "" {
		cfg.LLM.APIURL = v
	}
	if v := os.Getenv("AGENTFI_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("AGENTFI_AUTH_JWT_SECRET"); v != "" {
		cfg.Auth.JWTSecret = v
	}
	if v := os.Getenv("AGENTFI_LOG_LEVEL"); v != "" {
		cfg.Log.Level = strings.ToLower(v)
	}
}
