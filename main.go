// PiecesOfLife is a self-hosted, private group newsletter platform.
package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/email"
	"github.com/parithosh/piecesoflife/internal/scheduler"
	"github.com/parithosh/piecesoflife/internal/server"
	"github.com/parithosh/piecesoflife/internal/store"
)

//go:embed static
var staticFS embed.FS

//go:embed templates
var templatesFS embed.FS

func main() {
	if err := run(); err != nil {
		slog.Error("Fatal error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := setupLogger(cfg.LogLevel, cfg.LogFormat)

	logger.InfoContext(ctx, "Starting PiecesOfLife",
		slog.Int("port", cfg.Port),
		slog.String("base_url", cfg.BaseURL),
		slog.Bool("dev_mode", cfg.DevMode),
	)

	// Ensure upload directory exists.
	if err := os.MkdirAll(cfg.UploadPath, 0o755); err != nil {
		return fmt.Errorf("creating upload directory: %w", err)
	}

	// Open database.
	db, err := store.New(ctx, cfg.DatabasePath, logger)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	// Run migrations.
	if err := db.RunMigrations(ctx); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	// Seed data. Order matters: the default group must exist before the
	// admin user can receive a membership in it.
	if err := db.SeedInstanceSettings(ctx); err != nil {
		return fmt.Errorf("seeding instance settings: %w", err)
	}

	if err := db.SeedDefaultGroup(ctx); err != nil {
		return fmt.Errorf("seeding default group: %w", err)
	}

	if err := db.SeedAdminUser(ctx, cfg.AdminEmail); err != nil {
		return fmt.Errorf("seeding admin user: %w", err)
	}

	if err := db.SeedQuestionBank(ctx); err != nil {
		return fmt.Errorf("seeding question bank: %w", err)
	}

	// Email sender.
	emailSender := email.NewSender(cfg, logger)

	// HTTP server.
	srv := server.New(db, cfg, emailSender, logger, staticFS, templatesFS)

	// Scheduler — depends on server for reminder + auto-publish callbacks.
	// Use a separate context so Stop() can drain independently of HTTP shutdown.
	schedCtx, schedCancel := context.WithCancel(context.Background())
	defer schedCancel()

	sched := scheduler.New(db, srv, logger)
	sched.Start(schedCtx)

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in a goroutine.
	errCh := make(chan error, 1)

	go func() {
		logger.InfoContext(ctx, "HTTP server listening",
			slog.String("addr", httpServer.Addr),
		)

		if err := httpServer.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			errCh <- fmt.Errorf("http server error: %w", err)
		}
	}()

	// Wait for shutdown signal or server error.
	select {
	case <-ctx.Done():
		logger.InfoContext(context.Background(), "Shutdown signal received")
	case err := <-errCh:
		// Stop scheduler on unexpected server error too.
		schedCancel()
		sched.Stop()
		return err
	}

	// Graceful shutdown: stop accepting HTTP, drain in-flight requests, then
	// stop the scheduler. Order matters — stopping the scheduler first could
	// cause an in-flight publish/reminder handler to fail mid-way.
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	schedCancel()
	sched.Stop()

	logger.InfoContext(context.Background(), "Server stopped gracefully")

	return nil
}

func setupLogger(level, format string) *slog.Logger {
	var lvl slog.Level

	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
