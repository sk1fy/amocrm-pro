package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type portraitFieldKind uint8

const (
	portraitUUIDField portraitFieldKind = iota + 1
	portraitTextField
	portraitRatingField
	portraitTextArrayField
	portraitManualField
	portraitTopicField
)

type portraitField struct {
	Kind       portraitFieldKind
	Expression string
}

var portraitManualSignalIDs = [...]uuid.UUID{
	portraitFieldID("maturity/sales_pipeline"),
	portraitFieldID("maturity/incoming_leads"),
	portraitFieldID("maturity/messengers"),
	portraitFieldID("maturity/telephony"),
	portraitFieldID("maturity/tasks"),
	portraitFieldID("maturity/deals_have_tasks"),
	portraitFieldID("maturity/automation"),
	portraitFieldID("maturity/management_reports"),
	portraitFieldID("maturity/quality_control"),
	portraitFieldID("maturity/duplicate_control"),
	portraitFieldID("maturity/training"),
	portraitFieldID("maturity/invoices"),
	portraitFieldID("maturity/acquiring"),
}

func portraitFieldID(key string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://rakurscrm.ru/platform/portrait/"+key))
}

var portraitFields = func() map[uuid.UUID]portraitField {
	fields := map[uuid.UUID]portraitField{
		portraitFieldID("niche_id"):       {portraitUUIDField, "pf.niche_id"},
		portraitFieldID("site"):           {portraitTextField, "pf.site"},
		portraitFieldID("rating"):         {portraitRatingField, "pf.rating"},
		portraitFieldID("work_types"):     {portraitTextArrayField, "pf.work_types"},
		portraitFieldID("customer_types"): {portraitTextArrayField, "pf.customer_types"},
	}
	for _, id := range portraitManualSignalIDs {
		fields[id] = portraitField{Kind: portraitManualField}
	}
	return fields
}()

func portraitFixedField(id uuid.UUID) portraitField {
	return portraitFields[id]
}

func isPortraitManualSignal(id uuid.UUID) bool {
	return portraitFixedField(id).Kind == portraitManualField
}

type portraitFilter struct {
	Condition *portraitPredicate `json:"condition,omitempty"`
	And       []portraitFilter   `json:"and,omitempty"`
	Or        []portraitFilter   `json:"or,omitempty"`
}

type portraitPredicate struct {
	FieldID uuid.UUID `json:"field_id"`
	Op      string    `json:"op"`
	Value   any       `json:"value"`
}

