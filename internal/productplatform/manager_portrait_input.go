package productplatform

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

func readPortraitBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	media, _, err := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if err != nil || media != "application/json" {
		return nil, errManagerInput
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
}

// Keep nullable values in the raw members until the specific field validates them.
func portraitObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	members := make([]json.RawMessage, len(keys))
	targets := make(map[string]any, len(keys))
	for i, key := range keys {
		targets[key] = &members[i]
	}
	if err := readManagerObject(raw, targets); err != nil {
		return nil, err
	}
	result := make(map[string]json.RawMessage, len(keys))
	for i, key := range keys {
		if members[i] != nil {
			result[key] = members[i]
		}
	}
	return result, nil
}

func parseAccountPortraitPatch(raw []byte) (accountPortraitPatch, error) {
	var p accountPortraitPatch
	fields, err := portraitObject(raw, "company", "maturity_signals", "topics", "people")
	if err != nil {
		return p, err
	}
	if company := fields["company"]; company != nil {
		p.Company, err = parsePortraitCompanyPatch(company)
		if err != nil {
			return p, err
		}
	}
	if list := fields["maturity_signals"]; list != nil {
		p.Maturity, err = parsePortraitList(list, parsePortraitMaturity, 30)
		if err != nil {
			return p, err
		}
		slices.SortFunc(p.Maturity, func(a, b portraitMaturityPatch) int { return bytes.Compare(a.ID[:], b.ID[:]) })
		for i := 1; i < len(p.Maturity); i++ {
			if p.Maturity[i-1].ID == p.Maturity[i].ID {
				return p, errManagerInput
			}
		}
	}
	if list := fields["topics"]; list != nil {
		p.Topics, err = parsePortraitList(list, parsePortraitTopicNote, 100)
		if err != nil {
			return p, err
		}
		slices.SortFunc(p.Topics, func(a, b portraitTopicNote) int { return bytes.Compare(a.ID[:], b.ID[:]) })
		for i := 1; i < len(p.Topics); i++ {
			if p.Topics[i-1].ID == p.Topics[i].ID {
				return p, errManagerInput
			}
		}
	}
	if list := fields["people"]; list != nil {
		people, err := parsePortraitList(list, parsePortraitPerson, 50)
		if err != nil {
			return p, err
		}
		seen := make(map[uuid.UUID]bool, len(people))
		for _, person := range people {
			if seen[person.ID] {
				return p, errManagerInput
			}
			seen[person.ID] = true
		}
		p.People = &people
	}
	return p, nil
}

var portraitPhone = regexp.MustCompile(`^\+?[0-9(). -]*[0-9][0-9(). -]*$`)
var portraitCRMContact = regexp.MustCompile(`^https://rakurscrm\.amocrm\.ru/contacts/detail/[1-9][0-9]{0,18}/?$`)

func parsePortraitPerson(raw []byte) (portraitPerson, error) {
	var p portraitPerson
	fields, err := portraitObject(raw, "id", "name", "phone", "crm_url", "roles")
	if err != nil {
		return p, err
	}
	p.ID, err = portraitMemberUUID(fields, "id")
	if err != nil {
		return p, err
	}
	ok, err := portraitMember(fields, "name", &p.Name, false)
	p.Name = strings.TrimSpace(p.Name)
	if !ok || err != nil || !managerText(p.Name, 1, 160) || strings.ContainsFunc(p.Name, unicode.IsControl) {
		return p, errManagerInput
	}
	ok, err = portraitMember(fields, "phone", &p.Phone, false)
	p.Phone = strings.TrimSpace(p.Phone)
	if err != nil || (ok && (!managerText(p.Phone, 1, 32) || !portraitPhone.MatchString(p.Phone))) {
		return p, errManagerInput
	}
	ok, err = portraitMember(fields, "crm_url", &p.CRMURL, false)
	p.CRMURL = strings.TrimSpace(p.CRMURL)
	if err != nil || (ok && !portraitCRMContact.MatchString(p.CRMURL)) {
		return p, errManagerInput
	}
	ok, err = portraitMember(fields, "roles", &p.Roles, false)
	if !ok || err != nil || len(p.Roles) > 5 {
		return p, errManagerInput
	}
	slices.Sort(p.Roles)
	for i, role := range p.Roles {
		if i > 0 && p.Roles[i-1] == role {
			return p, errManagerInput
		}
		switch role {
		case "decision_maker", "influencer", "billing", "setup", "partner":
		default:
			return p, errManagerInput
		}
	}
	return p, nil
}

