// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration, sourced from env vars.
type Config struct {
	Port                string
	PublicURL           string
	DBPath              string
	GitHubAppID         string
	GitHubAppPrivateKey string // PEM contents, resolved from path or literal
	WebhookSecret       string
	OAuthClientID       string
	OAuthClientSecret   string
	SessionSecret       string
	AdminLogins         []string
	BotUsername         string
	DefaultModel        string
	ReviewConcurrency   int
	ReviewTimeout       time.Duration
	OpenCodeBin         string
	OpenCodeArgs        []string
}

// Load reads env vars and returns the config. It returns an error when a
// required variable is missing or malformed.
func Load() (*Config, error) {
	c := &Config{
		Port:              env("PORT", "8080"),
		PublicURL:         strings.TrimRight(env("PUBLIC_URL", "http://localhost:"+env("PORT", "8080")), "/"),
		DBPath:            env("DB_PATH", "oc-review-bot.db"),
		GitHubAppID:       os.Getenv("GITHUB_APP_ID"),
		WebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		OAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SessionSecret:     os.Getenv("SESSION_SECRET"),
		BotUsername:       env("BOT_USERNAME", "oc-review-bot"),
		DefaultModel:      env("ZEN_DEFAULT_MODEL", "opencode/big-pickle"),
		OpenCodeBin:       env("OPENCODE_BIN", "opencode2"),
	}
	c.ReviewConcurrency = envInt("REVIEW_CONCURRENCY", 2)
	c.ReviewTimeout = time.Duration(envInt("REVIEW_TIMEOUT_MINUTES", 20)) * time.Minute

	if c.OpenCodeBin == "opencode2" {
		// The v2 CLI talks to a background service by default; each review
		// must run against its own isolated server and config.
		c.OpenCodeArgs = []string{"--standalone"}
	}

	for _, login := range strings.Split(os.Getenv("ADMIN_GITHUB_LOGINS"), ",") {
		if login = strings.TrimSpace(strings.TrimPrefix(login, "@")); login != "" {
			c.AdminLogins = append(c.AdminLogins, strings.ToLower(login))
		}
	}

	pem := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_PATH: %w", err)
		}
		pem = string(b)
	}
	c.GitHubAppPrivateKey = pem

	var missing []string
	if c.GitHubAppID == "" {
		missing = append(missing, "GITHUB_APP_ID")
	}
	if c.GitHubAppPrivateKey == "" {
		missing = append(missing, "GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH")
	}
	if c.WebhookSecret == "" {
		missing = append(missing, "GITHUB_WEBHOOK_SECRET")
	}
	if c.SessionSecret == "" {
		missing = append(missing, "SESSION_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// OAuthConfigured reports whether GitHub OAuth login can work.
func (c *Config) OAuthConfigured() bool {
	return c.OAuthClientID != "" && c.OAuthClientSecret != ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
