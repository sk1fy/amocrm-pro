package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	amocrmclient "github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"google.golang.org/grpc"
)

func startPlatformAccountGateway(pool *pgxpool.Pool, client *amocrmclient.Client, address string) (*grpc.Server, net.Addr, <-chan error, error) {
	if address == "" {
		return nil, nil, nil, nil
	}
	dir := os.Getenv("PLATFORM_CORE_IDENTITY_DIR")
	if dir == "" {
		return nil, nil, nil, errors.New("Platform account Gateway requires PLATFORM_CORE_IDENTITY_DIR")
	}
	config, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"), "", true)
	if err != nil {
		return nil, nil, nil, errors.New("could not configure Platform account Gateway TLS")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, nil, err
	}
	server := servicerpc.NewServer(config, &servicerpc.Endpoints{
		PlatformAccountObservations: corelifecycle.NewAccountObservationService(corelifecycle.NewStore(pool), client),
		Ready:                       pool.Ping,
	})
	outcome := make(chan error, 1)
	go func() { outcome <- server.Serve(listener) }()
	return server, listener.Addr(), outcome, nil
}
