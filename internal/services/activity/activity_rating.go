package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Activity owns these formulae. CRM Events supplies facts, never percentages.
func (s *Service) ViewActivityRatings(ctx context.Context, req serviceapi.ActivityRatingRequest) (serviceapi.ActivityRatings, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceapi.ActivityRatingTimeout)
	defer cancel()
	if err := serviceapi.ValidateActivityRatingRequest(req); err != nil {
		return serviceapi.ActivityRatings{}, err
	}
	_, panel, users, err := s.viewerContext(ctx, req.Auth, req.EmployeeIDs)
	if err != nil {
		return serviceapi.ActivityRatings{}, err
	}
	loc, err := time.LoadLocation(panel.Timezone)
	if err != nil {
		return serviceapi.ActivityRatings{}, serviceapi.Fail(serviceapi.Unavailable, "account timezone unavailable")
	}
	day, _ := time.ParseInLocation("2006-01-02", req.Day, loc)
	if req.From < day.Unix() || req.To > day.AddDate(0, 0, 1).Unix() {
		return serviceapi.ActivityRatings{}, serviceapi.Fail(serviceapi.InvalidArgument, "activity rating period must belong to the selected account day")
	}
	ids := req.EmployeeIDs
	if len(ids) == 0 {
		ids = panel.EmployeeIDs
	}
	// Exactly the existing UI's ascending 60 x 100 page selection. No per-user
	// scan, external amoCRM fetch, or silent aggregate of an unbounded account.
	q := serviceapi.Query{Auth: req.Auth, From: req.From, To: req.To, UserIDs: ids, Limit: 100}
	observations := make([]ratingObservation, 0)
	candidates := make([]ratingCardCandidate, 0, 150)
	partial := false
	seen := map[string]bool{}
	for page := 0; page < serviceapi.ActivityRatingMaxPages; page++ {
		data, err := s.viewerQuery(ctx, q, panel, users, req.Surface == "dashboard")
		if err != nil {
			return serviceapi.ActivityRatings{}, err
		}
		for _, event := range data.Events {
			normalized := ratingEventFromAPI(event)
			if event.CallFacts == nil && normalized.Duration == nil && (event.Type == "incoming_call" || event.Type == "outgoing_call") && len(candidates) < 150 {
				candidates = append(candidates, ratingCardCandidate{ID: event.ID, Index: len(observations)})
			}
			observations = append(observations, ratingObservation{Event: normalized, EmployeeID: event.CreatedBy, Visible: req.Surface != "dashboard" || ratingPipelineMatch(event, panel.Options.PipelineIDs)})
		}
		if data.NextCursor == "" {
			break
		}
		if seen[data.NextCursor] {
			return serviceapi.ActivityRatings{}, serviceapi.Fail(serviceapi.Unavailable, "repeated activity rating event cursor")
		}
		seen[data.NextCursor] = true
		q.Cursor = data.NextCursor
		if page == serviceapi.ActivityRatingMaxPages-1 {
			partial = true
		}
	}
	// Match the old UI order: finish the bounded page scan first, then resolve
	// at most150 missing cards with four workers, before the pipeline filter.
	if reader, ok := s.events.(serviceapi.EventReader); ok {
		if err := resolveRatingCards(ctx, reader, req.Auth, observations, candidates); err != nil {
			return serviceapi.ActivityRatings{}, err
		}
	}
	byID := map[int64][]ratingEvent{}
	for _, observation := range observations {
		if observation.Visible {
			byID[observation.EmployeeID] = append(byID[observation.EmployeeID], observation.Event)
		}
	}
	out := serviceapi.ActivityRatings{FormulaVersion: serviceapi.DashboardActivityFormula, Day: req.Day, Timezone: panel.Timezone, StartMinutes: req.StartMinutes, EndMinutes: req.EndMinutes, CutoffUnix: req.CutoffUnix, SnapshotAt: time.Now().Unix(), Partial: partial, Employees: []serviceapi.EmployeeActivityRating{}}
	if req.Surface == "profile" {
		out.FormulaVersion = serviceapi.ProfileActivityFormula
	}
	for _, id := range ids {
		row := serviceapi.EmployeeActivityRating{EmployeeID: id}
		if req.Surface == "dashboard" {
			row.Percent = dashboardRating(byID[id], req.Day, loc, req.StartMinutes, req.EndMinutes, req.CutoffUnix)
		} else {
			row.MinutePercentages = profileRatingSeries(byID[id], req.Day, loc, req.StartMinutes, req.EndMinutes)
			minute := 1440
			if req.CutoffUnix != nil {
				cut := time.Unix(*req.CutoffUnix, 0).In(loc)
				if cut.Format("2006-01-02") == req.Day {
					minute = cut.Hour()*60 + cut.Minute()
				}
			}
			row.Percent = row.MinutePercentages[minute]
		}
		out.Employees = append(out.Employees, row)
	}
	if err := serviceapi.ValidateResponseSize(out); err != nil {
		return serviceapi.ActivityRatings{}, err
	}
	return out, nil
}

