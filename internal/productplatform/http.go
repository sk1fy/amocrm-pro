package productplatform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const (
	coreIdentity       = wire.CoreIdentity
	managerBFFIdentity = "spiffe://amocrm-pro/manager-bff"
	coreEventsPath     = wire.EventsPath
	managerPath        = "/internal/platform/v1/managers/"
)

func PrivateTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("load Platform server TLS certificate or key")
	}
	pem, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, errors.New("read Platform client CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("parse Platform client CA")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
	}, nil
}

func PrivateHandler(core, manager http.Handler) http.Handler {
	if core == nil {
		core = http.NotFoundHandler()
	}
	if manager == nil {
		manager = http.NotFoundHandler()
	}

	mux := http.NewServeMux()
	mux.Handle(coreEventsPath, requireIdentity(coreIdentity, core))
	mux.Handle(wire.ActivationPath, requireIdentity(coreIdentity, core))
	mux.Handle(managerPath, requireIdentity(managerBFFIdentity, manager))
	mux.HandleFunc("/internal/platform/v1/managers", http.NotFound)
	return mux
}

func requireIdentity(identity string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			leaf := r.TLS.VerifiedChains[0][0]
			if len(leaf.URIs) == 1 && leaf.URIs[0].String() == identity {
				next.ServeHTTP(w, r)
				return
			}
		}
		if identity == managerBFFIdentity {
			writeManagerError(w, http.StatusForbidden, "permission_denied", uuid.Nil, 0)
			return
		}
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	})
}

func HealthHandler(checkAccess func(context.Context) error, timeout time.Duration) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := checkAccess(ctx); err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