func parsePortraitList[T any](raw []byte, parse func([]byte) (T, error), limit int) ([]T, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil || len(items) > limit {
		return nil, errManagerInput
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		value, err := parse(item)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

func parsePortraitCompanyPatch(raw []byte) (portraitCompanyPatch, error) {
	var p portraitCompanyPatch
	fields, err := portraitObject(raw, "niche_id", "site", "rating", "work_types", "customer_types")
	if err != nil {
		return p, err
	}
	var nicheID *string
	p.NicheID.Set, err = portraitMember(fields, "niche_id", &nicheID, true)
	if err != nil {
		return p, err
	}
	if nicheID != nil {
		id, err := managerUUID(*nicheID)
		if err != nil {
			return p, err
		}
		p.NicheID.Value = &id
	}
	p.Site.Set, err = portraitMember(fields, "site", &p.Site.Value, true)
	if err != nil || p.Site.Value != nil && !validPortraitSite(*p.Site.Value) {
		return p, errManagerInput
	}
	p.Rating.Set, err = portraitMember(fields, "rating", &p.Rating.Value, true)
	if err != nil || p.Rating.Value != nil && (*p.Rating.Value < 1 || *p.Rating.Value > 5) {
		return p, errManagerInput
	}
	for key, value := range map[string]*portraitPatchValue[*[]string]{
		"work_types": &p.WorkTypes, "customer_types": &p.CustomerTypes,
	} {
		value.Set, err = portraitMember(fields, key, &value.Value, false)
		if err != nil || value.Set && !validPortraitStrings(*value.Value, 30, 120) {
			return p, errManagerInput
		}
		if value.Set {
			slices.Sort(*value.Value)
			*value.Value = slices.Compact(*value.Value)
		}
	}
	return p, nil
}

func validPortraitSite(site string) bool {
	if !managerText(site, 1, 2048) {
		return false
	}
	parsed, err := url.Parse(site)
	return err == nil && parsed.Hostname() != "" && parsed.User == nil &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}

func validPortraitStrings(values []string, maxItems, maxLength int) bool {
	if values == nil || len(values) > maxItems {
		return false
	}
	for _, s := range values {
		if !managerText(s, 1, maxLength) || strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

func parsePortraitMaturity(raw []byte) (portraitMaturityPatch, error) {
	var signal portraitMaturityPatch
	fields, err := portraitObject(raw, "id", "manual")
	if err != nil {
		return signal, err
	}
	signal.ID, err = portraitMemberUUID(fields, "id")
	if err != nil || !isPortraitManualSignal(signal.ID) {
		return signal, errManagerInput
	}
	ok, err := portraitMember(fields, "manual", &signal.Manual, true)
	if err != nil || !ok {
		return signal, errManagerInput
	}
	return signal, nil
}

func parsePortraitTopicNote(raw []byte) (portraitTopicNote, error) {
	var topic portraitTopicNote
	fields, err := portraitObject(raw, "id", "note")
	if err != nil {
		return topic, err
	}
	topic.ID, err = portraitMemberUUID(fields, "id")
	if err != nil {
		return topic, err
	}
	ok, err := portraitMember(fields, "note", &topic.Note, false)
	if err != nil || !ok || !managerText(topic.Note, 0, 4000) {
		return topic, errManagerInput
	}
	return topic, nil
}

func parsePortraitAuditDate(raw []byte) (portraitAuditInput, error) {
	var input portraitAuditInput
	fields, err := portraitObject(raw, "date", "note")
	if err != nil {
		return input, err
	}
	ok, err := portraitMember(fields, "date", &input.Date, false)
	date, dateErr := time.Parse(time.DateOnly, input.Date)
	if err != nil || !ok || dateErr != nil || date.Year() < 1 || date.Format(time.DateOnly) != input.Date {
		return input, errManagerInput
	}
	_, err = portraitMember(fields, "note", &input.Note, true)
	if err != nil || input.Note != nil && !managerText(*input.Note, 0, 4000) {
		return input, errManagerInput
	}
	return input, nil
}

func portraitMember(fields map[string]json.RawMessage, key string, dst any, nullable bool) (bool, error) {
	raw := fields[key]
	if raw == nil {
		return false, nil
	}
	if !nullable && bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, dst) != nil {
		return true, errManagerInput
	}
	return true, nil
}

func portraitMemberUUID(fields map[string]json.RawMessage, key string) (uuid.UUID, error) {
	var raw string
	ok, err := portraitMember(fields, key, &raw, false)
	if err != nil || !ok {
		return uuid.Nil, errManagerInput
	}
	return managerUUID(raw)
}
