package crmevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type Slice struct {
	ID, InstallationID, IntegrationID uuid.UUID
	From, To, Target                  time.Time
	Kind                              string
	Page, Pass, Attempts              int
	Digest, Previous                  string
	Token                             int64
}

func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, err := s.RunOnce(ctx)
				s.logBackgroundError("collect", err)
				if err != nil || !worked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(250 * time.Millisecond):
					}
				}
			}
		}()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	retention := time.NewTicker(time.Minute)
	defer retention.Stop()
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			s.logBackgroundError("schedule", s.Schedule(ctx))
		case <-retention.C:
			_, err := s.Retain(ctx)
			s.logBackgroundError("retention", err)
		}
	}
}
func (s *Service) RunOnce(ctx context.Context) (bool, error) {
	c, err := s.repository.Claim(ctx)
	if errors.Is(err, ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	auth, err := s.policy.Issue(callCtx, serviceapi.IssueRequest{Scope: serviceapi.Scope{InstallationID: c.InstallationID, IntegrationID: c.IntegrationID}, System: true, Consumer: serviceapi.ActivityService, RequestID: c.ID.String(), Grants: []serviceapi.Grant{{Audience: serviceapi.EventsService, Action: serviceapi.ActionSync}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionEvents}}})
	var page serviceapi.EventPage
	if err == nil {
		page, err = s.gateway.Events(callCtx, serviceapi.EventPageRequest{Auth: auth, From: c.From.Unix(), To: c.To.Unix(), Page: c.Page, Limit: 100})
	}
	cancel()
	persistCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.PersistTimeout)
	defer stop()
	if err != nil {
		s.logBackgroundError("fetch", err)
		return true, s.repository.Fail(persistCtx, c, err)
	}
	if len(page.Events) > 100 || (page.HasNext && len(page.Events) == 0) {
		cause := serviceapi.Fail(serviceapi.InvalidArgument, "invalid upstream page")
		s.logBackgroundError("validate", cause)
		return true, s.repository.Fail(persistCtx, c, cause)
	}
	for _, event := range page.Events {
		if event.ID == "" || len(event.ID) > 256 || event.CreatedAt < c.From.Unix() || event.CreatedAt > c.To.Unix() || !validJSON(event.ValueBefore) || !validJSON(event.ValueAfter) {
			cause := serviceapi.Fail(serviceapi.InvalidArgument, "invalid upstream event")
			s.logBackgroundError("validate", cause)
			return true, s.repository.Fail(persistCtx, c, cause)
		}
	}
	return true, s.repository.SavePage(persistCtx, c, page)
}
func validJSON(b []byte) bool  { return len(b) == 0 || json.Valid(b) }
func (c Slice) String() string { return fmt.Sprintf("%s page %d pass %d", c.ID, c.Page, c.Pass) }

func (s *Service) Schedule(ctx context.Context) error        { return s.repository.Schedule(ctx) }
func (s *Service) Retain(ctx context.Context) (int64, error) { return s.repository.Retain(ctx) }

// Log only finite categories, never upstream payloads, tokens or arbitrary DB
// messages. Each stage/code is emitted at most once a minute across workers.
func (s *Service) logBackgroundError(stage string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	apiCode := serviceapi.ErrorCode(err)
	switch apiCode {
	case serviceapi.InvalidArgument, serviceapi.Unauthenticated, serviceapi.PermissionDenied, serviceapi.NotFound, serviceapi.Conflict, serviceapi.Unavailable, serviceapi.DeadlineExceeded, serviceapi.ResourceExhausted, serviceapi.ReauthRequired, serviceapi.Internal:
	default:
		apiCode = serviceapi.Internal
	}
	code := string(apiCode)
	if errors.Is(err, ErrLeaseLost) {
		code = "lease_lost"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "deadline_exceeded"
	}
	key := stage + ":" + code
	now := time.Now()
	s.logMu.Lock()
	if s.lastLog == nil {
		s.lastLog = make(map[string]time.Time)
	}
	if last, ok := s.lastLog[key]; ok && now.Sub(last) < time.Minute {
		s.logMu.Unlock()
		return
	}
	s.lastLog[key] = now
	s.logMu.Unlock()
	s.cfg.Logger.Error("CRM Events background operation failed", "stage", stage, "code", code)
}
