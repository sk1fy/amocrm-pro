package amocrm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type LeadScanPage struct {
	IDs     []int64
	HasNext bool
}

type leadScanWire struct {
	Embedded *struct {
		Leads *[]struct {
			ID *int64 `json:"id"`
		} `json:"leads"`
	} `json:"_embedded"`
	Links json.RawMessage `json:"_links"`
	empty bool
}

func (v *leadScanWire) NoContent() { v.empty = true }

// The documented updated_at range requires the amoCRM filtering feature.
// Failure remains visible; there is no unbounded fallback scan.
func (c *Client) DistributionLeadPage(ctx context.Context, installation uuid.UUID, from, to time.Time, page int) (LeadScanPage, error) {
	if page < 1 || page > 20 || !to.After(from) || to.Sub(from) > 24*time.Hour {
		return LeadScanPage{}, fmt.Errorf("invalid bounded scan")
	}
	q := url.Values{"page": {strconv.Itoa(page)}, "limit": {"250"}, "filter[updated_at][from]": {strconv.FormatInt(from.Unix(), 10)}, "filter[updated_at][to]": {strconv.FormatInt(to.Unix(), 10)}, "order[id]": {"asc"}}
	var raw leadScanWire
	if e := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/leads?"+q.Encode(), nil, &raw); e != nil {
		return LeadScanPage{}, e
	}
	if raw.empty {
		return LeadScanPage{IDs: []int64{}}, nil
	}
	if raw.Embedded == nil || raw.Embedded.Leads == nil {
		return LeadScanPage{}, ErrIncompleteResponse
	}
	var links map[string]json.RawMessage
	if json.Unmarshal(raw.Links, &links) != nil || links == nil {
		return LeadScanPage{}, ErrIncompleteResponse
	}
	p := LeadScanPage{}
	if next, ok := links["next"]; ok && string(next) != "null" {
		var n struct {
			Href string `json:"href"`
		}
		if json.Unmarshal(next, &n) != nil || n.Href == "" {
			return p, ErrIncompleteResponse
		}
		p.HasNext = true
	}
	if len(*raw.Embedded.Leads) > 250 || (len(*raw.Embedded.Leads) == 0 && p.HasNext) {
		return LeadScanPage{}, ErrIncompleteResponse
	}
	seen := map[int64]bool{}
	for _, l := range *raw.Embedded.Leads {
		if l.ID == nil || *l.ID <= 0 || seen[*l.ID] {
			return LeadScanPage{}, ErrIncompleteResponse
		}
		seen[*l.ID] = true
		p.IDs = append(p.IDs, *l.ID)
	}
	return p, nil
}
