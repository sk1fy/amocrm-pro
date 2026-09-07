package crmevents

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type persistProbe struct {
	Repository
	save  bool
	fail  bool
	token int64
	cause error
}

func (p *persistProbe) Claim(context.Context) (Slice, error) {
	return Slice{From: time.Unix(1, 0), To: time.Unix(3, 0), Page: 1, Token: p.token}, nil
}
func (p *persistProbe) SavePage(ctx context.Context, c Slice, _ serviceapi.EventPage) error {
	p.save = true
	return p.check(ctx, c)
}
func (p *persistProbe) Fail(ctx context.Context, c Slice, cause error) error {
	p.fail = true
	p.cause = cause
	return p.check(ctx, c)
}
func (p *persistProbe) check(ctx context.Context, c Slice) error {
	if ctx.Err() != nil {
		return errors.New("persistence inherited canceled context")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		return errors.New("persistence deadline missing or unbounded")
	}
	if c.Token != p.token {
		return errors.New("lost fencing token")
	}
	return nil
}
func TestCanceledFetchPersistsAdmittedPageAndFailureWithBoundedContext(t *testing.T) {
	for _, kind := range []string{"valid", "invalid", "fetch_error"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := &persistProbe{token: 12}
			g := &testGateway{events: func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
				cancel()
				if kind == "fetch_error" {
					return serviceapi.EventPage{}, errors.New("network failure")
				}
				at := int64(2)
				if kind == "invalid" {
					at = 4
				}
				return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "a", CreatedAt: at}}}, nil
			}}
			svc := NewWithRepository(repo, &testPolicy{}, g, DefaultConfig())
			worked, err := svc.RunOnce(ctx)
			if err != nil || !worked {
				t.Fatalf("persist after cancellation %t %v", worked, err)
			}
			if repo.save != (kind == "valid") || repo.fail == (kind == "valid") {
				t.Fatalf("wrong terminal write %+v", repo)
			}
			if kind == "invalid" && serviceapi.ErrorCode(repo.cause) != serviceapi.InvalidArgument {
				t.Fatalf("invalid page not failed closed %v", repo.cause)
			}
		})
	}
}
func TestBackgroundDiagnosticsBoundedAndNoPayloads(t *testing.T) {
	var out bytes.Buffer
	cfg := DefaultConfig()
	cfg.Logger = slog.New(slog.NewJSONHandler(&out, nil))
	svc := NewWithRepository(nil, nil, nil, cfg)
	for i := 0; i < 100; i++ {
		svc.logBackgroundError("collect", serviceapi.Fail(serviceapi.Code("attacker-secret-token"), "sensitive payload"))
	}
	svc.logBackgroundError("schedule", errors.New("database password should never appear"))
	svc.logBackgroundError("retention", context.DeadlineExceeded)
	svc.logBackgroundError("collect", context.Canceled)
	got := out.String()
	if strings.Count(got, "\n") != 3 || strings.Contains(got, "secret") || strings.Contains(got, "payload") || strings.Contains(got, "password") || len(svc.lastLog) != 3 {
		t.Fatalf("unbounded/sensitive diagnostics %s", got)
	}
}

func TestPersistenceTimeoutCannotExceedShutdownAllowance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PersistTimeout = time.Hour
	if got := normalizeConfig(cfg).PersistTimeout; got != 5*time.Second {
		t.Fatalf("persist bound %v", got)
	}
	cfg.PersistTimeout = time.Second
	if got := normalizeConfig(cfg).PersistTimeout; got != time.Second {
		t.Fatalf("shorter persist bound ignored %v", got)
	}
}
