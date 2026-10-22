// Package colorleads implements the isolated Color Leads product store.
package colorleads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

var (
	ErrNotConfigured    = errors.New("not_configured")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrRouteRejected    = errors.New("route_rejected")
	ErrRevisionConflict = errors.New("revision_conflict")
	ErrInvalid          = errors.New("invalid_payload")
)

const ProductCode = "rkrs_color_leads"
const MaxColors = 1000

type Fence struct {
	Epoch      int64
	Generation int64
}

// Guard must lock the authoritative route AND installation incarnation in tx
// until commit, verify product binding, active Core ownership, epoch/generation,
// entitlement and user permissions (especially settings writes). A cached check
// or HTTP check cannot implement this contract. Reads also require admission.
// Lock order: active installation/integration, guard, product document.
type Guard interface {
	Admit(context.Context, pgx.Tx, widgetauth.Principal, Fence, bool) error
}
type SettingsDocument struct {
	Settings json.RawMessage `json:"settings"`
	Revision int64           `json:"revision"`
}
type Color struct {
	LeadID          int64   `json:"lead_id"`
	StageID         int64   `json:"stage_id"`
	PipelineID      int64   `json:"pipeline_id"`
	BorderColor     *string `json:"borderColor"`
	BackgroundColor *string `json:"backgroundColor"`
	Timestamp       *string `json:"timestamp"`
}
type ColorsDocument struct {
	Colors   []Color `json:"colors"`
	Revision int64   `json:"revision"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

// Settings remain lossless JSON: the migration must not silently drop legacy
// condition fields. The browser owns rule evaluation; this boundary caps shape
// and size and preserves the raw source for explicit migration reconciliation.
func ValidateSettings(raw json.RawMessage) error {
	if len(raw) > 256*1024 || !json.Valid(raw) || rejectDuplicateKeys(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return ErrInvalid
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ErrInvalid
	}
	if rules, ok := object["main_settings"]; ok {
		var entries []json.RawMessage
		if json.Unmarshal(rules, &entries) != nil || bytes.Equal(bytes.TrimSpace(rules), []byte("null")) || len(entries) > 500 {
			return ErrInvalid
		}
		for _, entry := range entries {
			var rule map[string]json.RawMessage
			if json.Unmarshal(entry, &rule) != nil || rule == nil {
				return ErrInvalid
			}
		}
	}
	return nil
}
func ValidateColors(colors []Color) error {
	if len(colors) > MaxColors {
		return ErrInvalid
	}
	seen := map[[3]int64]bool{}
	for _, c := range colors {
		key := [3]int64{c.LeadID, c.StageID, c.PipelineID}
		if c.LeadID <= 0 || c.StageID <= 0 || c.PipelineID <= 0 || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		for _, v := range []*string{c.BorderColor, c.BackgroundColor} {
			if v != nil && !hexColor.MatchString(*v) {
				return ErrInvalid
			}
		}
		if c.Timestamp != nil && len(*c.Timestamp) > 128 {
			return ErrInvalid
		}
	}
	return nil
}

// Reject duplicate JSON keys before PostgreSQL jsonb can silently collapse them.
func rejectDuplicateKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrInvalid
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalid
			}
			seen[name] = true
			if err = rejectDuplicateKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = rejectDuplicateKeys(d); err != nil {
				return err
			}
		}
	default:
		return ErrInvalid
	}
	_, err = d.Token()
	return err
}
