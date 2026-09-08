package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strconv"
)

// EventsPageLimit is verified against the official Events API (2026-09-06):
// https://www.amocrm.ru/developers/content/crm_platform/events-and-notes
const EventsPageLimit = 100

type CRMEvent struct {
	ID                  string          `json:"id"`
	CreatedAt           int64           `json:"created_at"`
	CreatedBy           int64           `json:"created_by"`
	Type                string          `json:"type"`
	EntityID            int64           `json:"entity_id"`
	EntityType          string          `json:"entity_type"`
	LinkedTalkContactID int64           `json:"linked_talk_contact_id,omitempty"`
	ValueBefore         json.RawMessage `json:"value_before"`
	ValueAfter          json.RawMessage `json:"value_after"`
}
type CRMEventPage struct {
	Events  []CRMEvent
	HasNext bool
}

// ListEvents uses fixed from/to filters; callers own boundary overlap and
// durable page progress. The next href is only a pagination signal, never an
// arbitrary URL to follow, so upstream links cannot escape the API boundary.
func (c *Client) ListEvents(ctx context.Context, id uuid.UUID, from, to int64, page, limit int) (CRMEventPage, error) {
	if from < 0 || to < from || to-from > 31*86400 || page < 1 || page > 100000 || limit < 1 || limit > EventsPageLimit {
		return CRMEventPage{}, errors.New("invalid bounded event window or page")
	}
	q := url.Values{"filter[created_at][from]": {strconv.FormatInt(from, 10)}, "filter[created_at][to]": {strconv.FormatInt(to, 10)}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	var result struct {
		Embedded struct {
			Events []CRMEvent `json:"events"`
		} `json:"_embedded"`
		Links struct {
			Next struct {
				Href string `json:"href"`
			} `json:"next"`
		} `json:"_links"`
	}
	if err := c.DoJSON(ctx, id, http.MethodGet, "/api/v4/events?"+q.Encode(), nil, &result); err != nil {
		return CRMEventPage{}, err
	}
	if len(result.Embedded.Events) > limit {
		return CRMEventPage{}, ErrIncompleteResponse
	}
	for _, e := range result.Embedded.Events {
		if e.ID == "" || len(e.ID) > 128 || e.CreatedAt < from || e.CreatedAt > to || e.LinkedTalkContactID < 0 || len(e.ValueBefore) > 32768 || len(e.ValueAfter) > 32768 {
			return CRMEventPage{}, ErrIncompleteResponse
		}
	}
	if result.Links.Next.Href != "" && len(result.Embedded.Events) == 0 {
		return CRMEventPage{}, ErrIncompleteResponse
	}
	return CRMEventPage{Events: result.Embedded.Events, HasNext: result.Links.Next.Href != ""}, nil
}

type DirectoryUser struct {
	ID        int64
	Name      string
	GroupID   int64
	GroupName string
}
type AccountDirectory struct {
	Users    []DirectoryUser
	Timezone string
}

// GetDirectory is bounded to 1000 users/4 pages and returns only fields consumed
// by Activity. It deliberately does not return emails or full rights profiles.
func (c *Client) GetDirectory(ctx context.Context, id uuid.UUID) (AccountDirectory, error) {
	var account struct {
		Embedded struct {
			Groups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"users_groups"`
			DateTime struct {
				Timezone string `json:"timezone"`
			} `json:"datetime_settings"`
		} `json:"_embedded"`
	}
	if err := c.DoJSON(ctx, id, http.MethodGet, "/api/v4/account?with=users_groups,datetime_settings", nil, &account); err != nil {
		return AccountDirectory{}, err
	}
	groups := map[int64]string{}
	for _, g := range account.Embedded.Groups {
		groups[g.ID] = g.Name
	}
	result := AccountDirectory{Timezone: account.Embedded.DateTime.Timezone, Users: []DirectoryUser{}}
	for page := 1; page <= 4; page++ {
		var response struct {
			Embedded struct {
				Users []struct {
					ID     int64  `json:"id"`
					Name   string `json:"name"`
					Rights struct {
						GroupID int64 `json:"group_id"`
					} `json:"rights"`
				} `json:"users"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/users?limit=250&page=%d", page), nil, &response); err != nil {
			return AccountDirectory{}, err
		}
		if len(response.Embedded.Users) > 250 {
			return AccountDirectory{}, ErrIncompleteResponse
		}
		for _, u := range response.Embedded.Users {
			if u.ID <= 0 {
				return AccountDirectory{}, ErrIncompleteResponse
			}
			result.Users = append(result.Users, DirectoryUser{u.ID, u.Name, u.Rights.GroupID, groups[u.Rights.GroupID]})
		}
		if response.Links.Next.Href == "" {
			return result, nil
		}
		if len(response.Embedded.Users) == 0 {
			return AccountDirectory{}, ErrIncompleteResponse
		}
	}
	return AccountDirectory{}, errors.New("amoCRM directory exceeds v0 limit of 1000 users")
}