type ratingObservation struct {
	Event      ratingEvent
	EmployeeID int64
	Visible    bool
}
type ratingCardCandidate struct {
	ID    string
	Index int
}

func resolveRatingCards(ctx context.Context, reader serviceapi.EventReader, auth serviceapi.Auth, observations []ratingObservation, candidates []ratingCardCandidate) error {
	var next atomic.Int64
	var stopped atomic.Bool
	var workers sync.WaitGroup
	for worker := 0; worker < min(4, len(candidates)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if stopped.Load() || ctx.Err() != nil {
					return
				}
				index := int(next.Add(1) - 1)
				if index >= len(candidates) {
					return
				}
				candidate := candidates[index]
				card, err := reader.GetEvent(ctx, serviceapi.EventRequest{Auth: auth, EventID: candidate.ID})
				if err == nil {
					// Keep original observation type/timestamp, just as resolveCallFacts.
					observations[candidate.Index].Event.Duration = ratingEventFromAPI(card).Duration
				} else if serviceapi.ErrorCode(err) == serviceapi.ResourceExhausted {
					stopped.Store(true)
				}
			}
		}()
	}
	workers.Wait()
	return ctx.Err()
}

type ratingEvent struct {
	At       int64
	Type     string
	Duration *float64
}

func ratingEventFromAPI(e serviceapi.Event) ratingEvent {
	out := ratingEvent{At: e.CreatedAt, Type: e.Type}
	// mapEvent in React attaches a call only for these two exact types.
	if e.Type != "incoming_call" && e.Type != "outgoing_call" {
		return out
	}
	if e.CallFacts != nil {
		if e.CallFacts.Duration != nil && *e.CallFacts.Duration >= 0 && *e.CallFacts.Duration <= 9007199254740991 {
			v := float64(*e.CallFacts.Duration)
			out.Duration = &v
		}
		return out
	}
	// Legacy payload-only rows use the same projection precedence as mapEvent.
	for _, item := range e.Enrichment {
		if item.ObjectKind != "note" || item.State != "ready" {
			continue
		}
		var p struct {
			Facts map[string]any `json:"facts"`
		}
		if json.Unmarshal(item.Payload, &p) != nil || p.Facts == nil {
			continue
		}
		if _, ok := p.Facts["duration"]; ok {
			out.Duration = ratingDuration(p.Facts["duration"])
			return out
		}
		if _, ok := p.Facts["phone"]; ok {
			return out
		}
	}
	var raw any
	if json.Unmarshal(e.ValueAfter, &raw) == nil {
		if values, ok := raw.([]any); ok {
			if len(values) > 0 {
				raw = values[0]
			} else {
				raw = nil
			}
		}
		if first, ok := raw.(map[string]any); ok {
			if note, ok := first["note"].(map[string]any); ok {
				params := note
				if p, ok := note["params"].(map[string]any); ok {
					params = p
				}
				out.Duration = ratingDuration(params["duration"])
			}
		}
	}
	return out
}
func ratingDuration(v any) *float64 {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		var err error
		text := strings.TrimSpace(x)
		n, err = strconv.ParseFloat(text, 64)
		if err != nil {
			// JavaScript Number accepts unsigned0x/0o/0b literals in legacy
			// string payloads. Canonical projected facts are already integers.
			base := 0
			if len(text) > 2 && text[0] == '0' {
				switch text[1] {
				case 'x', 'X':
					base = 16
				case 'o', 'O':
					base = 8
				case 'b', 'B':
					base = 2
				}
			}
			if base == 0 {
				return nil
			}
			integer, parseErr := strconv.ParseUint(text[2:], base, 64)
			if parseErr != nil {
				return nil
			}
			n = float64(integer)
		}
	default:
		return nil
	}
	if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n > 9007199254740991 {
		return nil
	}
	return &n
}
func roundedRating(active, total float64) float64 {
	if total <= 0 {
		return 0
	}
	// Keep the JavaScript evaluation order. Folding these multiplications into
	// *1000 changes half-tie rounding for representable inputs (e.g.138/480).
	percent := (active / total) * 100
	return math.Max(0, math.Min(100, math.Round(percent*10)/10))
}
func dashboardRating(events []ratingEvent, day string, loc *time.Location, start, end int, cutoff *int64) float64 {
	rangeEnd := end
	if cutoff != nil {
		now := time.Unix(*cutoff, 0).In(loc)
		hour := min(end/60, now.Hour())
		rangeEnd = hour * 60
		if now.Hour() == hour {
			rangeEnd += now.Minute()
		}
	}
	total := rangeEnd - start
	if total <= 0 {
		return 0
	}
	bins := make([]bool, (total+1)/2)
	for _, e := range events {
		at := time.Unix(e.At, 0).In(loc)
		if at.Format("2006-01-02") != day {
			continue
		}
		m := at.Hour()*60 + at.Minute()
		length := 1.
		if strings.Contains(e.Type, "call") && e.Duration != nil && *e.Duration > 0 {
			length = math.Ceil(*e.Duration / 60)
		}
		for i := range bins {
			left := start + i*2
			right := min(left+2, rangeEnd)
			if m < right && float64(m)+length > float64(left) {
				bins[i] = true
			}
		}
	}
	active := 0
	for _, b := range bins {
		if b {
			active += 2
		}
	}
	return roundedRating(float64(active), float64(total))
}
func profileRatingSeries(events []ratingEvent, day string, loc *time.Location, start, end int) []float64 {
	bins := make([]bool, (end-start+1)/2)
	for _, e := range events {
		at := time.Unix(e.At, 0).In(loc)
		if at.Format("2006-01-02") != day || !ratingProfileType(e.Type) {
			continue
		}
		m := at.Hour()*60 + at.Minute()
		if e.Type != "incoming_call" && e.Type != "outgoing_call" {
			if m >= start && m < end {
				bins[(m-start)/2] = true
			}
			continue
		}
		seconds := 60.
		if e.Duration != nil && *e.Duration > 0 {
			seconds = *e.Duration
		}
		visibleStart := max(m, start)
		visibleEnd := math.Min(float64(m)+seconds/60, float64(end))
		if visibleEnd <= float64(visibleStart) {
			continue
		}
		for i := range bins {
			left := start + i*2
			right := min(left+2, end)
			if float64(left) < visibleEnd && right > visibleStart {
				bins[i] = true
			}
		}
	}
	series := make([]float64, 1441)
	active, total, next := 0, 0, 0
	for minute := 0; minute <= 1440; minute++ {
		for next < len(bins) && min(start+next*2+2, end) <= minute {
			total++
			if bins[next] {
				active++
			}
			next++
		}
		series[minute] = roundedRating(float64(active), float64(total))
	}
	return series
}

