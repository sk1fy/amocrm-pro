// Package admincommand owns durable admin command receipts. Domain mutations
// remain in their existing application packages; external I/O runs in worker.
package admincommand

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
)

const CheckJobType = "admin.connection_check"
const UninstallJobType = "admin.webhook_unregister"

type Request struct {
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Command    string          `json:"command"`
	Payload    json.RawMessage `json:"payload"`
}

type Receipt struct {
	ID         uuid.UUID       `json:"id"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Command    string          `json:"command"`
	State      string          `json:"state"`
	Outcome    string          `json:"outcome,omitempty"`
	Result     json.RawMessage `json:"result"`
	Error      *Error          `json:"error,omitempty"`
	ObservedAt time.Time       `json:"observed_at"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	JobID      *uuid.UUID      `json:"job_id,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func invalid(message string) *Error {
	return &Error{Code: "invalid_argument", Message: message, Status: http.StatusBadRequest}
}
func conflict(message string) *Error {
	return &Error{Code: "conflict", Message: message, Status: http.StatusConflict}
}
func notFound() *Error {
	return &Error{Code: "not_found", Message: "target not found", Status: http.StatusNotFound}
}

type commandPayload struct {
	InstallationID string    `json:"installation_id"`
	Code           string    `json:"code"`
	ClientID       string    `json:"client_id"`
	ClientSecret   string    `json:"client_secret"`
	RedirectURI    *string   `json:"redirect_uri"`
	WebhookEvents  *[]string `json:"webhook_events"`
	Services       []string  `json:"services"`
	Service        string    `json:"service"`
	Enabled        *bool     `json:"enabled"`
}

func ReceiptID(key string) uuid.UUID {
	if id, err := uuid.Parse(key); err == nil && id != uuid.Nil {
		return id
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
}

func normalize(req Request, key, actor string) (Request, commandPayload, [32]byte, [32]byte, error) {
	var p commandPayload
	if len(key) == 0 || len(key) > 200 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\t") {
		return req, p, [32]byte{}, [32]byte{}, invalid("invalid idempotency key")
	}
	if len(actor) == 0 || len(actor) > 128 || strings.TrimSpace(actor) != actor || strings.ContainsAny(actor, "\r\n\t") {
		return req, p, [32]byte{}, [32]byte{}, invalid("invalid actor")
	}
	if id, err := uuid.Parse(key); err == nil && id != uuid.Nil {
		key = id.String()
	}
	if req.TargetType == "integration" && req.Command == "create" {
		if req.TargetID != "new" {
			return req, p, [32]byte{}, [32]byte{}, invalid("create target must be new")
		}
	} else {
		id, err := uuid.Parse(req.TargetID)
		if err != nil || id == uuid.Nil {
			return req, p, [32]byte{}, [32]byte{}, invalid("invalid target identifier")
		}
		req.TargetID = id.String()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &fields); err != nil || fields == nil {
		return req, p, [32]byte{}, [32]byte{}, invalid("payload must be an object")
	}
	allowed := map[string]bool{}
	allow := func(names ...string) {
		for _, name := range names {
			allowed[name] = true
		}
	}
	switch req.TargetType {
	case "installation":
		switch req.Command {
		case "enable", "disable", "revoke", "uninstall", "reconcile", "check", "pilot-enable", "pilot-disable":
		default:
			return req, p, [32]byte{}, [32]byte{}, invalid("unsupported installation command")
		}
	case "integration":
		switch req.Command {
		case "create":
			allow("code", "client_id", "client_secret", "redirect_uri", "webhook_events", "services")
		case "update":
			allow("redirect_uri", "webhook_events")
		case "rotate-secret":
			allow("client_secret")
		case "set-service":
			allow("service", "enabled")
		case "enable", "disable":
		default:
			return req, p, [32]byte{}, [32]byte{}, invalid("unsupported integration command")
		}
	case "job", "delivery":
		if req.TargetType == "delivery" {
			allow("installation_id")
		}
		if req.Command != "retry" {
			return req, p, [32]byte{}, [32]byte{}, invalid("unsupported retry command")
		}
	default:
		return req, p, [32]byte{}, [32]byte{}, invalid("unsupported target type")
	}
	for name := range fields {
		if !allowed[name] {
			return req, p, [32]byte{}, [32]byte{}, invalid("unexpected command payload field")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return req, p, [32]byte{}, [32]byte{}, invalid("invalid command payload")
	}
	if req.Command == "set-service" && p.Enabled == nil {
		return req, p, [32]byte{}, [32]byte{}, invalid("set-service requires enabled")
	}
	if req.TargetType == "delivery" {
		id, err := uuid.Parse(p.InstallationID)
		if err != nil || id == uuid.Nil {
			return req, p, [32]byte{}, [32]byte{}, invalid("delivery retry requires installation_id")
		}
		p.InstallationID = id.String()
	}
	if req.TargetType == "integration" {
		code := p.Code
		if req.Command != "create" {
			code = "validated-target"
		}
		c := integrationCommand(req, p, actor, code)
		defer clear(c.Secret)
		if err := c.Validate(); err != nil {
			return req, p, [32]byte{}, [32]byte{}, invalid(err.Error())
		}
	}
	canonical, _ := json.Marshal(fields)
	// Raw field values are canonicalized too, so whitespace/key order do not
	// change replay identity. Only validated strings/booleans/arrays are accepted.
	var value any
	_ = json.Unmarshal(canonical, &value)
	req.Payload, _ = json.Marshal(value)
	body, _ := json.Marshal(req)
	defer clear(body)
	return req, p, sha256.Sum256([]byte(key)), sha256.Sum256(body), nil
}

func integrationCommand(req Request, p commandPayload, actor, code string) integrations.Command {
	action := req.Command
	if req.TargetType == "installation" && (action == "enable" || action == "disable") {
		action += "-installation"
	}
	c := integrations.Command{Action: action, Actor: actor, ActorType: "admin", Code: code, ClientID: p.ClientID,
		Secret: []byte(p.ClientSecret), RedirectURI: p.RedirectURI, WebhookEvents: p.WebhookEvents, Services: p.Services, Service: p.Service}
	if p.Enabled != nil {
		c.Enabled = *p.Enabled
	}
	if req.TargetType == "installation" {
		c.InstallationID, _ = uuid.Parse(req.TargetID)
	}
	return c
}

func decodeRequest(reader io.Reader) (Request, error) {
	var req Request
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return req, invalid("invalid command request")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return req, invalid("request must contain one object")
	}
	return req, nil
}
