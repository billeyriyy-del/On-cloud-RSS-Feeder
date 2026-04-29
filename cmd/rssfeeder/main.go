package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rssfeeder/internal/api"
	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/storage"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := loadConfig()

	store, err := storage.New(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer store.Close()

	slog.Info("database ready", "path", cfg.DBPath)

	apiSrv := api.New(api.Config{
		JWTSecret: cfg.JWTSecret,
		Password:  cfg.Password,
	}, store)

	scheduler := fetcher.NewScheduler(store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start background scheduler.
	go func() {
		slog.Info("scheduler started")
		scheduler.Run(ctx)
	}()

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.Port),
		Handler:      apiSrv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		srv.Shutdown(shutCtx)
	}()

	slog.Info("server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

type config struct {
	DBPath    string
	JWTSecret string
	Password  string
	Port      string
}

func loadConfig() config {
	cfg := config{
		DBPath:    getenv("RSS_DB_PATH", "./rssfeeder.db"),
		JWTSecret: getenv("RSS_JWT_SECRET", ""),
		Password:  getenv("RSS_PASSWORD", ""),
		Port:      getenv("RSS_PORT", "8080"),
	}
	if cfg.JWTSecret == "" {
		slog.Error("RSS_JWT_SECRET is required")
		os.Exit(1)
	}
	if cfg.Password == "" {
		slog.Error("RSS_PASSWORD is required")
		os.Exit(1)
	}
	return cfg
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
