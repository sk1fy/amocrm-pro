package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpserver"
)

func companyCRMServer(pool *pgxpool.Pool, client *amocrm.Client, tokens amocrm.TokenProvider, cfg companycrm.Config) (*http.Server, error) {
	if cfg.Address == "" {
		return nil, nil
	}
	tlsConfig, err := servicerpc.TLSFromFiles(filepath.Join(cfg.IdentityDir, "ca.crt"), filepath.Join(cfg.IdentityDir, "tls.crt"), filepath.Join(cfg.IdentityDir, "tls.key"), "", true)
	if err != nil {
		return nil, errors.New("could not configure company CRM TLS")
	}
	resolver := &companycrm.Store{Pool: pool, Binding: cfg.Binding}
	scoped := client.WithTokenProvider(companycrm.ScopedTokens{Provider: tokens, Binding: cfg.Binding})
	service := &companycrm.Service{Resolver: resolver, CRM: scoped}
	server := httpserver.New(cfg.Address, httpmiddleware.RequestID(companycrm.Router(service)))
	server.TLSConfig = tlsConfig
	return server, nil
}

func runCompanyCRM(ctx context.Context, server *http.Server, logger *slog.Logger, shutdown time.Duration) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	return serveCompanyCRM(ctx, server, listener, logger, shutdown)
}
func serveCompanyCRM(ctx context.Context, server *http.Server, listener net.Listener, logger *slog.Logger, shutdown time.Duration) error {
	outcome := make(chan error, 1)
	go func() { outcome <- server.ServeTLS(listener, "", "") }()
	select {
	case err := <-outcome:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), shutdown)
	defer cancel()
	err := server.Shutdown(stop)
	if err != nil {
		_ = server.Close()
	}
	<-outcome
	logger.Info("company CRM HTTPS stopped")
	return err
}
