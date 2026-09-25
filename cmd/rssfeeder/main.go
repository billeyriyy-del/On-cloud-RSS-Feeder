package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // RSS_TZ works even on minimal images without zoneinfo

	"rssfeeder/internal/api"
	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/storage"
	"rssfeeder/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cfg := loadConfig()

	store, err := storage.New(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer store.Close()
	v, _ := store.SchemaVersion(context.Background())
	slog.Info("database ready", "path", cfg.DBPath, "schema", v)

	sched := fetcher.NewScheduler(store, fetcher.Options{
		Retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		PublicURL: cfg.PublicURL,
		BackupDir: cfg.BackupDir,
	})
	var static http.Handler
	if cfg.WebUI {
		static = web.Handler()
	}
	apiSrv := api.New(api.Config{
		JWTSecret: cfg.JWTSecret,
		Password:  cfg.Password,
		Location:  cfg.Location,
		Static:    static,
	}, store, sched)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		slog.Info("scheduler started", "retention_days", cfg.RetentionDays, "websub", cfg.PublicURL != "")
		sched.Run(ctx)
	}()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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
	DBPath        string
	JWTSecret     string
	Password      string
	Port          string
	PublicURL     string
	BackupDir     string
	RetentionDays int
	Location      *time.Location
	WebUI         bool
}

func loadConfig() config {
	cfg := config{
		DBPath:    getenv("RSS_DB_PATH", "./rssfeeder.db"),
		JWTSecret: getenv("RSS_JWT_SECRET", ""),
		Password:  getenv("RSS_PASSWORD", ""),
		Port:      getenv("RSS_PORT", "8080"),
		PublicURL: getenv("RSS_PUBLIC_URL", ""),
		BackupDir: getenv("RSS_BACKUP_DIR", ""),
		WebUI:     getenv("RSS_WEB_UI", "1") != "0",
		Location:  time.Local,
	}
	if cfg.JWTSecret == "" {
		slog.Error("RSS_JWT_SECRET is required")
		os.Exit(1)
	}
	if len(cfg.JWTSecret) < 32 {
		slog.Warn("RSS_JWT_SECRET is shorter than 32 characters; use `openssl rand -hex 32`")
	}
	if cfg.Password == "" {
		slog.Error("RSS_PASSWORD is required")
		os.Exit(1)
	}
	days, err := strconv.Atoi(getenv("RSS_RETENTION_DAYS", "90"))
	if err != nil || days < 7 {
		slog.Error("RSS_RETENTION_DAYS must be an integer ≥ 7")
		os.Exit(1)
	}
	cfg.RetentionDays = days
	if tz := os.Getenv("RSS_TZ"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			slog.Error("RSS_TZ is not a valid IANA time zone", "tz", tz)
			os.Exit(1)
		}
		cfg.Location = loc
	}
	return cfg
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
