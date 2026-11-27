package amocrm

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type ActivationProfile struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Role  string `json:"role,omitempty"`
}

func (c *Client) WidgetActivationProfile(ctx context.Context, installation uuid.UUID, userID int64, includeRole bool) (ActivationProfile, error) {
	var user struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
		Rights struct {
			Active *bool `json:"is_active"`
			Admin  bool  `json:"is_admin"`
			RoleID int64 `json:"role_id"`
		} `json:"rights"`
		Embedded struct {
			Role struct {
				Name string `json:"name"`
			} `json:"role"`
		} `json:"_embedded"`
	}
	if userID <= 0 {
		return ActivationProfile{}, ErrIncompleteResponse
	}
	path := "/api/v4/users/" + strconv.FormatInt(userID, 10)
	if includeRole {
		path += "?with=role"
	}
	if err := c.DoJSON(ctx, installation, http.MethodGet, path, nil, &user); err != nil {
		return ActivationProfile{}, err
	}
	profile := ActivationProfile{Name: strings.TrimSpace(user.Name), Email: strings.TrimSpace(user.Email)}
	if user.ID != userID || user.Rights.Active == nil || !*user.Rights.Active || len(profile.Email) > 320 {
		return ActivationProfile{}, ErrIncompleteResponse
	}
	if !includeRole {
		return profile, nil
	}
	profile.Role = strings.TrimSpace(user.Embedded.Role.Name)
	if profile.Name == "" || len(profile.Name) > 512 || len(profile.Role) > 256 {
		return ActivationProfile{}, ErrIncompleteResponse
	}
	if profile.Role == "" && user.Rights.Admin {
		profile.Role = "Администратор"
	}
	if profile.Role == "" && user.Rights.RoleID > 0 {
		var role struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if err := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/roles/"+strconv.FormatInt(user.Rights.RoleID, 10), nil, &role); err == nil && role.ID == user.Rights.RoleID && len(role.Name) <= 256 {
			profile.Role = strings.TrimSpace(role.Name)
		}
	}
	return profile, nil
}
