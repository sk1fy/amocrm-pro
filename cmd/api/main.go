package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	amocrmclient "github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	oauthflow "github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/logging"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.ServiceName, cfg.Environment, cfg.LogLevel)
	slog.SetDefault(logger)
	keyRing, err := cryptox.ParseKeyRing(cfg.EncryptionKeys, cfg.EncryptionKeyVersion)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	databaseContext, cancel := context.WithTimeout(ctx, cfg.DatabaseTimeout)
	pool, err := postgres.Open(databaseContext, cfg.DatabaseURL, cfg.ServiceName, cfg.DBMaxConns)
	cancel()
	if err != nil {
		return err
	}
	defer pool.Close()

	installationStore := installations.NewStore(pool)
	webhookStore := webhook.NewStore(pool)
	jobStore := jobs.NewStore(pool)
	webhookHandler := webhook.NewHandler(
		installationStore,
		webhookStore,
		logger,
		cfg.MaxWebhookBody,
		cfg.WebhookTimeout,
	)

	externalHTTPClient := &http.Client{
		Timeout: cfg.ExternalRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	oauthGateway := oauthflow.NewGateway(amocrmclient.NewOAuthClient(externalHTTPClient))
	oauthStore := oauthflow.NewStore(pool, keyRing)
	if bootstrap := cfg.BootstrapIntegration; bootstrap != nil {
		bootstrapContext, bootstrapCancel := context.WithTimeout(ctx, cfg.DatabaseTimeout)
		integration, bootstrapErr := oauthStore.EnsureIntegration(bootstrapContext, oauthflow.IntegrationInput{
			Code: bootstrap.Code, ClientID: bootstrap.ClientID,
			ClientSecret: bootstrap.ClientSecret, RedirectURI: bootstrap.RedirectURI,
			WebhookEvents: bootstrap.WebhookEvents,
		})
		bootstrapCancel()
		if bootstrapErr != nil {
			return bootstrapErr
		}
		logger.Info("integration bootstrap applied", "integration_id", integration.ID, "integration_code", integration.Code)
	}
	oauthService := oauthflow.NewService(
		oauthStore, keyRing, oauthGateway, cfg.OAuthStateTTL, cfg.ExternalRequestTimeout,
	)
	oauthHandler := oauthflow.NewHandler(oauthService, logger)

	widgetAuthenticator, err := widgetauth.NewAuthenticator(
		widgetauth.NewStore(pool), keyRing, widgetauth.WithLeeway(cfg.WidgetJWTLeeway),
	)
	if err != nil {
		return err
	}
	widgetHandler := widgetapi.NewHandler(jobStore)

	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(httpmiddleware.Recover(logger))
	router.Use(httpmiddleware.AccessLog(logger))
	router.Get("/live", httpserver.Live)
	router.Get("/ready", httpserver.Ready(pool, cfg.DatabaseTimeout))
	router.Handle("/metrics", promhttp.Handler())
	router.Get("/oauth/amocrm/start", oauthHandler.Start)
	router.Get("/oauth/amocrm/callback", oauthHandler.Callback)
	router.Post("/hooks/amocrm/v1/{webhookKey}", webhookHandler.Receive)
	router.Route("/api/v1/widget", func(widgetRouter chi.Router) {
		widgetRouter.Use(widgetauth.Middleware(widgetAuthenticator))
		widgetRouter.Get("/bootstrap", widgetHandler.Bootstrap)
		widgetRouter.Post("/actions/ping", widgetHandler.Ping)
		widgetRouter.Get("/jobs/{jobID}", widgetHandler.JobStatus)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})

	server := httpserver.New(cfg.HTTPAddress, router)
	if err := httpserver.Run(ctx, server, logger, cfg.ShutdownTimeout); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