// Frozen whitelist of the legacy profile formula (not the newer event category taxonomy).
var ratingProfileKeys = []string{"lead_added", "lead_deleted", "lead_restored", "lead_status_changed", "lead_linked", "lead_unlinked", "contact_added", "contact_deleted", "contact_restored", "contact_linked", "contact_unlinked", "company_added", "company_deleted", "company_restored", "company_linked", "company_unlinked", "customer_added", "customer_deleted", "customer_status_changed", "customer_linked", "customer_unlinked", "task_added", "task_deleted", "task_completed", "task_result_added", "task_type_changed", "task_text_changed", "task_deadline_changed", "incoming_call", "outgoing_call", "incoming_mail", "outgoing_mail", "incoming_chat_message", "outgoing_chat_message", "entity_direct_message", "incoming_sms", "outgoing_sms", "entity_tag_added", "entity_tag_deleted", "entity_linked", "entity_unlinked", "sale_field_changed", "name_field_changed", "ltv_field_changed", "custom_field_value_changed", "entity_responsible_changed", "robot_replied", "intent_identified", "nps_rate_added", "link_followed", "transaction_added", "common_note_added", "common_note_deleted", "attachment_note_added", "targeting_in_note_added", "targeting_out_note_added", "geo_note_added", "service_note_added", "site_visit_note_added", "message_to_cashier_note_added", "key_action_completed", "entity_merged"}

