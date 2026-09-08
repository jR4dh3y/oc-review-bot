// Command oc-review-bot runs the PR review bot: webhook server, review
// engine, and embedded dashboard.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/bot"
	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/pool"
	"github.com/jR4dh3y/oc-review-bot/internal/runner"
	"github.com/jR4dh3y/oc-review-bot/internal/seal"
	"github.com/jR4dh3y/oc-review-bot/internal/server"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
	webpkg "github.com/jR4dh3y/oc-review-bot/web"
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		return err
	}
	log = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := runner.Preflight(cfg.OpenCodeBin, cfg.OpenCodeRuntimeDir, cfg.BubblewrapBin); err != nil {
		log.Error("review sandbox preflight", "err", err)
		return err
	}

	app, err := gh.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKey, cfg.WebhookSecret, cfg.BotGitHubID)
	if err != nil {
		log.Error("github app auth", "err", err)
		return err
	}

	enc, err := seal.New(cfg.SessionSecret)
	if err != nil {
		log.Error("cipher", "err", err)
		return err
	}

	st, err := store.Open(cfg.DBPath, enc)
	if err != nil {
		log.Error("open db", "err", err)
		return err
	}
	defer st.Close()

	p := pool.New(st, cfg.ZenCooldown)
	eng := bot.NewEngine(cfg, st, app, p, log)
	if err := eng.Start(workers(cfg.ReviewConcurrency)); err != nil {
		log.Error("start review engine", "err", err)
		return err
	}

	handler := server.New(cfg, st, app, eng, log, webpkg.Dist)
	addr := ":" + cfg.Port
	log.Info("listening", "addr", addr, "public_url", cfg.PublicURL)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopSignals)
	serverDone := make(chan error, 1)
	go func() { serverDone <- httpServer.ListenAndServe() }()
	var serverErr error

	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			serverErr = err
		}
	case sig := <-stopSignals:
		log.Info("shutting down", "signal", sig.String())
	}

	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 15*time.Second)
	if err := httpServer.Shutdown(httpShutdownCtx); err != nil {
		log.Error("shutdown http server", "err", err)
	}
	cancelHTTP()

	engineShutdownCtx, cancelEngine := context.WithTimeout(context.Background(), 30*time.Second)
	engineErr := eng.Stop(engineShutdownCtx)
	cancelEngine()
	if engineErr != nil {
		log.Error("stop review engine", "err", engineErr)
		// Stop may have returned its caller's deadline while its cleanup waiter
		// is still draining. Wait without the expired context before the deferred
		// database close so the lease cannot be left behind.
		if errors.Is(engineErr, context.DeadlineExceeded) {
			if waitErr := eng.Stop(context.Background()); waitErr != nil {
				log.Error("finish review engine cleanup", "err", waitErr)
				return waitErr
			}
		}
		if serverErr == nil {
			serverErr = engineErr
		}
	}
	if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
		return serverErr
	}
	return nil
}

func workers(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
