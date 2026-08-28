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
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/herotask"
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
	if err := config.ValidatePublicSecurity(cfg); err != nil {
		logger.Error("public HTTP security startup failed", "error", err)
		os.Exit(1)
	}
	secretKey, err := config.ResolveSecretKey(cfg)
	if err != nil {
		logger.Error("secret key startup failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	box, err := secure.New(secretKey)
	if err != nil {
		logger.Error("secret storage startup failed", "error", err)
		os.Exit(1)
	}
	store, err := storage.OpenPostgres(ctx, storage.PostgresConfig{
		URL: cfg.DatabaseURL, Protector: box, MaxConns: 20, MinConns: 1,
	})
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	settingsManager := appsettings.New(store, box, cfg.SMSBaseURL, cfg.HeroSMSBaseURL)
	heroTaskManager := herotask.New(store, &settingsHeroSMSClient{settings: settingsManager}, herotask.Config{}, logger)
	workflowManager := workflow.New(store, settingsManager, box, workflow.Config{
		PollInterval: cfg.PollInterval, ActivationTTL: cfg.ActivationTTL,
		LoginStatusTTL: cfg.LoginStatusTTL,
		SSOBaseURL:     cfg.GoPaySSOBaseURL, GoPayBaseURL: cfg.GoPayBaseURL,
	}, logger)
	if err = workflowManager.Run(ctx); err != nil {
		logger.Error("workflow startup cleanup failed", "error", err)
		os.Exit(1)
	}
	if err = heroTaskManager.Run(ctx); err != nil {
		logger.Error("HeroSMS number task startup failed", "error", err)
		os.Exit(1)
	}
	webhookReceiver := heroSMSWebhookFanout{legacy: workflowManager, numberTasks: heroTaskManager}

	httpServer := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewRouterWithConfig(store, settingsManager, workflowManager, webui.Handler(), api.RouterConfig{
			AuthUsername: cfg.AuthUsername, AuthPassword: cfg.AuthPassword,
			HeroSMSWebhookToken: cfg.HeroSMSWebhookToken, PublicURL: cfg.PublicURL,
			HeroSMSWebhookReceiver: webhookReceiver,
			HeroSMSTasks:           api.AdaptHeroSMSTaskManager(heroTaskManager),
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
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
	heroTaskManager.Wait()
}

// settingsHeroSMSClient reloads the encrypted provider setting for every
// provider operation, so updating the API key in the UI takes effect without
// restarting long-lived number tasks.
type settingsHeroSMSClient struct {
	settings *appsettings.Manager
}

func (client *settingsHeroSMSClient) provider(ctx context.Context) (*herosms.Client, error) {
	value, err := client.settings.GetHeroSMS(ctx)
	if err != nil {
		return nil, err
	}
	return herosms.NewClient(herosms.Config{APIKey: value.APIKey, BaseURL: value.BaseURL})
}

func (client *settingsHeroSMSClient) PurchaseOne(ctx context.Context, request herosms.PurchaseRequest) (herosms.Purchase, error) {
	provider, err := client.provider(ctx)
	if err != nil {
		return herosms.Purchase{}, err
	}
	return provider.PurchaseOne(ctx, request)
}

func (client *settingsHeroSMSClient) GetMessages(ctx context.Context, activationID string, rent bool) ([]herosms.Message, error) {
	provider, err := client.provider(ctx)
	if err != nil {
		return nil, err
	}
	return provider.GetMessages(ctx, activationID, rent)
}

func (client *settingsHeroSMSClient) RequestAnother(ctx context.Context, activationID string) error {
	provider, err := client.provider(ctx)
	if err != nil {
		return err
	}
	return provider.RequestAnother(ctx, activationID)
}

func (client *settingsHeroSMSClient) Finish(ctx context.Context, activationID string, rent bool) error {
	provider, err := client.provider(ctx)
	if err != nil {
		return err
	}
	return provider.Finish(ctx, activationID, rent)
}

func (client *settingsHeroSMSClient) Cancel(ctx context.Context, activationID string, rent bool) error {
	provider, err := client.provider(ctx)
	if err != nil {
		return err
	}
	return provider.Cancel(ctx, activationID, rent)
}
