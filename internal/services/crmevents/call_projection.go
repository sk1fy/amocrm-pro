package crmevents

import (
	"encoding/json"
	"strconv"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func projectEmbeddedCall(event serviceapi.Event) *serviceapi.CallFacts {
	if event.Type != "incoming_call" && event.Type != "outgoing_call" {
		return nil
	}
	for _, note := range nestedObjectsFromEvent(event, "note") {
		projection := projectCallFacts(event.Type, noteParams(note), serviceapi.SourceEventPayload)
		if projection.Duration != nil || projection.Phone != nil || projection.Result != nil {
			return projection
		}
	}
	return projectCallFacts(event.Type, nil, "")
}

func projectCallFacts(eventType string, raw json.RawMessage, source string) *serviceapi.CallFacts {
	projection := &serviceapi.CallFacts{Direction: "incoming", Source: source, Current: source != "" && source != serviceapi.SourceEventPayload}
	if eventType == "outgoing_call" {
		projection.Direction = "outgoing"
	}
	fields := jsonObject(raw)
	if n, ok := jsonInt(fields["duration"]); ok && n >= 0 {
		projection.Duration = &n
	}
	projection.Phone = callScalar(fields["phone"])
	projection.Result = callScalar(fields["result"])
	return projection
}

func callScalar(raw json.RawMessage) *string {
	var value string
	if json.Unmarshal(raw, &value) == nil && value != "" {
		return &value
	}
	if n, ok := jsonInt(raw); ok {
		value = strconv.FormatInt(n, 10)
		return &value
	}
	return nil
}
