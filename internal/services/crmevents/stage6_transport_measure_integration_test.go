package crmevents

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"google.golang.org/protobuf/proto"
)

// L-03 exercises real loopback sockets: HTTP -> Activity mTLS -> Events mTLS
// -> PostgreSQL. Only the policy and external directory are fixture ports.
// The normal gate checks boundaries; opt-in adds 3 warmups and 25 samples.
func TestStage6TransportMeasure(t *testing.T) {
	pool := eventsPool(t)
	store := NewPostgres(pool, DefaultConfig())
	src := rel02InsertSource(t, pool, time.Now().UTC())
	src.Users, src.From, src.To = rel02Users(8), time.Now().Add(-time.Hour).Unix(), time.Now().Unix()
	payload := []byte(`[{"text":"` + strings.Repeat("w", 32000) + `"}]`)
	rel02CopyEvents(t, pool, src, 100, payload, false)
	gw := &rel02Gateway{users: src.Users}
	policy := &testPolicy{principal: src.Principal}
	owner := NewWithRepository(store, policy, gw, DefaultConfig())
	chain := stage6HTTPChain(t, src, owner, policy, gw)
	cardID := rel02AnyEventID(t, pool, src.Principal.InstallationID)
	panelPath := func(limit int, compact bool) string {
		return fmt.Sprintf("/api/v1/widget/activity/panel?from=%d&to=%d&limit=%d&compact=%t", src.From, src.To, limit, compact)
	}
	request := func(t *testing.T, path string) (int, []byte) {
		t.Helper()
		resp, err := chain.http.Client().Get(chain.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, serviceapi.MaxResponseBytes+1024))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > serviceapi.MaxResponseBytes {
			t.Fatalf("HTTP body exceeded 3 MiB: %d", len(body))
		}
		if resp.StatusCode != http.StatusOK && (resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), `"code":"resource_exhausted"`)) {
			t.Fatalf("unexpected HTTP response %d: %.300s", resp.StatusCode, body)
		}
		return resp.StatusCode, body
	}
	// Discover the actual HTTP boundary, including presentation fields and the
	// final JSON envelope, rather than inferring it from an owner DTO.
	low, high := 1, 100
	for low < high {
		mid := (low + high + 1) / 2
		code, _ := request(t, panelPath(mid, false))
		if code == http.StatusOK {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 100 {
		t.Fatal("large non-compact profile did not reach the response bound")
	}
	samples, warmup := 1, 0
	full := os.Getenv("STAGE6_REL02_MEASURE") == "true"
	if full {
		samples, warmup = rel02Samples, rel02Warmup
	}
	report := &rel02Report{}
	results := map[string]any{"last_http_ok_limit": low, "first_http_rejected_limit": low + 1, "transport": "HTTP -> Activity mTLS -> CRM Events mTLS -> PostgreSQL", "samples": samples}
	for _, tc := range []struct {
		name, path string
		limit      int
		compact    bool
		want       int
		p95, p99   time.Duration
	}{
		{"near_bound", panelPath(low, false), low, false, 200, rel02PanelP95, rel02PanelP99},
		{"over_bound", panelPath(low+1, false), low + 1, false, 429, rel02PanelP95, rel02PanelP99},
		{"full_100", panelPath(100, false), 100, false, 429, rel02PanelP95, rel02PanelP99},
		{"compact_100", panelPath(100, true), 100, true, 200, rel02PanelP95, rel02PanelP99},
		{"card", "/api/v1/widget/activity/events/" + cardID, 0, false, 200, rel02EventCardP95, rel02EventCardP99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var durations []time.Duration
			var body []byte
			for i := -warmup; i < samples; i++ {
				start := time.Now()
				code, data := request(t, tc.path)
				elapsed := time.Since(start)
				if code != tc.want {
					t.Fatalf("status %d want %d", code, tc.want)
				}
				body = data
				if i >= 0 {
					durations = append(durations, elapsed)
				}
			}
			lat := rel02Percentiles(durations)
			if full {
				rel02CheckLatency(t, report, tc.name, lat, tc.p95, tc.p99)
			}
			entry := map[string]any{"http_status": tc.want, "http_bytes": len(body), "p95": lat.p95.String(), "p99": lat.p99.String()}
			if tc.want == 200 && tc.limit > 0 {
				var panel serviceapi.Panel
				if err := json.Unmarshal(body, &panel); err != nil {
					t.Fatal(err)
				}
				if len(panel.Data.Events) != tc.limit || panel.Data.PayloadsOmitted != tc.compact {
					t.Fatalf("truncated/incorrect projection: events=%d omitted=%t", len(panel.Data.Events), panel.Data.PayloadsOmitted)
				}
				if tc.name == "near_bound" && len(body) < serviceapi.MaxResponseBytes*9/10 {
					t.Fatalf("boundary probe too small: %d", len(body))
				}
			}
			q := &pb.Query{From: src.From, To: src.To, Limit: int32(tc.limit), Compact: tc.compact}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var wire proto.Message
			var err error
			if tc.limit == 0 {
				wire, err = chain.activity.GetEvent(ctx, &pb.EventRequest{EventId: cardID})
			} else {
				wire, err = chain.activity.GetPanel(ctx, q)
			}
			if tc.want == 429 {
				if serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
					t.Fatalf("RPC boundary error: %v", err)
				}
				entry["activity_rpc_error"] = string(serviceapi.ErrorCode(err))
			} else {
				if err != nil {
					t.Fatal(err)
				}
				entry["activity_rpc_bytes"] = proto.Size(wire)
				if proto.Size(wire) >= servicerpc.MaxMessageSize {
					t.Fatal("RPC success crossed frame limit")
				}
			}
			// Inspect real generated responses from the owner, with the actual
			// production conversion. The owner can fit when the Panel cannot.
			if tc.limit == 0 {
				wire, err = chain.owner.GetEvent(ctx, &pb.EventRequest{EventId: cardID})
			} else {
				wire, err = chain.owner.QueryEvents(ctx, q)
			}
			if err != nil {
				if serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
					t.Fatal(err)
				}
				entry["owner_rpc_error"] = string(serviceapi.ErrorCode(err))
			} else {
				entry["owner_rpc_bytes"] = proto.Size(wire)
			}
			results[tc.name] = entry
		})
	}
	if full {
		rel02WriteJSON(t, filepath.Join(rel02ArtifactDir(t), "transport.json"), results)
	}
}

