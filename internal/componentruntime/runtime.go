package componentruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	oauthflow "github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
	"google.golang.org/grpc"
)

type Graph struct {
	Bridge        *activitybridge.Bridge
	AccountReader oauthflow.AccountReader
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	closers       []func()
	components    *services.Registry
	failed        chan struct{}
	mu            sync.Mutex
	err           error
}

func newGraph(ctx context.Context) *Graph {
	c, cancel := context.WithCancel(ctx)
	return &Graph{ctx: c, cancel: cancel, failed: make(chan struct{}), components: services.NewRegistry()}
}
func (g *Graph) goRun(fn func(context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		err := fn(g.ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, grpc.ErrServerStopped) {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				close(g.failed)
			}
			g.mu.Unlock()
			g.cancel()
		}
	}()
}
func (g *Graph) Failed() <-chan struct{} { return g.failed }
func (g *Graph) Err() error              { g.mu.Lock(); defer g.mu.Unlock(); return g.err }
func (g *Graph) Close() {
	g.cancel()
	g.wg.Wait()
	for i := len(g.closers) - 1; i >= 0; i-- {
		g.closers[i]()
	}
}

func (g *Graph) dial(address, identity string) (*servicerpc.Clients, error) {
	return g.connectClient(address, identity, true)
}
func (g *Graph) connect(address, identity string) (*servicerpc.Clients, error) {
	return g.connectClient(address, identity, false)
}
func (g *Graph) connectClient(address, identity string, wait bool) (*servicerpc.Clients, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid service address")
	}
	tls, err := servicerpc.TLSFromFiles(filepath.Join(identity, "ca.crt"), filepath.Join(identity, "tls.crt"), filepath.Join(identity, "tls.key"), host, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	var client *servicerpc.Clients
	if wait {
		client, err = servicerpc.Dial(ctx, address, tls)
	} else {
		client, err = servicerpc.Connect(address, tls)
	}
	if err != nil {
		return nil, err
	}
	g.closers = append(g.closers, func() { _ = client.Close() })
	return client, nil
}

// StartAPI composes product ports. Core grpc deployment has no product DSNs.
func StartAPI(ctx context.Context, c Config, core *pgxpool.Pool, reg prometheus.Registerer) (*Graph, error) {
	g := newGraph(ctx)
	if c.Mode == "off" {
		_ = g.components.Seal()
		return g, nil
	}
	RegisterPoolMetrics(reg, core, "core")
	ok := false
	defer func() {
		if !ok {
			g.Close()
		}
	}()
	policyClient, err := g.connect(c.GatewayAddress, c.IdentityDir)
	if err != nil {
		return nil, err
	}
	if err := g.components.Register(serviceapi.GatewayService, policyClient.Gateway, remotePlacement(), policyClient.Ready); err != nil {
		return nil, err
	}
	g.AccountReader = func(ctx context.Context, integrationID uuid.UUID, domain, accessToken string) (oauthflow.Account, error) {
		a, err := policyClient.BootstrapAccount.GetAccount(ctx, serviceapi.BootstrapAccountRequest{IntegrationID: integrationID, AccountDomain: domain, AccessToken: accessToken})
		return oauthflow.Account{ID: a.ID, Subdomain: a.Subdomain}, err
	}
	var product serviceapi.Activity
	var events serviceapi.CRMEvents
	if c.Mode == "grpc" {
		a, err := g.connect(c.ActivityAddress, c.IdentityDir)
		if err != nil {
			return nil, err
		}
		product = a.Activity
		if err := g.components.Register(serviceapi.ActivityService, product, remotePlacement(), a.Ready); err != nil {
			return nil, err
		}
		e, err := g.connect(c.EventsAddress, c.IdentityDir)
		if err != nil {
			return nil, err
		}
		events = e.CRMEvents
		if err := g.components.Register(serviceapi.EventsService, events, remotePlacement(), e.Ready); err != nil {
			return nil, err
		}
	} else {
		eClient, err := g.connect(c.GatewayAddress, c.Identity(serviceapi.EventsService))
		if err != nil {
			return nil, err
		}
		ePool, err := openOwned(g.ctx, c.EventsDSN, serviceapi.EventsService, c.EventsPool, reg)
		if err != nil {
			return nil, err
		}
		g.closers = append(g.closers, ePool.Close)
		eCfg := crmevents.DefaultConfig()
		eCfg.Workers = c.EventsWorkers
		eCfg.PollInterval = c.PollInterval
		eCfg.DisableEnrichment = c.DisableEnrichment
		collector := crmevents.New(ePool, eClient.Policy, eClient.Gateway, eCfg)
		if reg != nil {
			reg.MustRegister(collector.Collector())
		}
		events = collector
		if err := g.components.Register(serviceapi.EventsService, events, ownedPlacement("embedded", c.EventsPool, c.EventsWorkers), readyAll(ePool.Ping, eClient.Ready)); err != nil {
			return nil, err
		}
		aClient, err := g.connect(c.GatewayAddress, c.Identity(serviceapi.ActivityService))
		if err != nil {
			return nil, err
		}
		aPool, err := openOwned(g.ctx, c.ActivityDSN, serviceapi.ActivityService, c.ActivityPool, reg)
		if err != nil {
			return nil, err
		}
		g.closers = append(g.closers, aPool.Close)
		product = activity.New(activity.NewPostgres(aPool), aClient.Policy, g.components.Events(), aClient.Gateway)
		if err := g.components.Register(serviceapi.ActivityService, product, ownedPlacement("embedded", c.ActivityPool, 0), readyAll(aPool.Ping, aClient.Ready)); err != nil {
			return nil, err
		}
		g.goRun(collector.Run)
	}
	if err := g.components.Seal(); err != nil {
		return nil, err
	}
	g.Bridge = activitybridge.New(core, policyClient.Policy, g.components.Activity(), g.components.Events())
	if reg != nil {
		reg.MustRegister(g.Bridge.Collector())
	}
	g.goRun(g.Bridge.RunDelivery)
	ok = true
	return g, nil
}

