package amocrm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

var ErrIncompleteResponse = errors.New("amoCRM API response is incomplete")

type UserAuthorization struct {
	ID     int64 `json:"id"`
	Rights struct {
		IsAdmin  bool `json:"is_admin"`
		IsActive bool `json:"is_active"`
	} `json:"rights"`
}

type LeadState struct {
	ID         int64 `json:"id"`
	PipelineID int64 `json:"pipeline_id"`
	StatusID   int64 `json:"status_id"`
}

func (c *Client) GetUserAuthorization(
	ctx context.Context,
	installationID uuid.UUID,
	userID int64,
) (UserAuthorization, error) {
	if userID <= 0 {
		return UserAuthorization{}, errors.New("amoCRM user id must be positive")
	}
	var user UserAuthorization
	if err := c.DoJSON(
		ctx, installationID, http.MethodGet,
		fmt.Sprintf("/api/v4/users/%d", userID), nil, &user,
	); err != nil {
		return UserAuthorization{}, err
	}
	if user.ID != userID {
		return UserAuthorization{}, ErrIncompleteResponse
	}
	return user, nil
}

func (c *Client) GetLeadState(
	ctx context.Context,
	installationID uuid.UUID,
	leadID int64,
) (LeadState, error) {
	if leadID <= 0 {
		return LeadState{}, errors.New("amoCRM lead id must be positive")
	}
	var lead LeadState
	if err := c.DoJSON(
		ctx, installationID, http.MethodGet,
		fmt.Sprintf("/api/v4/leads/%d", leadID), nil, &lead,
	); err != nil {
		return LeadState{}, err
	}
	if lead.ID != leadID || lead.PipelineID <= 0 || lead.StatusID <= 0 {
		return LeadState{}, ErrIncompleteResponse
	}
	return lead, nil
}

type LeadStatusMutation interface {
	SetLeadStatus(context.Context, int64, int64, int64) error
}

type preparedLeadStatusMutation struct {
	client *Client
	access AccessToken
}

func (c *Client) PrepareLeadStatus(
	ctx context.Context,
	installationID uuid.UUID,
) (LeadStatusMutation, error) {
	access, err := c.tokens.Token(ctx, installationID)
	if err != nil {
		return nil, err
	}
	return &preparedLeadStatusMutation{client: c, access: access}, nil
}

func (mutation *preparedLeadStatusMutation) SetLeadStatus(
	ctx context.Context,
	leadID int64,
	pipelineID int64,
	statusID int64,
) error {
	if leadID <= 0 || pipelineID <= 0 || statusID <= 0 {
		return errors.New("lead, pipeline, and status ids must be positive")
	}
	path := fmt.Sprintf("/api/v4/leads/%d", leadID)
	body := map[string]int64{"pipeline_id": pipelineID, "status_id": statusID}
	if err := mutation.client.limiter.wait(
		ctx, mutation.access.IntegrationID, mutation.access.AccountID,
	); err != nil {
		return fmt.Errorf("wait for amoCRM rate limit: %w", err)
	}
	status, header, _, err := mutation.client.request(
		ctx, mutation.access, http.MethodPatch, path, body,
	)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return classifyResponse(status, header, time.Now())
	}
	return nil
}