func (m managerHTTP) searchPortraitAccounts(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	f, rawCursor, err := parseAccountFilters(r.URL.RawQuery)
	raw, readErr := readPortraitBody(w, r)
	filter, filterErr := parsePortraitFilterInput(raw)
	if err != nil || readErr != nil || filterErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	f.Portrait = &filter
	var after accountSeek
	hash := f.hash()
	if rawCursor != "" {
		after, err = m.options.parseAccountCursor(rawCursor, env.Actor, hash)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.listAccounts(r.Context(), env.Actor, f, after)
	if err != nil {
		if errors.Is(err, errManagerInput) {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		} else {
			writeManagerStoreError(w, env.RequestID, err)
		}
		return
	}
	if next.Snapshot != uuid.Nil {
		cursor := m.options.accountCursor(next, env.Actor, hash)
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func parsePortraitFilterInput(raw []byte) (portraitFilter, error) {
	fields, err := portraitObject(raw, "portrait_filter")
	if err != nil {
		return portraitFilter{}, err
	}
	conditions := 30
	return parsePortraitFilterNode(fields["portrait_filter"], 1, &conditions)
}

func parsePortraitFilterNode(raw []byte, depth int, left *int) (portraitFilter, error) {
	var node portraitFilter
	if depth > 3 {
		return node, errManagerInput
	}
	fields, err := portraitObject(raw, "condition", "and", "or")
	if err != nil || len(fields) != 1 {
		return node, errManagerInput
	}
	if rawCondition := fields["condition"]; rawCondition != nil {
		if *left <= 0 {
			return node, errManagerInput
		}
		*left = *left - 1
		condition, err := parsePortraitPredicate(rawCondition)
		node.Condition = &condition
		return node, err
	}
	child := func(raw []byte) (portraitFilter, error) {
		return parsePortraitFilterNode(raw, depth+1, left)
	}
	if children := fields["and"]; children != nil {
		node.And, err = parsePortraitList(children, child, 30)
		if len(node.And) == 0 {
			err = errManagerInput
		}
	} else {
		node.Or, err = parsePortraitList(fields["or"], child, 30)
		if len(node.Or) == 0 {
			err = errManagerInput
		}
	}
	return node, err
}

func parsePortraitPredicate(raw []byte) (portraitPredicate, error) {
	var predicate portraitPredicate
	fields, err := portraitObject(raw, "field_id", "op", "value")
	if err != nil {
		return predicate, err
	}
	predicate.FieldID, err = portraitMemberUUID(fields, "field_id")
	if err != nil {
		return predicate, err
	}
	ok, err := portraitMember(fields, "op", &predicate.Op, false)
	if err != nil || !ok {
		return predicate, errManagerInput
	}
	kind := portraitFixedField(predicate.FieldID).Kind
	if kind == 0 {
		kind = portraitTopicField
	}
	switch predicate.Op {
	case "exists":
		exists := true
		if _, err := portraitMember(fields, "value", &exists, false); err != nil {
			return predicate, err
		}
		predicate.Value = exists
		return predicate, nil
	case "lt", "gte":
		if kind != portraitRatingField {
			return predicate, errManagerInput
		}
	case "eq", "in":
	default:
		return predicate, errManagerInput
	}
	predicate.Value, err = parsePortraitFilterValue(kind, predicate.Op == "in", fields["value"])
	return predicate, err
}

func parsePortraitFilterValue(kind portraitFieldKind, list bool, raw json.RawMessage) (any, error) {
	switch kind {
	case portraitUUIDField:
		return portraitOperand(list, raw, func(value string) (uuid.UUID, error) { return managerUUID(value) })
	case portraitRatingField:
		return portraitOperand(list, raw, func(value int32) (int32, error) { return value, nil })
	case portraitManualField:
		return portraitOperand(list, raw, func(value bool) (bool, error) { return value, nil })
	default:
		return portraitOperand(list, raw, func(value string) (string, error) {
			if !managerText(value, 0, 4000) {
				return "", errManagerInput
			}
			return value, nil
		})
	}
}

func portraitOperand[T, U any](list bool, raw json.RawMessage, validate func(T) (U, error)) (any, error) {
	decode := func(raw []byte) (U, error) {
		var input T
		var out U
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &input) != nil {
			return out, errManagerInput
		}
		return validate(input)
	}
	if list {
		values, err := parsePortraitList(raw, decode, 30)
		if len(values) == 0 {
			return nil, errManagerInput
		}
		return values, err
	}
	return decode(raw)
}

type portraitFilterSQL struct {
	first  int
	args   []any
	topics map[uuid.UUID]bool
}

func portraitAccountListPredicate(ctx context.Context, tx pgx.Tx, f *portraitFilter, firstParameter int) ([]any, string, error) {
	if f == nil {
		return nil, "", nil
	}
	builder := portraitFilterSQL{first: firstParameter}
	if f.referencesTopics() {
		template, err := readPortraitTemplate(ctx, tx, "")
		if err != nil {
			return nil, "", err
		}
		builder.topics = make(map[uuid.UUID]bool, len(template.Topics))
		for _, topic := range template.Topics {
			builder.topics[topic.ID] = true
		}
	}
	sql, err := builder.node(*f)
	return builder.args, " AND (" + sql + ")", err
}

func (f portraitFilter) referencesTopics() bool {
	if f.Condition != nil {
		return portraitFixedField(f.Condition.FieldID).Kind == 0
	}
	return slices.ContainsFunc(f.And, portraitFilter.referencesTopics) || slices.ContainsFunc(f.Or, portraitFilter.referencesTopics)
}

func (b *portraitFilterSQL) parameter(value any) string {
	index := b.first + len(b.args)
	b.args = append(b.args, value)
	return fmt.Sprintf("$%d", index)
}

func (b *portraitFilterSQL) node(f portraitFilter) (string, error) {
	if f.Condition != nil {
		return b.predicate(*f.Condition)
	}
	children, join := f.And, " AND "
	if len(children) == 0 {
		children, join = f.Or, " OR "
	}
	expressions := make([]string, 0, len(children))
	for _, child := range children {
		expr, err := b.node(child)
		if err != nil {
			return "", err
		}
		expressions = append(expressions, expr)
	}
	return "(" + strings.Join(expressions, join) + ")", nil
}

func (b *portraitFilterSQL) predicate(p portraitPredicate) (string, error) {
	field := portraitFixedField(p.FieldID)
	switch field.Kind {
	case portraitManualField:
		field.Expression = "((pf.maturity ->> " + b.parameter(p.FieldID.String()) + "::text)::boolean)"
	case 0:
		if !b.topics[p.FieldID] {
			return "", errManagerInput
		}
		field = portraitField{Kind: portraitTopicField, Expression: "(pf.topic_notes ->> " + b.parameter(p.FieldID.String()) + "::text)"}
	}
	if p.Op == "exists" {
		if p.Value.(bool) {
			return field.Expression + " IS NOT NULL", nil
		}
		return field.Expression + " IS NULL", nil
	}
	arg := b.parameter(p.Value)
	switch p.Op {
	case "eq":
		if field.Kind == portraitTextArrayField {
			return arg + " = ANY(" + field.Expression + ")", nil
		}
		return field.Expression + " = " + arg, nil
	case "in":
		if field.Kind == portraitTextArrayField {
			return field.Expression + " && " + arg + "::text[]", nil
		}
		return field.Expression + " = ANY(" + arg + ")", nil
	case "lt":
		return field.Expression + " < " + arg, nil
	default:
		return field.Expression + " >= " + arg, nil
	}
}