// StartGateway shares the worker's existing client and token provider, including
// the limiter used by leadstatus and webhook reconciliation.
func StartGateway(ctx context.Context, c Config, core *pgxpool.Pool, amo *amocrm.Client, reg prometheus.Registerer) (*Graph, error) {
	g := newGraph(ctx)
	if c.Mode == "off" {
		_ = g.components.Seal()
		return g, nil
	}
	RegisterPoolMetrics(reg, core, "core")
	ok := false
	defer func() {
		if !ok {
			g.Close()
		}
	}()
	data, err := os.ReadFile(filepath.Join(c.IdentityDir, "delegation.key"))
	if err != nil {
		return nil, fmt.Errorf("read Core delegation signing key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("Core delegation key must be PKCS8 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Core delegation key: %w", err)
	}
	key, valid := parsed.(ed25519.PrivateKey)
	if !valid {
		return nil, fmt.Errorf("Core delegation key must be Ed25519")
	}
	policy, err := corepolicy.New(core, amo, key)
	if err != nil {
		return nil, err
	}
	api := gateway.New(amo, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	if err := g.components.Register(serviceapi.GatewayService, api, ownedPlacement(c.Mode, core.Config().MaxConns, 0), core.Ping); err != nil {
		return nil, err
	}
	if err := g.components.Seal(); err != nil {
		return nil, err
	}
	if err := g.serve(c.RPCAddress, c.IdentityDir, &servicerpc.Endpoints{Policy: policy, Gateway: g.components.Gateway(), BootstrapAccount: gateway.NewBootstrap(core, amo), Ready: g.Ready, Metrics: reg}); err != nil {
		return nil, err
	}
	ok = true
	return g, nil
}

func (g *Graph) serve(address, identity string, endpoints *servicerpc.Endpoints) error {
	tls, err := servicerpc.TLSFromFiles(filepath.Join(identity, "ca.crt"), filepath.Join(identity, "tls.crt"), filepath.Join(identity, "tls.key"), "", true)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := servicerpc.NewServer(tls, endpoints)
	g.goRun(func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				stopped := make(chan struct{})
				go func() { server.GracefulStop(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					server.Stop()
				}
			case <-done:
			}
		}()
		err := server.Serve(listener)
		close(done)
		return err
	})
	return nil
}

