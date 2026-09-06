package services

import (
	"context"
	"errors"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

type registryActivity struct {
	serviceapi.Activity
	calls int
}

func (p *registryActivity) Settings(context.Context, serviceapi.Auth) (serviceapi.Settings, error) {
	p.calls++
	return serviceapi.DefaultSettings(), nil
}

type registryEvents struct{ serviceapi.CRMEvents }
type registryGateway struct{ serviceapi.Gateway }

func registryReady(context.Context) error { return nil }

func TestRegistryBindsActualPortsAndReadiness(t *testing.T) {
	r := NewRegistry()
	product := &registryActivity{}
	events := &registryEvents{}
	gateway := &registryGateway{}
	remote := Placement{Mode: "grpc"}
	local := Placement{Mode: "embedded", OwnsDatabase: true, MaxConnections: 3}
	var down error
	if err := r.Register(Activity, product, local, func(context.Context) error { return down }); err != nil {
		t.Fatal(err)
	}
	if err := r.Seal(); err == nil {
		t.Fatal("accepted missing CRM Events/Gateway dependencies")
	}
	if err := r.Register(CRMEvents, events, remote, registryReady); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("gateway", gateway, remote, registryReady); err != nil {
		t.Fatal(err)
	}
	if err := r.Seal(); err != nil {
		t.Fatal(err)
	}
	if r.Activity() != product || r.Events() != events || r.Gateway() != gateway {
		t.Fatal("registry did not retain actual selected ports")
	}
	if _, err := r.Activity().Settings(context.Background(), serviceapi.Auth{}); err != nil || product.calls != 1 {
		t.Fatal("selected product was not called")
	}
	if err := r.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	down = errors.New("owner unavailable")
	if err := r.Ready(context.Background()); !errors.Is(err, down) {
		t.Fatalf("registered readiness ignored owner failure: %v", err)
	}
	entries := r.Snapshot()
	if len(entries) != 3 || entries[0].Descriptor.Version != "v1" || entries[0].Descriptor.ProductVersion != "v0" || entries[0].Placement != local {
		t.Fatalf("runtime snapshot=%+v", entries)
	}
	entries[0].Descriptor.Dependencies[0] = "mutated"
	if r.Snapshot()[0].Descriptor.Dependencies[0] == "mutated" {
		t.Fatal("snapshot mutated registry")
	}
	if err := r.Register(Activity, product, local, registryReady); err == nil {
		t.Fatal("late registration accepted")
	}
}

func TestRegistryRejectsBrokenRegistration(t *testing.T) {
	var typedNil *registryActivity
	for _, tc := range []struct {
		name, code string
		port       any
		placement  Placement
		ready      func(context.Context) error
	}{
		{"unknown", "unknown", &registryActivity{}, Placement{Mode: "grpc"}, registryReady},
		{"mode", Activity, &registryActivity{}, Placement{Mode: "fallback"}, registryReady},
		{"nil", Activity, typedNil, Placement{Mode: "grpc"}, registryReady},
		{"wrong-contract", CRMEvents, &registryActivity{}, Placement{Mode: "grpc"}, registryReady},
		{"remote-pool", Activity, &registryActivity{}, Placement{Mode: "grpc", MaxConnections: 5}, registryReady},
		{"local-no-db", Activity, &registryActivity{}, Placement{Mode: "embedded"}, registryReady},
		{"no-readiness", Activity, &registryActivity{}, Placement{Mode: "grpc"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(tc.code, tc.port, tc.placement, tc.ready); err == nil {
				t.Fatal("invalid registration accepted")
			}
			if len(r.Snapshot()) != 0 {
				t.Fatal("failed registration changed registry")
			}
		})
	}
	r := NewRegistry()
	if err := r.Register("gateway", &registryGateway{}, Placement{Mode: "grpc"}, registryReady); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("gateway", &registryGateway{}, Placement{Mode: "grpc"}, registryReady); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if err := r.Ready(context.Background()); err == nil {
		t.Fatal("unsealed registry ready")
	}
}
