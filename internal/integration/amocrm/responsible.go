package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var ErrLeadAbsent = errors.New("amoCRM lead absent (documented204)")
var ErrLeadDeleted = errors.New("amoCRM lead explicitly deleted")

type leadSnapshotWire struct {
	ID                *int64  `json:"id"`
	Name              *string `json:"name"`
	PipelineID        *int64  `json:"pipeline_id"`
	StatusID          *int64  `json:"status_id"`
	ResponsibleUserID *int64  `json:"responsible_user_id"`
	UpdatedAt         *int64  `json:"updated_at"`
	IsDeleted         *bool   `json:"is_deleted"`
	absent            bool
}

func (v *leadSnapshotWire) NoContent() { v.absent = true }

// GetLeadSnapshot reads the complete distribution precondition without making
// older leadstatus callers require the new fields in GetLeadState.
func (c *Client) GetLeadSnapshot(ctx context.Context, installationID uuid.UUID, leadID int64) (LeadState, error) {
	if leadID <= 0 {
		return LeadState{}, errors.New("amoCRM lead id must be positive")
	}
	var raw leadSnapshotWire
	if err := c.DoJSON(ctx, installationID, http.MethodGet, fmt.Sprintf("/api/v4/leads/%d", leadID), nil, &raw); err != nil {
		return LeadState{}, err
	}
	if raw.absent {
		return LeadState{}, ErrLeadAbsent
	}
	if raw.IsDeleted != nil && *raw.IsDeleted {
		if raw.ID == nil || *raw.ID != leadID {
			return LeadState{}, ErrIncompleteResponse
		}
		return LeadState{}, ErrLeadDeleted
	}
	if raw.ID == nil || *raw.ID != leadID || raw.Name == nil || raw.PipelineID == nil || *raw.PipelineID <= 0 || raw.StatusID == nil || *raw.StatusID <= 0 || raw.ResponsibleUserID == nil || *raw.ResponsibleUserID <= 0 || raw.UpdatedAt == nil || *raw.UpdatedAt <= 0 {
		return LeadState{}, ErrIncompleteResponse
	}
	return LeadState{ID: *raw.ID, Name: *raw.Name, PipelineID: *raw.PipelineID, StatusID: *raw.StatusID, ResponsibleUserID: *raw.ResponsibleUserID, UpdatedAt: *raw.UpdatedAt}, nil
}

type LeadResponsibleResult struct {
	HTTPStatus int
	Accepted   bool
	UpdatedAt  int64
}

// ResponsibleDispatchError separates proven local rejection from a possible
// remote effect. Dispatched means request() was invoked; it does not mean the
// server changed the lead. Transport/5xx/incomplete acknowledgements need GET
// reconciliation, never an automatic PATCH retry.
type ResponsibleDispatchError struct {
	Dispatched bool
	StatusCode int
	Cause      error
}

func (e *ResponsibleDispatchError) Error() string {
	return fmt.Sprintf("amoCRM responsible dispatch failed (dispatched=%t, status=%d): %v", e.Dispatched, e.StatusCode, e.Cause)
}
func (e *ResponsibleDispatchError) Unwrap() error { return e.Cause }

type LeadResponsibleMutation interface {
	Assign(context.Context, int64, int64) (LeadResponsibleResult, error)
}
type preparedLeadResponsibleMutation struct {
	client *Client
	access AccessToken
	used   atomic.Bool
}

// PrepareLeadResponsible obtains OAuth and one shared outbound budget slot
// before the worker requests its short-lived TeamOS decision token. Assign does
// not wait for a budget or silently refresh/replay a PATCH after a 401.
func (c *Client) PrepareLeadResponsible(ctx context.Context, installationID uuid.UUID) (LeadResponsibleMutation, error) {
	access, err := c.tokens.Token(ctx, installationID)
	if err != nil {
		return nil, err
	}
	if access.InstallationID != installationID || access.AccountID <= 0 || access.IntegrationID == uuid.Nil || access.Value == "" {
		return nil, ErrIncompleteResponse
	}
	if err = c.waitBudget(ctx, access); err != nil {
		return nil, fmt.Errorf("wait for amoCRM rate limit: %w", err)
	}
	// Redirects can replay a PATCH (307/308) or forward credentials. This
	// prepared sender makes exactly one HTTP exchange using the same limiter.
	sender := c.WithTokenProvider(c.tokens)
	httpClient := *c.httpClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sender.httpClient = &httpClient
	return &preparedLeadResponsibleMutation{client: sender, access: access}, nil
}
func (m *preparedLeadResponsibleMutation) Assign(ctx context.Context, leadID, targetID int64) (LeadResponsibleResult, error) {
	local := func(cause error) (LeadResponsibleResult, error) {
		return LeadResponsibleResult{}, &ResponsibleDispatchError{Cause: cause}
	}
	if leadID <= 0 || targetID <= 0 {
		return local(errors.New("lead and responsible user ids must be positive"))
	}
	if err := ctx.Err(); err != nil {
		return local(err)
	}
	if !m.used.CompareAndSwap(false, true) {
		return local(errors.New("prepared responsible mutation already consumed"))
	}
	// The only field sent is responsible_user_id. No pipeline, status, tags,
	// related entity, timestamp or custom field is rewritten.
	body := struct {
		ResponsibleUserID int64 `json:"responsible_user_id"`
	}{targetID}
	status, header, response, err := m.client.request(ctx, m.access, http.MethodPatch, fmt.Sprintf("/api/v4/leads/%d", leadID), body)
	result := LeadResponsibleResult{HTTPStatus: status}
	dispatched := func(cause error) (LeadResponsibleResult, error) {
		return result, &ResponsibleDispatchError{Dispatched: true, StatusCode: status, Cause: cause}
	}
	if err != nil {
		return dispatched(err)
	}
	if status != http.StatusOK {
		if status >= 200 && status < 300 {
			return dispatched(ErrIncompleteResponse)
		}
		classified := classifyResponse(status, header, time.Now())
		if status == http.StatusUnauthorized {
			markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.client.reauthTimeout)
			markErr := m.client.tokens.MarkReauthRequired(markContext, m.access.InstallationID, m.access.TokenVersion)
			cancel()
			if markErr != nil {
				return dispatched(errors.Join(classified, fmt.Errorf("mark amoCRM reauthorization required: %w", markErr)))
			}
		}
		return dispatched(classified)
	}
	// Official PATCH response: collection of changed lead IDs and updated_at.
	// A 200 alone is not proof; even a valid acknowledgement is confirmed by the
	// worker using a fresh GET, rather than treated as final success here.
	var ack struct {
		Embedded *struct {
			Leads []struct {
				ID        *int64 `json:"id"`
				UpdatedAt *int64 `json:"updated_at"`
			} `json:"leads"`
		} `json:"_embedded"`
	}
	if err = json.Unmarshal(response, &ack); err != nil {
		return dispatched(fmt.Errorf("decode responsible acknowledgement: %w", ErrIncompleteResponse))
	}
	if ack.Embedded == nil || len(ack.Embedded.Leads) != 1 || ack.Embedded.Leads[0].ID == nil || *ack.Embedded.Leads[0].ID != leadID || ack.Embedded.Leads[0].UpdatedAt == nil || *ack.Embedded.Leads[0].UpdatedAt <= 0 {
		return dispatched(ErrIncompleteResponse)
	}
	result.Accepted = true
	result.UpdatedAt = *ack.Embedded.Leads[0].UpdatedAt
	return result, nil
}
