// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reviewer engines. The default keeps the OpenCode 2 beta integration; "pi"
// selects the pi coding agent against the same pooled Zen credentials.
const (
	EngineOpenCode2 = "opencode2"
	EnginePi        = "pi"
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
	AdminGitHubIDs      []int64
	ReviewerGitHubIDs   []int64
	InstallationIDs     []int64
	RepositoryIDs       []int64
	BotGitHubID         int64
	BotUsername         string
	CookieSecure        bool
	DefaultModel        string
	ZenCooldown         time.Duration
	ReviewConcurrency   int
	ReviewTimeout       time.Duration
	UserReviewsPerHour  int
	RepoReviewsPerHour  int
	MaxActiveReviews    int
	ReviewEngine        string // active reviewer engine: EngineOpenCode2 (default) or EnginePi
	OpenCodeBin         string
	OpenCodeRuntimeDir  string
	PiBin               string
	PiRuntimeDir        string
	BubblewrapBin       string
	OpenCodeArgs        []string // flags passed after `opencode2 run`; empty for pi
	LogLevel            slog.Level
}

// Load reads env vars and returns the config. It returns an error when a
// required variable is missing or malformed.
func Load() (*Config, error) {
	c := &Config{
		Port:               env("PORT", "8080"),
		PublicURL:          normalizePublicURLScheme(strings.TrimRight(env("PUBLIC_URL", "http://localhost:"+env("PORT", "8080")), "/")),
		DBPath:             env("DB_PATH", "oc-review-bot.db"),
		GitHubAppID:        os.Getenv("GITHUB_APP_ID"),
		WebhookSecret:      os.Getenv("GITHUB_WEBHOOK_SECRET"),
		OAuthClientID:      os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		OAuthClientSecret:  os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SessionSecret:      os.Getenv("SESSION_SECRET"),
		BotUsername:        env("BOT_USERNAME", "oc-review-bot"),
		DefaultModel:       strings.TrimSpace(os.Getenv("ZEN_DEFAULT_MODEL")),
		ReviewEngine:       strings.TrimSpace(env("REVIEW_ENGINE", EngineOpenCode2)),
		OpenCodeBin:        strings.TrimSpace(env("OPENCODE_BIN", "opencode2")),
		OpenCodeRuntimeDir: strings.TrimSpace(os.Getenv("OPENCODE_RUNTIME_DIR")),
		PiBin:              strings.TrimSpace(env("PI_BIN", "pi")),
		PiRuntimeDir:       strings.TrimSpace(os.Getenv("PI_RUNTIME_DIR")),
		BubblewrapBin:      strings.TrimSpace(os.Getenv("BUBBLEWRAP_BIN")),
	}
	botIDs, err := envIDList("BOT_GITHUB_ID", true)
	if err != nil {
		return nil, err
	}
	if len(botIDs) != 1 {
		return nil, fmt.Errorf("BOT_GITHUB_ID must contain exactly one positive GitHub user ID")
	}
	c.BotGitHubID = botIDs[0]
	c.CookieSecure, err = envBool("COOKIE_SECURE", publicURLUsesScheme(c.PublicURL, "https"))
	if err != nil {
		return nil, err
	}
	if c.ReviewConcurrency, err = envInt("REVIEW_CONCURRENCY", 2); err != nil {
		return nil, err
	}
	var reviewTimeoutMinutes, cooldownMinutes int
	if reviewTimeoutMinutes, err = envInt("REVIEW_TIMEOUT_MINUTES", 20); err != nil {
		return nil, err
	}
	if cooldownMinutes, err = envInt("ZEN_COOLDOWN_MINUTES", 60); err != nil {
		return nil, err
	}
	c.ReviewTimeout = time.Duration(reviewTimeoutMinutes) * time.Minute
	c.ZenCooldown = time.Duration(cooldownMinutes) * time.Minute
	if c.UserReviewsPerHour, err = envInt("USER_REVIEWS_PER_HOUR", 6); err != nil {
		return nil, err
	}
	if c.RepoReviewsPerHour, err = envInt("REPO_REVIEWS_PER_HOUR", 30); err != nil {
		return nil, err
	}
	if c.MaxActiveReviews, err = envInt("MAX_ACTIVE_REVIEWS", 50); err != nil {
		return nil, err
	}
	if c.LogLevel, err = envLogLevel("LOG_LEVEL"); err != nil {
		return nil, err
	}

	// Engine-specific staging is validated per engine so one deployment can
	// hold both configurations and switch REVIEW_ENGINE without re-staging.
	switch c.ReviewEngine {
	case EngineOpenCode2:
		if !isOpenCode2Bin(c.OpenCodeBin) {
			return nil, fmt.Errorf("OPENCODE_BIN must name the OpenCode 2 opencode2 executable")
		}
		// The v2 CLI talks to a background service by default; each review must
		// run against its own isolated server and config, including when a full
		// executable path is configured.
		c.OpenCodeArgs = []string{"--standalone"}
	case EnginePi:
		if !isPiBin(c.PiBin) {
			return nil, fmt.Errorf("PI_BIN must name the pi executable")
		}
	default:
		return nil, fmt.Errorf("REVIEW_ENGINE must be %q or %q", EngineOpenCode2, EnginePi)
	}
	if !validGitHubLogin(c.BotUsername) {
		return nil, fmt.Errorf("BOT_USERNAME must be a GitHub login without @")
	}

	if c.AdminGitHubIDs, err = envIDList("ADMIN_GITHUB_IDS", true); err != nil {
		return nil, err
	}
	if c.ReviewerGitHubIDs, err = envIDList("REVIEWER_GITHUB_IDS", false); err != nil {
		return nil, err
	}
	if c.InstallationIDs, err = envIDList("ALLOWED_GITHUB_INSTALLATION_IDS", true); err != nil {
		return nil, err
	}
	if c.RepositoryIDs, err = envIDList("ALLOWED_GITHUB_REPOSITORY_IDS", true); err != nil {
		return nil, err
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
	if c.BotGitHubID < 1 {
		missing = append(missing, "BOT_GITHUB_ID")
	}
	if c.SessionSecret == "" {
		missing = append(missing, "SESSION_SECRET")
	} else if len(c.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 bytes")
	}
	if strings.TrimSpace(os.Getenv("ADMIN_GITHUB_LOGINS")) != "" {
		return nil, fmt.Errorf("ADMIN_GITHUB_LOGINS is unsafe and unsupported; use ADMIN_GITHUB_IDS")
	}
	if len(c.AdminGitHubIDs) == 0 {
		missing = append(missing, "ADMIN_GITHUB_IDS")
	}
	if len(c.InstallationIDs) == 0 {
		missing = append(missing, "ALLOWED_GITHUB_INSTALLATION_IDS")
	}
	if len(c.RepositoryIDs) == 0 {
		missing = append(missing, "ALLOWED_GITHUB_REPOSITORY_IDS")
	}
	if c.DefaultModel == "" {
		missing = append(missing, "ZEN_DEFAULT_MODEL")
	} else if !ValidModel(c.DefaultModel) {
		return nil, fmt.Errorf("ZEN_DEFAULT_MODEL must be a provider/model identifier")
	}
	if c.ReviewConcurrency < 1 {
		return nil, fmt.Errorf("REVIEW_CONCURRENCY must be at least 1")
	}
	if c.ReviewTimeout <= 0 {
		return nil, fmt.Errorf("REVIEW_TIMEOUT_MINUTES must be at least 1")
	}
	if c.ZenCooldown <= 0 {
		return nil, fmt.Errorf("ZEN_COOLDOWN_MINUTES must be at least 1")
	}
	if c.UserReviewsPerHour < 1 {
		return nil, fmt.Errorf("USER_REVIEWS_PER_HOUR must be at least 1")
	}
	if c.RepoReviewsPerHour < 1 {
		return nil, fmt.Errorf("REPO_REVIEWS_PER_HOUR must be at least 1")
	}
	if c.MaxActiveReviews < 1 {
		return nil, fmt.Errorf("MAX_ACTIVE_REVIEWS must be at least 1")
	}
	switch c.ReviewEngine {
	case EnginePi:
		if c.PiRuntimeDir == "" {
			missing = append(missing, "PI_RUNTIME_DIR")
		}
	default:
		if c.OpenCodeRuntimeDir == "" {
			missing = append(missing, "OPENCODE_RUNTIME_DIR")
		}
	}
	if c.BubblewrapBin == "" {
		missing = append(missing, "BUBBLEWRAP_BIN")
	}
	if err := validatePublicURL(c.PublicURL); err != nil {
		return nil, err
	}
	if publicURLUsesScheme(c.PublicURL, "https") && !c.CookieSecure {
		return nil, fmt.Errorf("COOKIE_SECURE must be true when PUBLIC_URL uses https")
	}
	if c.CookieSecure && publicURLUsesScheme(c.PublicURL, "http") && !isLoopbackHost(mustURLHost(c.PublicURL)) {
		return nil, fmt.Errorf("COOKIE_SECURE must be false for non-loopback http PUBLIC_URL")
	}
	if err := validatePort(c.Port); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func isOpenCode2Bin(bin string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(bin)), ".exe")
	return base == "opencode2"
}

