// Package main is the entrypoint for the URL shortener HTTP service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mmuqiitf/url-shortener/internal/config"
	"github.com/mmuqiitf/url-shortener/internal/handler"
	"github.com/mmuqiitf/url-shortener/internal/middleware"
	"github.com/mmuqiitf/url-shortener/internal/repository"
	"github.com/mmuqiitf/url-shortener/internal/service"
	"github.com/mmuqiitf/url-shortener/internal/tracker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	// Ensure SQLite database directory exists
	if dir := filepath.Dir(cfg.DBPath); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := repository.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("repository open: %w", err)
	}
	defer repo.Close()

	// Initialize click-tracking background worker pool
	tr := tracker.New(repo, logger, tracker.Config{
		Workers:       cfg.TrackerWorkers,
		BufferSize:    cfg.TrackerBuffer,
		BatchSize:     cfg.TrackerBatch,
		FlushInterval: cfg.TrackerFlush,
	})
	tr.Run(ctx)

	svc := service.New(repo, nil)
	h := handler.New(svc, tr, repo, logger, cfg.BaseURL)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Logging(logger))
	r.Use(middleware.Recover(logger))
	r.Use(middleware.CORS("*"))
	r.Mount("/", h.Routes())

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", "port", cfg.Port, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down gracefully...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout())
	defer cancel()

	// 1. Stop receiving new HTTP requests first
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", "err", err)
	}

	// 2. Drain buffered events in the tracker worker pool
	if err := tr.Shutdown(shutdownCtx); err != nil {
		logger.Error("tracker shutdown failed", "err", err)
	}

	logger.Info("server stopped")
	return nil
}
