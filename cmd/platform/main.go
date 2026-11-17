package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"github.com/sk1fy/amocrm-pro/internal/productplatform"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

const (
	startupTimeout   = 10 * time.Second
	readinessTimeout = time.Second
	shutdownTimeout  = 10 * time.Second
)

type config struct {
	accessDSN, jobsDSN      string
	accessMax, jobsMax      int32
	workers                 int
	public, private, manage string
	certificate, key, ca    string
}

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	if err := run(); err != nil {
		slog.Error("Platform stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	tlsConfig, err := productplatform.PrivateTLS(cfg.certificate, cfg.key, cfg.ca)
	if err != nil {
		return err
	}

	startup, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	access, err := storage.Open(startup, cfg.accessDSN, storage.Access, cfg.accessMax)
	if err != nil {
		return fmt.Errorf("open access pool: %w", err)
	}
	defer access.Close()
	if err := storage.CheckAccess(startup, access); err != nil {
		return fmt.Errorf("run Platform migrations and verify access role grants: %w", err)
	}
	jobs, err := storage.Open(startup, cfg.jobsDSN, storage.Jobs, cfg.jobsMax)
	if err != nil {
		return fmt.Errorf("open jobs pool: %w", err)
	}
	defer jobs.Close()
	cancel()

	checkAccess := func(ctx context.Context) error { return storage.CheckAccess(ctx, access) }
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	lifecycle := productplatform.NewLifecycleStore(jobs)
	worker := queue.NewWorker(queue.NewStore(jobs), map[queue.Kind]queue.Handler{productplatform.DrainKind: lifecycle.DrainJob}, queue.DefaultConfig(cfg.workers), registry)
	management := productplatform.HealthHandler(checkAccess, readinessTimeout)
	management.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	private := newServer(cfg.private, productplatform.PrivateHandler(productplatform.LifecycleHandler(lifecycle), nil))
	private.TLSConfig = tlsConfig

	return serve(ctx, worker,
		newServer(cfg.public, productplatform.HealthHandler(checkAccess, readinessTimeout)),
		private,
		newServer(cfg.manage, management),
	)
}

func newServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
}

func serve(ctx context.Context, worker queue.Worker, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, other := range listeners {
				_ = other.Close()
			}
			return fmt.Errorf("bind Platform listener: %w", err)
		}
		listeners = append(listeners, listener)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(servers))
	for i, server := range servers {
		listener := listeners[i]
		go func() {
			if server.TLSConfig != nil {
				errs <- server.ServeTLS(listener, "", "")
				return
			}
			errs <- server.Serve(listener)
		}()
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	slog.Info("Platform listening", "public", listeners[0].Addr(), "private", listeners[1].Addr(), "management", listeners[2].Addr())
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve Platform: %w", err)
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stop()
	var group sync.WaitGroup
	for _, server := range servers {
		group.Go(func() {
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
			}
		})
	}
	group.Wait()
	select {
	case <-workerDone:
	case <-shutdown.Done():
	}
	if shutdown.Err() != nil {
		return errors.Join(runErr, errors.New("Platform shutdown deadline exceeded"))
	}
	return runErr
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		accessDSN: getenv("PLATFORM_ACCESS_DATABASE_URL"), jobsDSN: getenv("PLATFORM_JOBS_DATABASE_URL"),
		certificate: getenv("PLATFORM_TLS_CERT_FILE"), key: getenv("PLATFORM_TLS_KEY_FILE"), ca: getenv("PLATFORM_TLS_CLIENT_CA_FILE"),
	}
	if cfg.accessDSN == "" || cfg.jobsDSN == "" {
		return config{}, errors.New("PLATFORM_ACCESS_DATABASE_URL and PLATFORM_JOBS_DATABASE_URL are required")
	}
	if cfg.certificate == "" || cfg.key == "" || cfg.ca == "" {
		return config{}, errors.New("PLATFORM_TLS_CERT_FILE, PLATFORM_TLS_KEY_FILE and PLATFORM_TLS_CLIENT_CA_FILE are required")
	}

	cfg.public = envDefault(getenv, "PLATFORM_PUBLIC_HTTP_ADDRESS", ":8080")
	cfg.private = envDefault(getenv, "PLATFORM_PRIVATE_HTTP_ADDRESS", ":8443")
	cfg.manage = envDefault(getenv, "PLATFORM_MANAGEMENT_HTTP_ADDRESS", "127.0.0.1:8081")
	accessMax, err := envInt(getenv, "PLATFORM_ACCESS_POOL_MAX_CONNS", "4")
	if err != nil {
		return config{}, err
	}
	jobsMax, err := envInt(getenv, "PLATFORM_JOBS_POOL_MAX_CONNS", "4")
	if err != nil {
		return config{}, err
	}
	workers, err := envInt(getenv, "PLATFORM_WORKERS", "2")
	if err != nil {
		return config{}, err
	}
	cfg.accessMax = int32(accessMax)
	cfg.jobsMax = int32(jobsMax)
	cfg.workers = workers
	return cfg, nil
}

func envDefault(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(getenv func(string) string, name, fallback string) (int, error) {
	value, err := strconv.Atoi(envDefault(getenv, name, fallback))
	if err != nil || value < 1 || value > 16 {
		return 0, fmt.Errorf("%s must be an integer from 1 to 16", name)
	}
	return value, nil
}
