package productplatform

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type productInput struct {
	Code         string
	DisplayName  *string
	Line         *string
	Availability *string
	Integrations *[]uuid.UUID
	Tariff       *int64
	TariffSet    bool
	Trial        *productTrial
	Links        *[]productLink
}

func parseProductInput(raw []byte, create bool) (productInput, error) {
	var input productInput
	var code, name, line, availability, refs, tariff, trial, links json.RawMessage
	fields := map[string]any{
		"display_name": &name, "line": &line, "availability": &availability, "integration_refs": &refs,
		"tariff_version": &tariff, "trial_policy": &trial, "links": &links,
	}
	if create {
		fields["code"] = &code
	}
	if err := readManagerObject(raw, fields); err != nil {
		return input, err
	}
	if create && (code == nil || name == nil || line == nil || availability == nil || refs == nil || tariff == nil || trial == nil || links == nil) {
		return input, errManagerInput
	}
	if code != nil {
		if json.Unmarshal(code, &input.Code) != nil || !wire.ValidProductCode(input.Code) {
			return input, errManagerInput
		}
	}
	for _, entry := range []struct {
		raw json.RawMessage
		dst **string
	}{{name, &input.DisplayName}, {line, &input.Line}, {availability, &input.Availability}} {
		if entry.raw != nil {
			if string(entry.raw) == "null" || json.Unmarshal(entry.raw, entry.dst) != nil {
				return input, errManagerInput
			}
		}
	}
	if input.DisplayName != nil {
		clean := strings.TrimSpace(*input.DisplayName)
		if !managerText(clean, 1, 200) {
			return input, errManagerInput
		}
		input.DisplayName = &clean
	}
	if input.Line != nil && !productLine(*input.Line) {
		return input, errManagerInput
	}
	if input.Availability != nil && !productAvailability(*input.Availability) {
		return input, errManagerInput
	}
	if refs != nil {
		var list []uuid.UUID
		if json.Unmarshal(refs, &list) != nil || list == nil || len(list) > 100 {
			return input, errManagerInput
		}
		slices.SortFunc(list, uuidCompare)
		for index, id := range list {
			if id == uuid.Nil || index > 0 && id == list[index-1] {
				return input, errManagerInput
			}
		}
		input.Integrations = &list
	}
	if tariff != nil {
		input.TariffSet = true
		if json.Unmarshal(tariff, &input.Tariff) != nil || input.Tariff != nil && (*input.Tariff < 1 || *input.Tariff > wire.MaxPosition) {
			return input, errManagerInput
		}
	}
	if trial != nil {
		var policy productTrial
		var days json.RawMessage
		if readManagerObject(trial, map[string]any{"kind": &policy.Kind, "duration_days": &days}) != nil || days == nil || json.Unmarshal(days, &policy.Days) != nil {
			return input, errManagerInput
		}
		switch policy.Kind {
		case "none", "free":
			if policy.Days != nil {
				return input, errManagerInput
			}
		case "once":
			if policy.Days == nil || *policy.Days < 0 {
				return input, errManagerInput
			}
		default:
			return input, errManagerInput
		}
		input.Trial = &policy
	}
	if links != nil {
		var list []json.RawMessage
		if json.Unmarshal(links, &list) != nil || list == nil || len(list) > 20 {
			return input, errManagerInput
		}
		clean := make([]productLink, 0, len(list))
		for _, rawLink := range list {
			var link productLink
			if readManagerObject(rawLink, map[string]any{"label": &link.Label, "url": &link.URL}) != nil {
				return input, errManagerInput
			}
			link.Label = strings.TrimSpace(link.Label)
			parsed, err := url.Parse(link.URL)
			if !managerText(link.Label, 1, 200) || !managerText(link.URL, 1, 2000) || err != nil ||
				(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
				return input, errManagerInput
			}
			clean = append(clean, link)
		}
		input.Links = &clean
	}
	return input, nil
}

func productLine(line string) bool { return line == "FREE" || line == "TOP" || line == "PRO" }
func productAvailability(availability string) bool {
	return availability == "draft" || availability == "pilot" || availability == "active" || availability == "archived"
}

func uuidCompare(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }

func productPolicyRule(line string, trial productTrial) (string, bool) {
	if trial.Kind == "none" && trial.Days == nil {
		return "none", true
	}
	if line == "FREE" && trial.Kind == "free" && trial.Days == nil {
		return "free", true
	}
	if trial.Kind != "once" || trial.Days == nil {
		return "", false
	}
	switch {
	case line == "TOP" && *trial.Days == 90:
		return "top_90d", true
	case line == "PRO" && *trial.Days == 7:
		return "pro_7d", true
	case line == "PRO" && *trial.Days == 14:
		return "pro_14d", true
	default:
		return "", false
	}
}
