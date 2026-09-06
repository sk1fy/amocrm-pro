package services

import (
	"context"
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"reflect"
	"slices"
)

// Placement records actual composition, never DSNs or secrets. A remote client
// owns no local pool or workers. Hosted standalone services still own their DB.
type Placement struct {
	Mode           string `json:"mode"`
	OwnsDatabase   bool   `json:"owns_database"`
	MaxConnections int    `json:"max_connections"`
	Workers        int    `json:"workers"`
}
type RegisteredComponent struct {
	Descriptor Component `json:"descriptor"`
	Placement  Placement `json:"placement"`
}
type registered struct {
	descriptor RegisteredComponent
	port       any
	ready      func(context.Context) error
}

// Registry is populated once by a composition root, before serving. It selects
// the actual typed ports used by HTTP/gRPC adapters and owns their readiness.
// There is no dynamic loading, service discovery or product SQL here.
type Registry struct {
	bindings map[string]registered
	sealed   bool
}

func NewRegistry() *Registry { return &Registry{bindings: map[string]registered{}} }
func (r *Registry) Register(code string, port any, placement Placement, ready func(context.Context) error) error {
	if r.sealed {
		return fmt.Errorf("component registration is sealed")
	}
	desc, known := Describe(code)
	if !known {
		return fmt.Errorf("unknown component %q", code)
	}
	if _, exists := r.bindings[code]; exists {
		return fmt.Errorf("duplicate component %q", code)
	}
	if !slices.Contains(desc.Modes, placement.Mode) {
		return fmt.Errorf("unsupported %s mode %q", code, placement.Mode)
	}
	if ready == nil {
		return fmt.Errorf("component %s lacks readiness", code)
	}
	if placement.Workers < 0 || placement.MaxConnections < 0 || placement.OwnsDatabase && placement.MaxConnections == 0 || !placement.OwnsDatabase && (placement.MaxConnections != 0 || placement.Workers != 0) || placement.Mode == "embedded" && !placement.OwnsDatabase {
		return fmt.Errorf("invalid local/remote ownership quota for %s", code)
	}
	valid := false
	if port == nil || reflect.ValueOf(port).Kind() == reflect.Pointer && reflect.ValueOf(port).IsNil() {
		return fmt.Errorf("component %s has a nil port", code)
	}
	switch code {
	case Activity:
		p, ok := port.(serviceapi.Activity)
		valid = ok && p != nil
	case CRMEvents:
		p, ok := port.(serviceapi.CRMEvents)
		valid = ok && p != nil
	case serviceapi.GatewayService:
		p, ok := port.(serviceapi.Gateway)
		valid = ok && p != nil
	}
	if !valid {
		return fmt.Errorf("component %s port does not implement its contract", code)
	}
	r.bindings[code] = registered{RegisteredComponent{desc, placement}, port, ready}
	return nil
}
func (r *Registry) Seal() error {
	for code, b := range r.bindings {
		for _, dependency := range b.descriptor.Descriptor.Dependencies {
			// These two are the existing Core-owned facilities, co-hosted by Gateway.
			if dependency == "core-policy" || dependency == "oauth" {
				continue
			}
			if _, ok := r.bindings[dependency]; !ok {
				return fmt.Errorf("component %s lacks dependency %s", code, dependency)
			}
		}
	}
	r.sealed = true
	return nil
}
func (r *Registry) Activity() serviceapi.Activity {
	p, _ := r.bindings[Activity].port.(serviceapi.Activity)
	return p
}
func (r *Registry) Events() serviceapi.CRMEvents {
	p, _ := r.bindings[CRMEvents].port.(serviceapi.CRMEvents)
	return p
}
func (r *Registry) Gateway() serviceapi.Gateway {
	p, _ := r.bindings[serviceapi.GatewayService].port.(serviceapi.Gateway)
	return p
}
func (r *Registry) Snapshot() []RegisteredComponent {
	out := make([]RegisteredComponent, 0, len(r.bindings))
	for _, d := range Components() {
		if b, ok := r.bindings[d.Code]; ok {
			entry := b.descriptor
			entry.Descriptor = d
			out = append(out, entry)
		}
	}
	return out
}
func (r *Registry) Ready(ctx context.Context) error {
	if !r.sealed {
		return fmt.Errorf("component registration is not complete")
	}
	for _, desc := range Components() {
		if b, ok := r.bindings[desc.Code]; ok {
			if err := b.ready(ctx); err != nil {
				return fmt.Errorf("%s: %w", desc.Code, err)
			}
		}
	}
	return nil
}
