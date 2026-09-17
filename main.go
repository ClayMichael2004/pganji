package main

import (
	"context"
	"log"
	"os"
	"time"
	"pganji/db"
	"pganji/api"
	"log/slog"//slog allows printing in structured format(JSON)
	"net/http"
	"os/signal"
	"time"
	"syscall"
)

func main(){
	//use structured JSON logging
	logger:= slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.Setdefault(logger)
	connStr:= os.Getenv("DATABASE_URL")
	if connStr==""{
		//fallback default for local dev
		connStr="postgres://localhost:5432/pganji?sslmode=disable"
	}

	//create a context with a 5 second timeout for the initial connection
	ctx, cancel:= context.WithTimeout(context.Background(), 10*time.Second)
	// Without defer cancel(): If your database connection succeeds in 5 milliseconds, the background timer keeps running in Go's memory for the full duration (e.g., 3 seconds). Under heavy traffic, thousands of these zombie timers will pile up, causing a major memory leak.With defer cancel(): The moment your function finishes connecting and returns the pool, the context is immediately destroyed, instantly freeing up system memory.
	defer cancel()

	pool, err:= db.ConnectDB(ctx, connStr)
	if err!=nil{
		log.Fatalf("Fatal Error: Could not connect to db; %v\n", err)
	}
	defer pool.Close()

	//initialize API Server
	server:= &api.Server{Pool: pool}
	httpServer:= &http.Server{
		Addr: ":8080",
		Handler: server.Routes(),
		ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	//gracefull shutdown time
	shutdownChan:= make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	go func(){
		slog.Info("Pganji api server listening on port 8080 ...")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed){
			slog.Error("HTTP server error", "error", err)
		}
	}()

	//wait for shutdown signal which in this case is control c
	<-shutdownChan
	slog.Info("shutting down Pganji server...")
	shutdownCtx, shutdownCancel:= context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err:= httpServer.Shutdown(shutdownCtx); err != nil{
		slog.Error("Graceful shutdown failed", "error", err)
	}

	slog.Info("pganji server stopped cleanly")
}