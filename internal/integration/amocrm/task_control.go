package amocrm

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// CheckTaskControlAuthorization is a read-only check after a signed disconnect
// hint. Do not refresh AFTER a 401: preserving the explicit current-token denial
// distinguishes it from generic refresh/configuration failures. Token() still
// obtains a fresh credential before the call using the normal provider.
func (c *Client) CheckTaskControlAuthorization(ctx context.Context, id uuid.UUID, account int64) error {
	access, err := c.tokens.Token(ctx, id)
	if err != nil {
		return err
	}
	if access.InstallationID != id || access.AccountID != account {
		return ErrIncompleteResponse
	}
	if err = c.waitBudget(ctx, access); err != nil {
		return err
	}
	status, headers, body, err := c.request(ctx, access, http.MethodGet, "/api/v4/account", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return classifyResponse(status, headers, time.Now())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(body, &result) != nil || result.ID != account {
		return ErrIncompleteResponse
	}
	return nil
}
