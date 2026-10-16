package taskcontrol

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const AssertionAudience = "teamos-task-control"

type Assertion struct {
	Audience       string    `json:"aud"`
	ID             string    `json:"jti"`
	IssuedAt       int64     `json:"iat"`
	ExpiresAt      int64     `json:"exp"`
	CompanyID      uuid.UUID `json:"companyId"`
	InstallationID uuid.UUID `json:"installationId"`
	IntegrationID  uuid.UUID `json:"integrationId"`
	AccountID      string    `json:"accountId"`
	ActorID        string    `json:"actorId"`
	Product        string    `json:"product"`
	Generation     int64     `json:"generation"`
	Admin          bool      `json:"admin"`
	Purpose        string    `json:"purpose"`
	RequestID      uuid.UUID `json:"requestId"`
}

func SignAssertion(keyID string, key ed25519.PrivateKey, a Assertion) (string, error) {
	if len(key) != ed25519.PrivateKeySize || keyID == "" || strings.ContainsAny(keyID, ".\n") {
		return "", errors.New("invalid assertion signing key")
	}
	body, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	payload := keyID + "." + base64.RawURLEncoding.EncodeToString(body)
	sig := ed25519.Sign(key, []byte("teamos-task-control/v1\n"+payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

type CRMIdentity interface {
	DistributionUser(context.Context, uuid.UUID, int64) (amocrm.DistributionUser, error)
}
type HandoffHandler struct {
	Store *Store
	CRM   CRMIdentity
	KeyID string
	Key   ed25519.PrivateKey
	Now   func() time.Time
}

func (h *HandoffHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		failure(w, 401)
		return
	}
	var in struct {
		Purpose   string    `json:"purpose"`
		RequestID uuid.UUID `json:"requestId"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || in.RequestID == uuid.Nil || (in.Purpose != "runtime" && in.Purpose != "link" && in.Purpose != "revoke" && in.Purpose != "mode" && in.Purpose != "adminBridge") {
		failure(w, 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := h.CRM.DistributionUser(ctx, p.InstallationID, p.UserID)
	if err != nil {
		failure(w, 503)
		return
	}
	if user.ID != p.UserID || user.Rights.IsActive == nil || !*user.Rights.IsActive || user.Rights.IsFree || (in.Purpose != "runtime" && !user.Rights.IsAdmin) {
		failure(w, 403)
		return
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now()
	}
	a, err := h.Store.AdmitAssertion(ctx, p, user.Rights.IsAdmin, in.Purpose, in.RequestID, now)
	if errors.Is(err, ErrDenied) || errors.Is(err, widgetauth.ErrReplay) {
		failure(w, 403)
		return
	}
	if err != nil {
		failure(w, 503)
		return
	}
	token, err := SignAssertion(h.KeyID, h.Key, a)
	if err != nil {
		failure(w, 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"assertion": token, "expiresAt": time.Unix(a.ExpiresAt, 0).UTC()})
}
func (s *Store) AdmitAssertion(ctx context.Context, p widgetauth.Principal, admin bool, purpose string, requestID uuid.UUID, now time.Time) (Assertion, error) {
	out := Assertion{}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return out, e
	}
	defer tx.Rollback(ctx)
	// Resolve the company from server grants, never from browser companyId.
	rows, e := tx.Query(ctx, `SELECT company_id FROM task_control_service_grants WHERE account_id=$1 AND integration_id=$2 AND enabled FOR SHARE`, p.AccountID, p.IntegrationID)
	if e != nil {
		return out, e
	}
	companies := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return out, e
		}
		companies[id] = true
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return out, e
	}
	if len(companies) != 1 {
		return out, ErrDenied
	}
	var company uuid.UUID
	for id := range companies {
		company = id
	}
	var state, productState string
	var enabled bool
	e = tx.QueryRow(ctx, `SELECT i.status,p.status,s.enabled FROM installations i JOIN integrations p ON p.id=i.integration_id JOIN integration_services s ON s.integration_id=p.id AND s.service_code='task-control' WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3 FOR SHARE OF i,p,s`, p.InstallationID, p.IntegrationID, p.AccountID).Scan(&state, &productState, &enabled)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, ErrDenied
	}
	if e != nil {
		return out, e
	}
	if state != "active" || productState != "active" || !enabled {
		return out, ErrDenied
	}
	_, e = tx.Exec(ctx, `INSERT INTO task_control_installation_versions(installation_id,last_status) VALUES($1,'active') ON CONFLICT DO NOTHING`, p.InstallationID)
	if e != nil {
		return out, e
	}
	var generation int64
	e = tx.QueryRow(ctx, `SELECT generation FROM task_control_installation_versions WHERE installation_id=$1 AND NOT signal_pending AND evidence<>'disconnect_confirmed' FOR SHARE`, p.InstallationID).Scan(&generation)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, ErrDenied
	}
	if e != nil {
		return out, e
	}
	used := p.UsedToken()
	tag, e := tx.Exec(ctx, `INSERT INTO used_widget_tokens(integration_id,jti,issuer,account_id,user_id,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, used.IntegrationID, used.TokenID, used.Issuer, used.AccountID, used.UserID, used.ExpiresAt)
	if e != nil {
		return out, e
	}
	if tag.RowsAffected() != 1 {
		return out, widgetauth.ErrReplay
	}
	out = Assertion{Audience: AssertionAudience, ID: uuid.NewString(), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CompanyID: company, InstallationID: p.InstallationID, IntegrationID: p.IntegrationID, AccountID: strconv.FormatInt(p.AccountID, 10), ActorID: strconv.FormatInt(p.UserID, 10), Product: Product, Generation: generation, Admin: admin, Purpose: purpose, RequestID: requestID}
	return out, tx.Commit(ctx)
}
