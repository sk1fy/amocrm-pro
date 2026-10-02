package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

type DeliveryReceipt struct {
	MessageID   uuid.UUID `json:"messageId"`
	ReceiptID   uuid.UUID `json:"receiptId"`
	Disposition string    `json:"disposition"`
	AcceptedAt  time.Time `json:"acceptedAt"`
}
type DeliveryWorker struct {
	Store      *Store
	URL, KeyID string
	Keys       map[string]string
	HTTP       *http.Client
	OnError    func(string)
}
type outboundMessage struct {
	ID      uuid.UUID
	Token   uuid.UUID
	Payload json.RawMessage
	Scope   AssignmentScope
	Attempt int
	Kind    string
}

func (w *DeliveryWorker) claim(ctx context.Context, kind string) (outboundMessage, error) {
	m := outboundMessage{Token: uuid.New(), Kind: kind}
	table := "distribution_event_outbox"
	if kind == "results" {
		table = "distribution_result_outbox"
	}
	// Table names are an internal two-element whitelist; no caller SQL is used.
	e := w.Store.pool.QueryRow(ctx, `WITH picked AS(SELECT ctid FROM `+table+` WHERE (state='pending' AND next_attempt_at<=clock_timestamp()) OR (state='delivering' AND lease_until<=clock_timestamp()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE `+table+` o SET state='delivering',lease_token=$1,lease_until=clock_timestamp()+interval '30 seconds',attempts=attempts+1 FROM picked WHERE o.ctid=picked.ctid RETURNING o.payload,o.attempts`, m.Token).Scan(&m.Payload, &m.Attempt)
	if e != nil {
		return m, e
	}
	var env struct {
		MessageID uuid.UUID       `json:"messageId"`
		Scope     AssignmentScope `json:"scope"`
	}
	if json.Unmarshal(m.Payload, &env) != nil || env.MessageID == uuid.Nil || env.Scope.CompanyID == uuid.Nil || env.Scope.InstallationID == uuid.Nil || env.Scope.BindingID == uuid.Nil || env.Scope.IntegrationID == uuid.Nil || env.Scope.AccountID <= 0 || !validRevision(env.Scope.BindingRevision) || len(m.Payload) > MaxBody {
		return m, errors.New("invalid durable envelope")
	}
	m.ID, m.Scope = env.MessageID, env.Scope
	return m, nil
}
func (w *DeliveryWorker) settle(ctx context.Context, m outboundMessage, receipt *DeliveryReceipt, code string, blocked bool) error {
	table := "distribution_event_outbox"
	if m.Kind == "results" {
		table = "distribution_result_outbox"
	}
	state := "pending"
	var ackAt *time.Time
	var ackID *uuid.UUID
	if receipt != nil {
		state = "acknowledged"
		now := time.Now().UTC()
		ackAt = &now
		ackID = &receipt.ReceiptID
		code = ""
	} else if blocked || m.Attempt >= 20 {
		state = "blocked"
	}
	delay := time.Second * time.Duration(1<<min(m.Attempt, 10))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	tag, e := w.Store.pool.Exec(ctx, `UPDATE `+table+` SET state=$2,lease_token=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()+$3::interval,acknowledged_at=$4,ack_receipt_id=$5,error_code=NULLIF($6,'') WHERE lease_token=$1 AND state='delivering' AND lease_until>clock_timestamp()`, m.Token, state, delay.String(), ackAt, ackID, code)
	if e == nil && tag.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	return e
}
func (w *DeliveryWorker) deliver(ctx context.Context, m outboundMessage) (*DeliveryReceipt, string, bool) {
	u, e := url.Parse(w.URL + "/internal/v1/distribution/" + m.Kind)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || w.Keys[w.KeyID] == "" {
		return nil, "delivery_configuration", true
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(call, "POST", u.String(), bytes.NewReader(m.Payload))
	if e != nil {
		return nil, "delivery_configuration", true
	}
	req.Header.Set("Content-Type", "application/json")
	Sign(req, Scope{KeyID: w.KeyID, CompanyID: m.Scope.CompanyID, InstallationID: m.Scope.InstallationID}, w.Keys[w.KeyID], m.Payload)
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if w.HTTP != nil {
		*client = *w.HTTP
		client.Timeout = 3 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	response, e := client.Do(req)
	if e != nil {
		return nil, "delivery_unavailable", false
	}
	defer response.Body.Close()
	if response.StatusCode != 202 {
		blocked := response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 429
		return nil, "delivery_http_" + http.StatusText(response.StatusCode), blocked
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 65537))
	if e != nil || len(raw) > 65536 {
		return nil, "invalid_ack", false
	}
	var receipt DeliveryReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil {
		return nil, "invalid_ack", false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || receipt.MessageID != m.ID || receipt.ReceiptID == uuid.Nil || receipt.AcceptedAt.IsZero() || receipt.AcceptedAt.After(time.Now().Add(time.Minute)) || (receipt.Disposition != "accepted" && receipt.Disposition != "duplicate") {
		return nil, "invalid_ack", false
	}
	return &receipt, "", false
}
func (w *DeliveryWorker) Tick(ctx context.Context) error {
	for _, kind := range []string{"events", "results"} {
		for i := 0; i < 20; i++ {
			m, e := w.claim(ctx, kind)
			if errors.Is(e, pgx.ErrNoRows) {
				break
			}
			if e != nil {
				if len(m.Payload) > 0 {
					if err := w.settle(ctx, m, nil, "invalid_envelope", true); err != nil {
						return err
					}
					continue
				}
				return e
			}
			receipt, code, blocked := w.deliver(ctx, m)
			if e = w.settle(ctx, m, receipt, code, blocked); e != nil {
				return e
			}
		}
	}
	return nil
}
func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if e := w.Tick(ctx); e != nil {
			if ctx.Err() != nil {
				return nil
			}
			if w.OnError != nil {
				w.OnError("delivery_tick_failed")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
