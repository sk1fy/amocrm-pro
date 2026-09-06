package componentruntime

import (
	"context"
	"encoding/json"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"net/http/httptest"
	"testing"
)

type catalogGateway struct{ serviceapi.Gateway }

func TestGraphCatalogAndReadinessUseRegisteredComponents(t *testing.T) {
	g := newGraph(context.Background())
	defer g.Close()
	ready := false
	if err := g.components.Register("gateway", &catalogGateway{}, ownedPlacement("grpc", 6, 0), func(context.Context) error { ready = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.components.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := g.Ready(context.Background()); err != nil || !ready {
		t.Fatalf("registry readiness not consulted: %v", err)
	}
	reply := httptest.NewRecorder()
	g.Catalog(reply, httptest.NewRequest("GET", "/components", nil))
	var bindings []services.RegisteredComponent
	if err := json.Unmarshal(reply.Body.Bytes(), &bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Descriptor.Code != "gateway" || bindings[0].Placement.MaxConnections != 6 || !bindings[0].Placement.OwnsDatabase {
		t.Fatalf("catalog did not reflect composition: %s", reply.Body.String())
	}
}