func RunStandalone(ctx context.Context, role string) error {
	c, err := Load(role)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	g := newGraph(ctx)
	defer g.Close()
	client, err := g.dial(c.GatewayAddress, c.IdentityDir)
	if err != nil {
		return err
	}
	if err := g.components.Register(serviceapi.GatewayService, client.Gateway, remotePlacement(), client.Ready); err != nil {
		return err
	}
	var pool *pgxpool.Pool
	endpoints := &servicerpc.Endpoints{}
	endpoints.Metrics = reg
	switch role {
	case serviceapi.ActivityService:
		pool, err = openOwned(g.ctx, c.ActivityDSN, role, c.ActivityPool, reg)
		if err != nil {
			return err
		}
		g.closers = append(g.closers, pool.Close)
		events, err := g.dial(c.EventsAddress, c.IdentityDir)
		if err != nil {
			return err
		}
		if err := g.components.Register(serviceapi.EventsService, events.CRMEvents, remotePlacement(), events.Ready); err != nil {
			return err
		}
		product := activity.New(activity.NewPostgres(pool), client.Policy, g.components.Events(), g.components.Gateway())
		if err := g.components.Register(role, product, ownedPlacement("grpc", c.ActivityPool, 0), pool.Ping); err != nil {
			return err
		}
		endpoints.Activity = g.components.Activity()
	case serviceapi.EventsService:
		pool, err = openOwned(g.ctx, c.EventsDSN, role, c.EventsPool, reg)
		if err != nil {
			return err
		}
		g.closers = append(g.closers, pool.Close)
		cfg := crmevents.DefaultConfig()
		cfg.Workers = c.EventsWorkers
		cfg.PollInterval = c.PollInterval
		cfg.DisableEnrichment = c.DisableEnrichment
		events := crmevents.New(pool, client.Policy, g.components.Gateway(), cfg)
		reg.MustRegister(events.Collector())
		if err := g.components.Register(role, events, ownedPlacement("grpc", c.EventsPool, c.EventsWorkers), pool.Ping); err != nil {
			return err
		}
		endpoints.CRMEvents = g.components.Events()
		g.goRun(events.Run)
	default:
		return fmt.Errorf("unknown standalone service")
	}
	if err := g.components.Seal(); err != nil {
		return err
	}
	endpoints.Ready = g.Ready
	if err = g.serve(c.RPCAddress, c.IdentityDir, endpoints); err != nil {
		return err
	}
	router := http.NewServeMux()
	router.HandleFunc("GET /live", httpserver.Live)
	router.HandleFunc("GET /ready", g.Readiness)
	router.HandleFunc("GET /components", g.Catalog)
	router.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	g.goRun(func(ctx context.Context) error {
		return httpserver.Run(ctx, httpserver.New(c.HealthAddress, router), slog.Default(), 10*time.Second)
	})
	slog.Info("component started", "service", role, "mode", c.Mode, "rpc", c.RPCAddress, "health", c.HealthAddress)
	select {
	case <-ctx.Done():
		return nil
	case <-g.Failed():
		return g.Err()
	}
}

// Mode is logged without DSNs, tokens or certificates.
func Mode(c Config) string { return strings.ToLower(c.Mode) }

func (g *Graph) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := g.Err(); err != nil {
		return err
	}
	return g.components.Ready(ctx)
}
func remotePlacement() services.Placement { return services.Placement{Mode: "grpc"} }
func ownedPlacement(mode string, pool int32, workers int) services.Placement {
	return services.Placement{Mode: mode, OwnsDatabase: true, MaxConnections: int(pool), Workers: workers}
}
func readyAll(checks ...func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		for _, check := range checks {
			if err := check(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}
func (g *Graph) Catalog(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g.components.Snapshot())
}
func (g *Graph) Readiness(w http.ResponseWriter, r *http.Request) {
	if err := g.Ready(r.Context()); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
