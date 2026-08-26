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

	"github.com/mangobubu/gopay-autosms/internal/api"
	"github.com/mangobubu/gopay-autosms/internal/config"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/webui"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg := config.Load()
	secretKey, err := config.ResolveSecretKey(cfg)
	if err != nil {
		logger.Error("secret key startup failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.OpenPostgres(ctx, storage.PostgresConfig{URL: cfg.DatabaseURL, MaxConns: 20, MinConns: 1})
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	box, err := secure.New(secretKey)
	if err != nil {
		logger.Error("secret storage startup failed", "error", err)
		os.Exit(1)
	}
	settingsManager := appsettings.New(store, box, cfg.SMSBaseURL)
	workflowManager := workflow.New(store, settingsManager, box, workflow.Config{
		PollInterval: cfg.PollInterval, ActivationTTL: cfg.ActivationTTL,
		LoginStatusTTL: cfg.LoginStatusTTL,
		SSOBaseURL:     cfg.GoPaySSOBaseURL, GoPayBaseURL: cfg.GoPayBaseURL,
	}, logger)
	workflowManager.Run(ctx)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(store, settingsManager, workflowManager, webui.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("AutoSMS web service started", "address", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if err != nil {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
		}
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP shutdown incomplete", "error", err)
	}
	workflowManager.Wait()
}
