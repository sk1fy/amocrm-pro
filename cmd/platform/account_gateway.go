package main

import (
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"

	"github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type accountGatewayConfig struct {
	address, identityDir, serverName string
}

func readAccountGatewayConfig(getenv func(string) string) (accountGatewayConfig, error) {
	cfg := accountGatewayConfig{
		address: getenv("PLATFORM_ACCOUNT_GATEWAY_ADDRESS"), identityDir: getenv("PLATFORM_ACCOUNT_GATEWAY_IDENTITY_DIR"),
		serverName: getenv("PLATFORM_ACCOUNT_GATEWAY_SERVER_NAME"),
	}
	if cfg.address == "" && cfg.identityDir == "" && cfg.serverName == "" {
		return cfg, nil
	}
	host, _, err := net.SplitHostPort(cfg.address)
	if err != nil || host == "" || cfg.identityDir == "" {
		return accountGatewayConfig{}, errors.New("Platform account Gateway requires PLATFORM_ACCOUNT_GATEWAY_ADDRESS in host:port form and PLATFORM_ACCOUNT_GATEWAY_IDENTITY_DIR")
	}
	if cfg.serverName == "" {
		cfg.serverName = host
	}
	return cfg, nil
}

func (cfg accountGatewayConfig) connect() (*grpc.ClientConn, error) {
	tlsConfig, err := servicerpc.TLSFromFiles(filepath.Join(cfg.identityDir, "ca.crt"), filepath.Join(cfg.identityDir, "tls.crt"),
		filepath.Join(cfg.identityDir, "tls.key"), cfg.serverName, false)
	if err != nil {
		return nil, errors.New("could not configure Platform account Gateway TLS")
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return errors.New("unverified Core account Gateway certificate")
		}
		uris := state.PeerCertificates[0].URIs
		if len(uris) != 1 || uris[0].String() != platformlifecycle.CoreIdentity {
			return errors.New("incorrect Core account Gateway identity")
		}
		return nil
	}
	return grpc.NewClient(cfg.address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(servicerpc.MaxMessageSize), grpc.MaxCallSendMsgSize(servicerpc.MaxMessageSize)))
}