func ratingProfileType(t string) bool {
	if strings.Contains(t, "custom_field") {
		return true
	}
	for _, key := range ratingProfileKeys {
		if strings.Contains(t, key) {
			return true
		}
	}
	return false
}

// Pipeline extraction intentionally mirrors the UI rule: unknown pipeline stays
// visible, either before/after pipeline matching the selection is sufficient.
func ratingPipelineMatch(e serviceapi.Event, selected []int64) bool {
	if len(selected) == 0 {
		return true
	}
	ids := map[string]bool{}
	var walk func(any)
	add := func(v any) {
		if v != nil {
			value := ""
			switch id := v.(type) {
			case string:
				value = id
			case float64:
				// JSON numbers decode as float64. JS String(number) does not use
				// an exponent for positive integer CRM IDs below1e21.
				value = strconv.FormatFloat(id, 'f', -1, 64)
			case bool:
				value = strconv.FormatBool(id)
			default:
				value = fmt.Sprint(v)
			}
			if value != "" {
				ids[value] = true
			}
		}
	}
	walk = func(value any) {
		o, ok := value.(map[string]any)
		if !ok {
			return
		}
		for _, k := range []string{"pipeline_id", "pipelineId", "lead_pipeline_id", "entity_pipeline_id"} {
			add(o[k])
		}
		for _, k := range []string{"pipeline", "status", "lead_status"} {
			if nested, ok := o[k].(map[string]any); ok {
				if k == "pipeline" {
					add(nested["id"])
				}
				add(nested["pipeline_id"])
			}
		}
		if opts, ok := o["options"].(map[string]any); ok {
			if pipeline, ok := opts["pipeline"].(map[string]any); ok {
				add(pipeline["id"])
				add(pipeline["pipeline_id"])
			}
		}
	}
	for _, raw := range []json.RawMessage{e.ValueAfter, e.ValueBefore} {
		var values []any
		if json.Unmarshal(raw, &values) != nil {
			continue
		}
		for _, value := range values {
			walk(value)
			if o, ok := value.(map[string]any); ok {
				for _, k := range []string{"lead_status", "status", "pipeline"} {
					walk(o[k])
				}
			}
		}
	}
	if len(ids) == 0 {
		return true
	}
	for _, id := range selected {
		if ids[strconv.FormatInt(id, 10)] {
			return true
		}
	}
	return false
}
