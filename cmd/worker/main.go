package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amocrmclient "github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/maintenance"
	oauthflow "github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/logging"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

func main() {
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
	widgetExecutionStore := widgetapi.NewExecutionStore(pool)
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
		webhook.LeadStatusTransitionJobType: webhook.LeadStatusTransitionJobHandler(
			webhookStore, widgetExecutionStore, amocrmAPI,
		),
		widgetapi.PingJobType:          widgetapi.PingJobHandler(widgetExecutionStore),
		widgetapi.LeadSetStatusJobType: widgetapi.LeadSetStatusJobHandler(widgetExecutionStore, amocrmAPI),
	}
	worker := jobs.NewWorker(jobStore, logger, jobs.WorkerConfig{
		ID:            cfg.WorkerID,
		PollInterval:  cfg.PollInterval,
		LeaseDuration: cfg.LeaseDuration,
		JobTimeout:    cfg.JobTimeout,
		BatchSize:     cfg.BatchSize,
		Concurrency:   cfg.Concurrency,
		DrainTimeout:  cfg.ShutdownTimeout,
		ClaimTimeout:  cfg.DatabaseTimeout,
	}, handlers, map[string]jobs.FailureObserver{
		"webhook.parse":                     webhook.JobFailureObserver(webhookStore),
		"webhook.process_event":             webhook.JobFailureObserver(webhookStore),
		webhook.LeadStatusTransitionJobType: webhook.JobFailureObserver(webhookStore),
	})
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
	case runError = <-errChannel:
	}
	cancelAll()
	processes.Wait()
	if runError != nil && !errors.Is(runError, context.Canceled) {
		return runError
	}
	return nil
}
