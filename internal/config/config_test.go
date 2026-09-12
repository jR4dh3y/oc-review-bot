package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLogLevel(t *testing.T) {
	cases := map[string]struct {
		want    slog.Level
		wantErr bool
	}{
		"":        {want: slog.LevelInfo},
		"debug":   {want: slog.LevelDebug},
		"INFO":    {want: slog.LevelInfo},
		" Warn ":  {want: slog.LevelWarn},
		"warning": {want: slog.LevelWarn},
		"error":   {want: slog.LevelError},
		"verbose": {wantErr: true},
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LOG_LEVEL", value)
			cfg, err := Load()
			if want.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error, want LOG_LEVEL rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.LogLevel != want.want {
				t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, want.want)
			}
		})
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PORT", "")
	t.Setenv("PUBLIC_URL", "")
	t.Setenv("DB_PATH", "")
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "test-pem")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("BOT_GITHUB_ID", "707")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	t.Setenv("ADMIN_GITHUB_LOGINS", "")
	t.Setenv("ADMIN_GITHUB_IDS", "101, 202")
	t.Setenv("REVIEWER_GITHUB_IDS", "303")
	t.Setenv("ALLOWED_GITHUB_INSTALLATION_IDS", "404")
	t.Setenv("ALLOWED_GITHUB_REPOSITORY_IDS", "505, 606")
	t.Setenv("BOT_USERNAME", "")
	t.Setenv("COOKIE_SECURE", "")
	t.Setenv("ZEN_DEFAULT_MODEL", "opencode/reviewer")
	t.Setenv("REVIEW_CONCURRENCY", "2")
	t.Setenv("REVIEW_TIMEOUT_MINUTES", "20")
	t.Setenv("ZEN_COOLDOWN_MINUTES", "60")
	t.Setenv("USER_REVIEWS_PER_HOUR", "6")
	t.Setenv("REPO_REVIEWS_PER_HOUR", "30")
	t.Setenv("MAX_ACTIVE_REVIEWS", "50")
	t.Setenv("OPENCODE_BIN", "opencode2")
	t.Setenv("OPENCODE_RUNTIME_DIR", "/opt/opencode-runtime")
	t.Setenv("BUBBLEWRAP_BIN", "/usr/bin/bwrap")
}

func TestLoadUsesImmutableGitHubIDAllowlists(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	for _, tc := range []struct {
		name string
		got  []int64
		want []int64
	}{
		{"admins", cfg.AdminGitHubIDs, []int64{101, 202}},
		{"reviewers", cfg.ReviewerGitHubIDs, []int64{303}},
		{"installations", cfg.InstallationIDs, []int64{404}},
		{"repositories", cfg.RepositoryIDs, []int64{505, 606}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("IDs = %v, want %v", tc.got, tc.want)
			}
		})
	}
	if cfg.BotGitHubID != 707 || cfg.CookieSecure {
		t.Fatalf("unexpected bot/cookie config: bot=%d secure=%t", cfg.BotGitHubID, cfg.CookieSecure)
	}

	if !cfg.IsAdminGitHubID(101) || cfg.IsAdminGitHubID(303) {
		t.Fatalf("unexpected administrator authorization")
	}
	for _, tc := range []struct {
		name                             string
		githubID, installationID, repoID int64
		want                             bool
	}{
		{"admin on allowed target", 101, 404, 505, true},
		{"reviewer on allowed target", 303, 404, 606, true},
		{"unlisted user", 999, 404, 505, false},
		{"unlisted installation", 101, 405, 505, false},
		{"unlisted repository", 101, 404, 607, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.CanRequestReview(tc.githubID, tc.installationID, tc.repoID); got != tc.want {
				t.Fatalf("CanRequestReview() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestLoadRejectsLegacyLoginAdminConfiguration(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ADMIN_GITHUB_LOGINS", "root")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADMIN_GITHUB_LOGINS is unsafe and unsupported") {
		t.Fatalf("legacy login error = %v", err)
	}
}

func TestLoadRequiresReviewBoundaryAndSandboxConfiguration(t *testing.T) {
	for _, key := range []string{
		"BOT_GITHUB_ID",
		"ADMIN_GITHUB_IDS",
		"ALLOWED_GITHUB_INSTALLATION_IDS",
		"ALLOWED_GITHUB_REPOSITORY_IDS",
		"OPENCODE_RUNTIME_DIR",
		"BUBBLEWRAP_BIN",
	} {
		t.Run(key, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(key, "")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing %s error = %v", key, err)
			}
		})
	}
}

