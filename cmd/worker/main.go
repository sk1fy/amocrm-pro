package main

import (
	"context"
	"errors"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/componentruntime"
	amocrmclient "github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/maintenance"
	oauthflow "github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/logging"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	if err := run(); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.ServiceName, cfg.Environment, cfg.LogLevel)
	slog.SetDefault(logger)
	keyRing, err := cryptox.ParseKeyRing(cfg.EncryptionKeys, cfg.EncryptionKeyVersion)
	if err != nil {
		return err
	}

	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancelAll := context.WithCancel(signalContext)
	defer cancelAll()

	databaseContext, cancel := context.WithTimeout(ctx, cfg.DatabaseTimeout)
	pool, err := postgres.Open(databaseContext, cfg.DatabaseURL, cfg.ServiceName, cfg.DBMaxConns)
	cancel()
	if err != nil {
		return err
	}
	defer pool.Close()

	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	cleanupMetrics := maintenance.NewMetrics(registry)
	jobMetrics := jobs.NewMetrics(registry)
	jobBacklogCollector, err := jobs.NewBacklogCollector(pool, cfg.DatabaseTimeout)
	if err != nil {
		return err
	}
	registry.MustRegister(jobBacklogCollector)
	serviceBacklogCollector, err := jobs.NewServiceBacklogCollector(pool, cfg.DatabaseTimeout)
	if err != nil {
		return err
	}
	registry.MustRegister(serviceBacklogCollector)
	webhookMetrics := webhook.NewMetrics(registry)
	webhookStore := webhook.NewStore(pool, webhookMetrics)
	jobStore := jobs.NewStore(pool)
	externalHTTPClient := &http.Client{
		Timeout: cfg.ExternalRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	oauthGateway := oauthflow.NewGateway(amocrmclient.NewOAuthClient(externalHTTPClient))
	tokenProvider := oauthflow.NewTokenProvider(pool, keyRing, oauthGateway)
	amocrmAPI := amocrmclient.NewClient(externalHTTPClient, tokenProvider)
	amocrmAPI.SetMetrics(amocrmclient.NewMetrics(registry))
	componentConfig, err := componentruntime.Load("worker")
	if err != nil {
		return err
	}
	components, err := componentruntime.StartGateway(ctx, componentConfig, pool, amocrmAPI, registry)
	if err != nil {
		return err
	}
	defer components.Close()
	go func() {
		select {
		case <-components.Failed():
			cancelAll()
		case <-ctx.Done():
		}
	}()
	widgetExecutionStore := widgetapi.NewExecutionStore(pool)
	leadStatusModule := leadstatus.NewModule(pool, jobStore)
	leadStatusModule.RegisterEvents(webhookStore)
	reconcileHandler, err := webhook.ReconcileJobHandler(
		webhook.NewReconcileStore(pool, keyRing), amocrmAPI, cfg.PublicBaseURL,
	)
	if err != nil {
		return err
	}
	handlers := map[string]jobs.Handler{
		"webhook.parse":         webhook.ParseJobHandler(webhookStore),
		"webhook.process_event": webhook.ProcessEventJobHandler(webhookStore),
		"webhook.reconcile":     reconcileHandler,
		widgetapi.PingJobType:   widgetapi.PingJobHandler(widgetExecutionStore),
	}
	observers := map[string]jobs.FailureObserver{
		"webhook.parse":         webhook.JobFailureObserver(webhookStore),
		"webhook.process_event": webhook.JobFailureObserver(webhookStore),
	}
	leadStatusModule.RegisterJobs(handlers, observers, amocrmAPI)

	worker := jobs.NewWorker(jobStore, logger, jobs.WorkerConfig{
		ID:                     cfg.WorkerID,
		PollInterval:           cfg.PollInterval,
		LeaseDuration:          cfg.LeaseDuration,
		JobTimeout:             cfg.JobTimeout,
		BatchSize:              cfg.BatchSize,
		ReapBatchSize:          cfg.ReapBatchSize,
		Concurrency:            cfg.Concurrency,
		IntegrationConcurrency: cfg.IntegrationConcurrency,
		DrainTimeout:           cfg.ShutdownTimeout,
		ClaimTimeout:           cfg.DatabaseTimeout,
	}, handlers, observers)
	worker.SetMetrics(jobMetrics)
	cleanupScheduler, err := maintenance.NewScheduler(
		maintenance.NewStore(pool), logger, maintenance.SchedulerConfig{
			Interval: cfg.CleanupInterval,
			Timeout:  cfg.CleanupTimeout,
			Policy: maintenance.Policy{
				SafetyMargin:             cfg.CleanupSafetyMargin,
				WebhookInboxRetention:    cfg.WebhookInboxRetention,
				WebhookDeliveryRetention: cfg.WebhookDeliveryRetention,
				BatchSize:                cfg.CleanupBatchSize,
				MaxBatches:               cfg.CleanupMaxBatches,
			},
		}, cleanupMetrics,
	)
	if err != nil {
		return err
	}

	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(httpmiddleware.Recover(logger))
	router.Use(httpmiddleware.AccessLog(logger))
	router.Get("/live", httpserver.Live)
	router.Get("/ready", httpserver.Ready(pool, cfg.DatabaseTimeout))
	if componentConfig.Mode != "off" {
		router.Get("/components", components.Catalog)
	}
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	healthServer := httpserver.New(cfg.HTTPAddress, router)

	errChannel := make(chan error, 3)
	var processes sync.WaitGroup
	processes.Add(3)
	go func() {
		defer processes.Done()
		errChannel <- httpserver.Run(ctx, healthServer, logger, cfg.ShutdownTimeout)
	}()
	go func() {
		defer processes.Done()
		errChannel <- worker.Run(ctx)
	}()
	go func() {
		defer processes.Done()
		errChannel <- cleanupScheduler.Run(ctx)
	}()

	var runError error
	select {
	case <-signalContext.Done():
	case <-components.Failed():
		runError = components.Err()
	case runError = <-errChannel:
	}
	cancelAll()
	processes.Wait()
	if runError != nil && !errors.Is(runError, context.Canceled) {
		return runError
	}
	return nil
}
