// Command oc-review-bot runs the PR review bot: webhook server, review
// engine, and embedded dashboard.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/bot"
	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/pool"
	"github.com/jR4dh3y/oc-review-bot/internal/seal"
	"github.com/jR4dh3y/oc-review-bot/internal/server"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
	webpkg "github.com/jR4dh3y/oc-review-bot/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	app, err := gh.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKey, cfg.WebhookSecret)
	if err != nil {
		log.Error("github app auth", "err", err)
		os.Exit(1)
	}

	// SESSION_SECRET is not in config.Load's required list on purpose: the
	// cipher needs it, so enforce it here before opening the DB.
	secret := os.Getenv("SESSION_SECRET")
	if secret == "" {
		log.Error("missing required env: SESSION_SECRET")
		os.Exit(1)
	}
	enc, err := seal.New(secret)
	if err != nil {
		log.Error("cipher", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath, enc)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	p := pool.New(st, time.Until(midnightUTC()))
	eng := bot.NewEngine(cfg, st, app, p, log)
	eng.Start(workers(cfg.ReviewConcurrency))

	handler := server.New(cfg, st, app, eng, log, webpkg.Dist)
	addr := ":" + cfg.Port
	log.Info("listening", "addr", addr, "public_url", cfg.PublicURL)
	if err := http.ListenAndServe(addr, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func workers(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func midnightUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}
