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
			_ = s.Schedule(ctx)
		case <-retention.C:
			_, _ = s.Retain(ctx)
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
	if err != nil {
		persistCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		return true, s.repository.Fail(persistCtx, c, err)
	}
	if len(page.Events) > 100 || (page.HasNext && len(page.Events) == 0) {
		return true, s.repository.Fail(ctx, c, serviceapi.Fail(serviceapi.InvalidArgument, "invalid upstream page"))
	}
	for _, event := range page.Events {
		if event.ID == "" || len(event.ID) > 256 || event.CreatedAt < c.From.Unix() || event.CreatedAt > c.To.Unix() || !validJSON(event.ValueBefore) || !validJSON(event.ValueAfter) {
			return true, s.repository.Fail(ctx, c, serviceapi.Fail(serviceapi.InvalidArgument, "invalid upstream event"))
		}
	}
	return true, s.repository.SavePage(ctx, c, page)
}
func validJSON(b []byte) bool  { return len(b) == 0 || json.Valid(b) }
func (c Slice) String() string { return fmt.Sprintf("%s page %d pass %d", c.ID, c.Page, c.Pass) }

func (s *Service) Schedule(ctx context.Context) error        { return s.repository.Schedule(ctx) }
func (s *Service) Retain(ctx context.Context) (int64, error) { return s.repository.Retain(ctx) }