type stage6Chain struct {
	http     *httptest.Server
	owner    pb.CRMEventsClient
	activity pb.ActivityClient
}

func stage6HTTPChain(t *testing.T, src rel02Source, owner *Service, policy serviceapi.Policy, gw serviceapi.Gateway) stage6Chain {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	tlsConfig := func(identity string, server bool) *tls.Config {
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse("spiffe://amocrm-pro/" + identity)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, URIs: []*url.URL{u}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, RootCAs: roots, ServerName: "localhost"}
		if server {
			cfg.ClientCAs, cfg.ClientAuth = roots, tls.RequireAndVerifyClientCert
		}
		return cfg
	}
	start := func(identity, caller string, endpoints *servicerpc.Endpoints) *servicerpc.Clients {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := servicerpc.NewServer(tlsConfig(identity, true), endpoints)
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, err := servicerpc.Dial(ctx, listener.Addr().String(), tlsConfig(caller, false))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	events := start(serviceapi.EventsService, serviceapi.ActivityService, &servicerpc.Endpoints{CRMEvents: owner})
	act := activity.New(rel02Store{}, policy, events.CRMEvents, gw)
	remote := start(serviceapi.ActivityService, serviceapi.CoreService, &servicerpc.Endpoints{Activity: act})
	bridge := activitybridge.New(nil, policy, remote.Activity, events.CRMEvents)
	router := chi.NewRouter()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := widgetauth.Principal{InstallationID: src.Principal.InstallationID, IntegrationID: src.Principal.IntegrationID, UserID: src.Principal.ActorID}
			next.ServeHTTP(w, r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p)))
		})
	}
	bridge.RegisterHTTP(router, inject, inject)
	httpServer := httptest.NewServer(router)
	httpServer.Client().Timeout = 10 * time.Second
	t.Cleanup(httpServer.Close)
	return stage6Chain{http: httpServer, owner: pb.NewCRMEventsClient(events.Connection()), activity: pb.NewActivityClient(remote.Connection())}
}