func isPiBin(bin string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(bin)), ".exe")
	return base == "pi"
}

// AgentRuntime returns the active reviewer engine with its executable and
// trusted runtime directory. The inactive engine's configuration is never
// consulted, so deployments can stage or retain both independently.
func (c *Config) AgentRuntime() (engine, bin, runtimeDir string) {
	if c.ReviewEngine == EnginePi {
		return EnginePi, c.PiBin, c.PiRuntimeDir
	}
	return EngineOpenCode2, c.OpenCodeBin, c.OpenCodeRuntimeDir
}

func validGitHubLogin(login string) bool {
	if login == "" || len(login) > 39 || strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") {
		return false
	}
	for _, r := range login {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return true
}

func envIDList(key string, required bool) ([]int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		if required {
			return nil, nil
		}
		return []int64{}, nil
	}
	ids := make([]int64, 0, len(strings.Split(raw, ",")))
	seen := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id < 1 {
			return nil, fmt.Errorf("%s must be a comma-separated list of positive GitHub IDs", key)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate GitHub ID %d", key, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// IsAdminGitHubID is the authoritative, immutable administrator allowlist.
func (c *Config) IsAdminGitHubID(githubID int64) bool {
	return containsID(c.AdminGitHubIDs, githubID)
}

// CanRequestReview enforces the service's explicit paid-review boundary.
// Administrators may request reviews; other users need an immutable operator grant.
func (c *Config) CanRequestReview(githubID, installationID, repositoryID int64) bool {
	if !c.AllowsReviewTarget(installationID, repositoryID) {
		return false
	}
	return c.IsAdminGitHubID(githubID) || containsID(c.ReviewerGitHubIDs, githubID)
}

// AllowsReviewTarget limits all webhook and worker activity to operator-owned
// installation and repository IDs before user entitlement is considered.
func (c *Config) AllowsReviewTarget(installationID, repositoryID int64) bool {
	return containsID(c.InstallationIDs, installationID) && containsID(c.RepositoryIDs, repositoryID)
}

func containsID(ids []int64, value int64) bool {
	for _, id := range ids {
		if id == value {
			return true
		}
	}
	return false
}

func normalizePublicURLScheme(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return parsed.String()
}

func publicURLUsesScheme(raw, scheme string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && strings.EqualFold(parsed.Scheme, scheme)
}

func validatePublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
		return fmt.Errorf("PUBLIC_URL must be an absolute http or https URL without credentials, query, or fragment")
	}
	if strings.EqualFold(parsed.Scheme, "http") && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("PUBLIC_URL must use https outside local development")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validatePort(value string) error {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be an integer between 1 and 65535")
	}
	return nil
}

// OAuthConfigured reports whether GitHub OAuth login can work.
func (c *Config) OAuthConfigured() bool {
	return c.OAuthClientID != "" && c.OAuthClientSecret != ""
}

// ValidModel accepts the provider/model[#variant] form documented by OpenCode.
func ValidModel(model string) bool {
	if model != strings.TrimSpace(model) || strings.ContainsAny(model, "\t\r\n ") {
		return false
	}
	parts := strings.Split(model, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.HasPrefix(model, "-")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}

func envBool(key string, fallback bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}

func envLogLevel(key string) (slog.Level, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return slog.LevelInfo, nil
	}
	switch v {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%s must be debug, info, warn, or error", key)
}

func mustURLHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
