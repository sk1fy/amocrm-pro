package amocrm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// CompanyEntryProfile stays server-side; product reference DTOs expose no email.
type CompanyEntryProfile struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	Email  string    `json:"email"`
	Rights CRMRights `json:"rights"`
}

func (c *Client) CompanyEntryProfile(ctx context.Context, installation uuid.UUID, actor int64) (CompanyEntryProfile, error) {
	var profile CompanyEntryProfile
	if actor <= 0 {
		return profile, ErrIncompleteResponse
	}
	err := c.DoJSON(ctx, installation, http.MethodGet, fmt.Sprintf("/api/v4/users/%d", actor), nil, &profile)
	if err == nil && (profile.ID != actor || profile.Rights.IsActive == nil || len(profile.Name) > 512 || len(profile.Email) > 320) {
		err = ErrIncompleteResponse
	}
	return profile, err
}
