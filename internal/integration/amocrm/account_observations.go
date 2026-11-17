package amocrm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (c *Client) ObserveAccountIdentity(ctx context.Context, installation uuid.UUID, account int64, coreDomain string) (serviceapi.ObservedAccountIdentity, error) {
	var result serviceapi.ObservedAccountIdentity
	base, err := AccountBaseURL(coreDomain)
	if err != nil {
		return result, ErrIncompleteResponse
	}
	var response struct {
		ID        int64   `json:"id"`
		Name      *string `json:"name"`
		Subdomain string  `json:"subdomain"`
		Embedded  struct {
			DateTime struct {
				Timezone string `json:"timezone"`
			} `json:"datetime_settings"`
		} `json:"_embedded"`
	}
	if err := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/account?with=datetime_settings", nil, &response); err != nil {
		return result, err
	}
	label := strings.ToLower(response.Subdomain)
	if account <= 0 || response.ID != account || !accountLabel.MatchString(label) ||
		response.Name == nil || strings.TrimSpace(*response.Name) == "" || len(*response.Name) > 512 {
		return result, ErrIncompleteResponse
	}
	_, suffix, _ := strings.Cut(base.Hostname(), ".")
	result.CurrentDomain = label + "." + suffix
	result.DisplayName = *response.Name
	zone := response.Embedded.DateTime.Timezone
	if zone != "" && zone != "Local" && len(zone) <= 128 {
		if _, err := time.LoadLocation(zone); err == nil {
			result.TimeZone = &zone
		}
	}
	return result, nil
}

const (
	observedUsersPageSize = 250
	observedUsersMaxPages = 4
)

type observedUsersPage struct {
	Number *int `json:"_page"`
	Count  *int `json:"_page_count"`
	Total  *int `json:"_total_items"`
	Links  struct {
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
	Embedded struct {
		Users []struct {
			ID     int64   `json:"id"`
			Name   *string `json:"name"`
			Rights struct {
				Active *bool `json:"is_active"`
				Admin  *bool `json:"is_admin"`
			} `json:"rights"`
		} `json:"users"`
	} `json:"_embedded"`
}

func (c *Client) ObserveCRMUsers(ctx context.Context, installation uuid.UUID) (serviceapi.ObservedCRMUserRoster, error) {
	users := make(serviceapi.ObservedCRMUserRoster, 0)
	seen := make(map[int64]bool)
	var total, pages *int
	for page := 1; page <= observedUsersMaxPages; page++ {
		var response observedUsersPage
		path := fmt.Sprintf("/api/v4/users?limit=%d&page=%d", observedUsersPageSize, page)
		if err := c.DoJSON(ctx, installation, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		entries := response.Embedded.Users
		more := response.Links.Next != nil
		if entries == nil || len(entries) > observedUsersPageSize ||
			(response.Number != nil && *response.Number != page) ||
			(more && (response.Links.Next.Href == "" || len(entries) == 0)) || (page > 1 && len(entries) == 0) {
			return nil, ErrIncompleteResponse
		}
		if !agreePageMetadata(&total, response.Total, observedUsersPageSize*observedUsersMaxPages) ||
			!agreePageMetadata(&pages, response.Count, observedUsersMaxPages) {
			return nil, ErrIncompleteResponse
		}
		for _, entry := range entries {
			if entry.ID <= 0 || entry.Name == nil || len(*entry.Name) > 512 ||
				entry.Rights.Active == nil || entry.Rights.Admin == nil || seen[entry.ID] {
				return nil, ErrIncompleteResponse
			}
			seen[entry.ID] = true
			users = append(users, serviceapi.ObservedCRMUser{
				ID: entry.ID, Name: *entry.Name, IsActive: *entry.Rights.Active, IsAdmin: *entry.Rights.Admin,
			})
		}
		if !consistentObservedPage(page, len(entries), len(users), more, total, pages) {
			return nil, ErrIncompleteResponse
		}
		if !more {
			return users, nil
		}
	}
	return nil, ErrIncompleteResponse
}

func agreePageMetadata(known **int, reported *int, max int) bool {
	if reported == nil {
		return true
	}
	if *reported < 0 || *reported > max || (*known != nil && **known != *reported) {
		return false
	}
	*known = reported
	return true
}

func consistentObservedPage(page, entries, seen int, more bool, total, pages *int) bool {
	if pages != nil && *pages != 0 && (*pages < page || more != (page < *pages)) {
		return false
	}
	if pages != nil && *pages == 0 && seen > 0 {
		return false
	}
	if total != nil {
		return seen <= *total && more == (seen < *total)
	}
	return more || pages != nil || entries < observedUsersPageSize
}
