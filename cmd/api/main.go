package main

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/componentruntime"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	amocrmclient "github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	oauthflow "github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/oauthlimit"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/logging"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
	"golang.org/x/time/rate"
)

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
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
	componentConfig, err := componentruntime.Load("api")
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	components, err := componentruntime.StartAPI(ctx, componentConfig, pool, registry)
	if err != nil {
		return err
	}
	defer components.Close()
	go func() {
		select {
		case <-components.Failed():
			stop()
		case <-ctx.Done():
		}
	}()

	installationStore := installations.NewStore(pool)
	webhookStore := webhook.NewStore(pool)
	jobStore := jobs.NewStore(pool)
	webhookIngressMetrics := webhook.NewIngressMetrics(registry)
	webhookIngressLimiter, err := webhook.NewIngressLimiter(webhook.IngressLimiterConfig{
		GlobalRate:        rate.Limit(cfg.WebhookGlobalRate),
		GlobalBurst:       cfg.WebhookGlobalBurst,
		InstallationRate:  rate.Limit(cfg.WebhookInstallationRate),
		InstallationBurst: cfg.WebhookInstallationBurst,
		InactiveTTL:       cfg.WebhookLimiterInactiveTTL,
	}, webhookIngressMetrics)
	if err != nil {
		return err
	}
	limiterContext, stopLimiter := context.WithCancel(ctx)
	limiterDone := make(chan struct{})
	go func() {
		defer close(limiterDone)
		webhookIngressLimiter.Run(limiterContext)
	}()
	defer func() {
		stopLimiter()
		<-limiterDone
	}()
	webhookHandler := webhook.NewHandler(
		installationStore,
		webhookStore,
		logger,
		cfg.MaxWebhookBody,
		cfg.WebhookTimeout,
		webhookIngressLimiter,
	)

	externalHTTPClient := &http.Client{
		Timeout: cfg.ExternalRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var oauthGateway oauthflow.OAuthGateway = oauthflow.NewGateway(amocrmclient.NewOAuthClient(externalHTTPClient))
	if components.AccountReader != nil {
		oauthGateway = oauthflow.WithAccountReader(oauthGateway, components.AccountReader)
	}
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
	oauthLimiter, err := oauthlimit.New(oauthlimit.Config{
		IPRate: cfg.OAuthIPRate, IPBurst: cfg.OAuthIPBurst,
		IdentityRate: cfg.OAuthIdentityRate, IdentityBurst: cfg.OAuthIdentityBurst,
		InactiveTTL: cfg.OAuthLimiterInactiveTTL, MaxEntries: cfg.OAuthLimiterMaxEntries,
	}, registry)
	if err != nil {
		return err
	}
	oauthLimitContext, stopOAuthLimiter := context.WithCancel(ctx)
	oauthLimitDone := make(chan struct{})
	go func() { defer close(oauthLimitDone); oauthLimiter.Run(oauthLimitContext) }()
	defer func() { stopOAuthLimiter(); <-oauthLimitDone }()

	widgetAuthenticator, err := widgetauth.NewAuthenticator(
		widgetauth.NewStore(pool), keyRing,
		widgetauth.WithLeeway(cfg.WidgetJWTLeeway),
		widgetauth.WithMaxLifetime(cfg.WidgetJWTMaxLifetime),
		widgetauth.WithLogger(logger),
	)
	if err != nil {
		return err
	}
	widgetLimiter, err := widgetlimit.New(widgetlimit.Config{
		IntegrationRate: cfg.WidgetIntegrationRate, IntegrationBurst: cfg.WidgetIntegrationBurst,
		InstallationRate: cfg.WidgetInstallationRate, InstallationBurst: cfg.WidgetInstallationBurst,
		InactiveTTL: cfg.WidgetLimiterInactiveTTL, MaxEntries: cfg.WidgetLimiterMaxEntries,
	}, registry)
	if err != nil {
		return err
	}
	widgetLimitContext, stopWidgetLimiter := context.WithCancel(ctx)
	widgetLimitDone := make(chan struct{})
	go func() { defer close(widgetLimitDone); widgetLimiter.Run(widgetLimitContext) }()
	defer func() { stopWidgetLimiter(); <-widgetLimitDone }()
	widgetActionStore := widgetapi.NewActionStore(pool, jobStore)
	widgetHandler := widgetapi.NewHandler(jobStore, widgetActionStore)
	leadStatusModule := leadstatus.NewModule(pool, jobStore)
	leadStatusModule.RegisterResults(widgetHandler)

	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(httpmiddleware.Recover(logger))
	router.Use(httpmiddleware.AccessLog(logger))
	httpserver.RegisterPublicSystemRoutes(router)
	router.Method(apicontract.OAuthStart.Method, apicontract.OAuthStart.Path, oauthLimiter.Middleware(http.HandlerFunc(oauthHandler.Start)))
	router.Method(apicontract.OAuthCallback.Method, apicontract.OAuthCallback.Path, oauthLimiter.Middleware(http.HandlerFunc(oauthHandler.Callback)))
	router.Method(apicontract.WebhookReceive.Method, apicontract.WebhookReceive.Path, http.HandlerFunc(webhookHandler.Receive))
	widgetCORSMiddleware := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	widgetRoute := func(consume bool, handler http.Handler) http.Handler {
		return protectWidgetRoute(widgetAuthenticator, widgetLimiter, widgetCORSMiddleware, consume, handler)
	}
	if components.Bridge != nil {
		components.Bridge.RegisterHTTP(router, func(handler http.Handler) http.Handler { return widgetRoute(true, handler) }, func(handler http.Handler) http.Handler { return widgetRoute(false, handler) })
	}
	router.Method(apicontract.WidgetBootstrap.Method, apicontract.WidgetBootstrap.Path,
		widgetRoute(true, http.HandlerFunc(widgetHandler.Bootstrap)))
	router.Method(apicontract.WidgetPing.Method, apicontract.WidgetPing.Path,
		widgetRoute(false, http.HandlerFunc(widgetHandler.Ping)))
	leadStatusModule.RegisterHTTP(router, func(handler http.Handler) http.Handler {
		return widgetRoute(false, handler)
	})
	router.Method(apicontract.WidgetJob.Method, apicontract.WidgetJob.Path,
		widgetRoute(true, http.HandlerFunc(widgetHandler.JobStatus)))
	for _, widgetRouteContract := range []apicontract.Route{
		apicontract.WidgetBootstrap,
		apicontract.WidgetPing,
		apicontract.WidgetJob,
	} {
		router.Method(http.MethodOptions, widgetRouteContract.Path,
			widgetCORSMiddleware(http.NotFoundHandler()))
	}
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})

	managementRouter := chi.NewRouter()
	managementRouter.Use(httpmiddleware.RequestID)
	managementRouter.Use(httpmiddleware.Recover(logger))
	managementRouter.Use(httpmiddleware.AccessLog(logger))
	httpserver.RegisterManagementRoutes(
		managementRouter,
		pool,
		cfg.DatabaseTimeout,
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	)
	if components.Bridge != nil {
		// Product readiness must not withdraw unrelated Core widget traffic.
		managementRouter.Get("/components/activity/ready", components.Readiness)
		managementRouter.Get("/components", components.Catalog)
	}

	publicServer := httpserver.New(cfg.HTTPAddress, router)
	managementServer := httpserver.New(cfg.ManagementHTTPAddress, managementRouter)
	if err := httpserver.RunAll(ctx, logger, cfg.ShutdownTimeout, publicServer, managementServer); err != nil {
		return err
	}
	return components.Err()
}
