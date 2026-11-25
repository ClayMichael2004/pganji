package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pganji/api"
	"pganji/db"
	"pganji/outbox"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL environment variable is not configured")
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 1. Connect Database Connection Pool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Error("unable to initialize database connection pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		slog.Error("database ping failed", "error", err)
		os.Exit(1)
	}
	slog.Info("successfully connected to PostgreSQL pool")

	// 2. Run Embedded Migrations using the active pool
	subFS, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		slog.Error("failed to create migration sub filesystem", "error", err)
		os.Exit(1)
	}

	if err := db.RunMigrationsWithPool(subFS, pool); err != nil {
		slog.Error("failed to run database migrations", "error", err)
		os.Exit(1)
	}

	// 3. Start Transactional Outbox Background Worker
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()

	outboxWorker := outbox.NewWorker(pool, &outbox.LogPublisher{}, 500*time.Millisecond)
	go outboxWorker.Start(workerCtx)

	// 4. Initialize HTTP Server
	srv := &api.Server{Pool: pool}
	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%s", port),
		Handler:      srv.Routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("pganji api server listening", "port", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("initiating graceful shutdown...")
	workerCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("forced shutdown triggered", "error", err)
	}
	slog.Info("server shutdown complete")
}