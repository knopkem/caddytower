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

	"caddytower/internal/auth"
	"caddytower/internal/config"
	"caddytower/internal/secrets"
	"caddytower/internal/server"
	"caddytower/internal/store"
	"caddytower/internal/ui"
	"caddytower/internal/version"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	webUI, err := ui.New()
	if err != nil {
		logger.Error("load embedded UI", "error", err)
		os.Exit(1)
	}

	secretService, err := secrets.NewOptionalFromBase64(cfg.MasterKey)
	if err != nil {
		logger.Error("load master key", "error", err)
		os.Exit(1)
	}

	stateStore, err := store.Open(cfg)
	if err != nil {
		logger.Error("open sqlite store", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := stateStore.Close(); err != nil {
			logger.Error("close sqlite store", "error", err)
		}
	}()

	authService := auth.New(stateStore, secretService, cfg.PublicBaseURL)

	app := server.New(cfg, webUI, logger, version.Current(), stateStore, authService)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown server", "error", err)
		}
	}()

	logger.Info("starting caddytower",
		"addr", cfg.HTTPAddr,
		"public_base_url", cfg.PublicBaseURL,
		"state_db_path", cfg.StateDBPath(),
		"version", version.Version,
		"commit", version.Commit,
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve http", "error", err)
		os.Exit(1)
	}
}
