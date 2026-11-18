package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"google.golang.org/grpc"
)

func startPlatformSnapshot(pool *pgxpool.Pool, address string) (*grpc.Server, <-chan error, error) {
	dir := os.Getenv("PLATFORM_CORE_IDENTITY_DIR")
	if dir == "" || os.Getenv("PLATFORM_SNAPSHOT_KEY_FILE") == "" {
		return nil, nil, errors.New("Core snapshot listener requires a TLS identity directory and delegation key")
	}
	cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"), "", true)
	if err != nil {
		return nil, nil, errors.New("could not configure Core snapshot TLS")
	}
	snapshot, err := corelifecycle.NewSnapshotService(corelifecycle.NewStore(pool), os.Getenv("PLATFORM_SNAPSHOT_KEY_FILE"))
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	server := servicerpc.NewServer(cfg, &servicerpc.Endpoints{PlatformInstallations: snapshot})
	outcome := make(chan error, 1)
	go func() { outcome <- server.Serve(listener) }()
	return server, outcome, nil
}
