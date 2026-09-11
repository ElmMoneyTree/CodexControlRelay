package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"codex-control/relay-go/internal/config"
	relayserver "codex-control/relay-go/internal/server"
	"codex-control/relay-go/internal/store"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("codex-relay", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to YAML config (default: ./relay.yaml or CODEX_RELAY_CONFIG)")
	checkConfig := flags.Bool("check-config", false, "validate configuration and exit")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Printf("codex-relay %s (%s, %s)\n", version, commit, date)
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := newLogger(cfg.Logging)
	if *checkConfig {
		logger.Info("configuration is valid", "config", cfg.Path, "listen", cfg.Server.Listen, "database", cfg.Database.Path, "tls", cfg.Server.TLS.CertFile != "")
		return 0
	}

	database, err := store.Open(cfg.Database.Path, cfg.Database.BusyTimeout.Duration)
	if err != nil {
		logger.Error("open database", "error", err)
		return 1
	}
	defer database.Close()
	handler, err := relayserver.New(cfg, database, logger)
	if err != nil {
		logger.Error("create relay server", "error", err)
		return 1
	}

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration,
		WriteTimeout:      cfg.Server.WriteTimeout.Duration,
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,
		MaxHeaderBytes:    32 * 1024,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverError := make(chan error, 1)
	go func() {
		logger.Info("relay started", "version", version, "listen", cfg.Server.Listen, "public_url", cfg.Server.PublicURL, "database", cfg.Database.Path, "tls", cfg.Server.TLS.CertFile != "")
		if cfg.Server.TLS.CertFile != "" {
			serverError <- httpServer.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			serverError <- httpServer.ListenAndServe()
		}
	}()

	select {
	case err := <-serverError:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("relay stopped unexpectedly", "error", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		_ = httpServer.Close()
		return 1
	}
	logger.Info("relay stopped")
	return 0
}

func newLogger(cfg config.LoggingConfig) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.Format, "text") {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}
