package crmevents

import (
	"context"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestReauthorizedSourceResumesWithoutManualSync(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	ctx := context.Background()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{}, serviceapi.Fail(serviceapi.ReauthRequired, "revoked")
	}
	runPages(t, s, 1)
	state, _ := s.Status(ctx, serviceapi.Auth{})
	if !state.ReauthRequired {
		t.Fatalf("source not parked: %+v", state)
	}

	// While Core still reports the installation unauthorized, the probe keeps the source parked.
	p.issueError = serviceapi.Fail(serviceapi.ReauthRequired, "installation requires authorization")
	if err := s.ResumeReauthorized(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ = s.Status(ctx, serviceapi.Auth{}); !state.ReauthRequired {
		t.Fatalf("resumed before reauthorization: %+v", state)
	}

	// The OAuth callback made the installation active again; the next probe is paced.
	p.issueError = nil
	if err := s.ResumeReauthorized(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ = s.Status(ctx, serviceapi.Auth{}); !state.ReauthRequired {
		t.Fatalf("probe ignored its interval: %+v", state)
	}
	if _, err := testStore(s).pool.Exec(ctx, `UPDATE event_sources SET next_poll_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeReauthorized(ctx); err != nil {
		t.Fatal(err)
	}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }
	runPages(t, s, 2)
	state, _ = s.Status(ctx, serviceapi.Auth{})
	if state.ReauthRequired || state.State != "idle" || state.VerifiedThrough == 0 {
		t.Fatalf("source did not resume: %+v", state)
	}
}

func TestReauthProbeLeavesOperatorStatesAlone(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	ctx := context.Background()
	if _, err := s.Apply(ctx, serviceapi.Command{CommandID: "stop", Kind: "disable"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeReauthorized(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := testStore(s).ResumeReauthorized(ctx, p.principal.InstallationID)
	if err != nil || resumed {
		t.Fatalf("disabled source resumed=%t err=%v", resumed, err)
	}
	if state, _ := s.Status(ctx, serviceapi.Auth{}); state.State != "disabled" {
		t.Fatalf("operator disable changed: %+v", state)
	}
}