func TestLoadRejectsMalformedGitHubIDLists(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"non-numeric admin", "ADMIN_GITHUB_IDS", "admin"},
		{"zero reviewer", "REVIEWER_GITHUB_IDS", "0"},
		{"negative installation", "ALLOWED_GITHUB_INSTALLATION_IDS", "-1"},
		{"duplicate repository", "ALLOWED_GITHUB_REPOSITORY_IDS", "505, 505"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("invalid %s error = %v", tc.key, err)
			}
		})
	}
}

func TestLoadUsesDefaultsAndStandaloneForOpenCode2(t *testing.T) {
	setRequiredEnv(t)
	for _, key := range []string{
		"PORT",
		"PUBLIC_URL",
		"DB_PATH",
		"BOT_USERNAME",
		"REVIEW_CONCURRENCY",
		"REVIEW_TIMEOUT_MINUTES",
		"ZEN_COOLDOWN_MINUTES",
		"USER_REVIEWS_PER_HOUR",
		"REPO_REVIEWS_PER_HOUR",
		"MAX_ACTIVE_REVIEWS",
		"OPENCODE_BIN",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.PublicURL != "http://localhost:8080" || cfg.DBPath != "samik-bot.db" {
		t.Fatalf("unexpected network/database defaults: port=%q publicURL=%q dbPath=%q", cfg.Port, cfg.PublicURL, cfg.DBPath)
	}
	if cfg.BotUsername != "samik-bot" || cfg.OpenCodeBin != "opencode2" {
		t.Fatalf("unexpected bot/runtime defaults: bot=%q bin=%q", cfg.BotUsername, cfg.OpenCodeBin)
	}
	if cfg.ReviewConcurrency != 2 || cfg.ReviewTimeout != 20*time.Minute || cfg.ZenCooldown != time.Hour ||
		cfg.UserReviewsPerHour != 6 || cfg.RepoReviewsPerHour != 30 || cfg.MaxActiveReviews != 50 {
		t.Fatalf("unexpected review defaults: concurrency=%d timeout=%s cooldown=%s user=%d repo=%d active=%d",
			cfg.ReviewConcurrency, cfg.ReviewTimeout, cfg.ZenCooldown, cfg.UserReviewsPerHour, cfg.RepoReviewsPerHour, cfg.MaxActiveReviews)
	}
	if !reflect.DeepEqual(cfg.OpenCodeArgs, []string{"--standalone"}) {
		t.Fatalf("OpenCode args = %v, want [--standalone]", cfg.OpenCodeArgs)
	}
}

func TestLoadUsesStandaloneForOpenCode2Path(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("OPENCODE_BIN", "/opt/opencode-runtime/bin/opencode2")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.OpenCodeArgs, []string{"--standalone"}) {
		t.Fatalf("OpenCode args = %v, want [--standalone]", cfg.OpenCodeArgs)
	}
}

func TestLoadSelectsPiEngineWithSeparateRuntimeConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REVIEW_ENGINE", "pi")
	t.Setenv("PI_RUNTIME_DIR", "/opt/pi-runtime")
	t.Setenv("PI_BIN", "")
	// The OpenCode runtime is not required while pi is active; its separate
	// configuration may remain absent or partially staged.
	t.Setenv("OPENCODE_RUNTIME_DIR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewEngine != EnginePi || cfg.PiBin != "pi" || cfg.PiRuntimeDir != "/opt/pi-runtime" {
		t.Fatalf("pi engine config = %q %q %q", cfg.ReviewEngine, cfg.PiBin, cfg.PiRuntimeDir)
	}
	if cfg.OpenCodeArgs != nil {
		t.Fatalf("OpenCode args = %v, want none for the pi engine", cfg.OpenCodeArgs)
	}
	engine, bin, runtimeDir := cfg.AgentRuntime()
	if engine != EnginePi || bin != "pi" || runtimeDir != "/opt/pi-runtime" {
		t.Fatalf("AgentRuntime() = %q %q %q, want the pi staging", engine, bin, runtimeDir)
	}
}

func TestLoadKeepsOpenCodeEngineSeparateFromPiConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REVIEW_ENGINE", "")
	t.Setenv("PI_RUNTIME_DIR", "/opt/pi-runtime")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewEngine != EngineOpenCode2 {
		t.Fatalf("ReviewEngine = %q, want the opencode2 default", cfg.ReviewEngine)
	}
	if !reflect.DeepEqual(cfg.OpenCodeArgs, []string{"--standalone"}) {
		t.Fatalf("OpenCode args = %v, want [--standalone]", cfg.OpenCodeArgs)
	}
	engine, bin, runtimeDir := cfg.AgentRuntime()
	if engine != EngineOpenCode2 || bin != "opencode2" || runtimeDir != "/opt/opencode-runtime" {
		t.Fatalf("AgentRuntime() = %q %q %q, want the opencode2 staging", engine, bin, runtimeDir)
	}
}

func TestLoadRejectsUnknownReviewEngine(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REVIEW_ENGINE", "claude")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REVIEW_ENGINE") {
		t.Fatalf("unknown engine error = %v", err)
	}
}

func TestLoadRequiresPiRuntimeForPiEngine(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REVIEW_ENGINE", "pi")
	t.Setenv("PI_RUNTIME_DIR", "")
	t.Setenv("PI_BIN", "pi")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PI_RUNTIME_DIR") {
		t.Fatalf("missing pi runtime error = %v", err)
	}
}

func TestLoadRejectsPiBinNamedForAnotherEngine(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REVIEW_ENGINE", "pi")
	t.Setenv("PI_RUNTIME_DIR", "/opt/pi-runtime")
	for _, value := range []string{"opencode2", "/opt/pi-runtime/bin/opencode2", "picli"} {
		t.Setenv("PI_BIN", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PI_BIN") {
			t.Fatalf("PI_BIN %q error = %v, want naming rejection", value, err)
		}
	}
}

func TestLoadNormalizesHTTPSPublicURLAndSecuresCookies(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PUBLIC_URL", "HTTPS://example.test/")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://example.test" {
		t.Fatalf("PublicURL = %q, want normalized https URL", cfg.PublicURL)
	}
	if !cfg.CookieSecure {
		t.Fatal("CookieSecure = false for HTTPS PUBLIC_URL")
	}
}

func TestLoadValidatesCurrentConstraints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"short session secret", "SESSION_SECRET", "too-short"},
		{"missing model", "ZEN_DEFAULT_MODEL", ""},
		{"invalid model", "ZEN_DEFAULT_MODEL", "provider/model/extra"},
		{"zero concurrency", "REVIEW_CONCURRENCY", "0"},
		{"zero timeout", "REVIEW_TIMEOUT_MINUTES", "0"},
		{"zero cooldown", "ZEN_COOLDOWN_MINUTES", "0"},
		{"zero user limit", "USER_REVIEWS_PER_HOUR", "0"},
		{"zero repository limit", "REPO_REVIEWS_PER_HOUR", "0"},
		{"zero active limit", "MAX_ACTIVE_REVIEWS", "0"},
		{"v1 OpenCode binary", "OPENCODE_BIN", "opencode"},
		{"invalid bot login", "BOT_USERNAME", "@samik-bot"},
		{"invalid bot ID", "BOT_GITHUB_ID", "0"},
		{"invalid cookie secure", "COOKIE_SECURE", "sometimes"},
		{"public HTTP URL", "PUBLIC_URL", "http://example.com"},
		{"invalid port", "PORT", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("invalid %s error = %v", tc.key, err)
			}
		})
	}
}

func TestValidModel(t *testing.T) {
	for _, model := range []string{"opencode/reviewer", "openai/gpt-5#high"} {
		if !ValidModel(model) {
			t.Errorf("ValidModel(%q) = false", model)
		}
	}
	for _, model := range []string{"", "model", "provider/", "provider/model/extra", " provider/model", "--model"} {
		if ValidModel(model) {
			t.Errorf("ValidModel(%q) = true", model)
		}
	}
}
