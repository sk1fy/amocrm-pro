// Package taskcontrol owns only installation evidence, not Task Control rules.
package taskcontrol

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Product = "task-control"

type Status struct {
	CompanyID      uuid.UUID  `json:"companyId"`
	AccountID      string     `json:"accountId"`
	InstallationID *uuid.UUID `json:"installationId,omitempty"`
	IntegrationID  *uuid.UUID `json:"integrationId,omitempty"`
	Product        string     `json:"product"`
	State          string     `json:"state"`
	Revision       int64      `json:"revision,omitempty"`
	Generation     int64      `json:"generation,omitempty"`
	CheckedAt      time.Time  `json:"checkedAt"`
	FreshUntil     time.Time  `json:"freshUntil"`
}
type Reader interface {
	Read(context.Context, string, uuid.UUID, int64, uuid.UUID, time.Time) (Status, error)
}
type Store struct{ Pool *pgxpool.Pool }

var ErrDenied = errors.New("task control access denied")

func (s *Store) Read(ctx context.Context, key string, company uuid.UUID, account int64, nonce uuid.UUID, now time.Time) (Status, error) {
	out := Status{CompanyID: company, AccountID: strconv.FormatInt(account, 10), Product: Product, State: "unknown", CheckedAt: now, FreshUntil: now.Add(5 * time.Minute)}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var integration uuid.UUID
	err = tx.QueryRow(ctx, `SELECT integration_id FROM task_control_service_grants WHERE key_id=$1 AND company_id=$2 AND account_id=$3 AND enabled FOR SHARE`, key, company, account).Scan(&integration)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrDenied
	}
	if err != nil {
		return out, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO task_control_service_nonces(key_id,nonce,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, key, nonce, now.Add(2*time.Minute))
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() != 1 {
		return out, ErrDenied
	}
	var install uuid.UUID
	var state, productState string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT i.id,i.status,p.status,svc.enabled FROM integrations p JOIN integration_services svc ON svc.integration_id=p.id AND svc.service_code='task-control' JOIN installations i ON i.integration_id=p.id WHERE p.id=$1 AND i.account_id=$2 FOR SHARE OF i,p,svc`, integration, account).Scan(&install, &state, &productState, &enabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil {
		// Seed when the capability was explicitly added after the installation existed.
		_, err = tx.Exec(ctx, `INSERT INTO task_control_installation_versions(installation_id,last_status) VALUES($1,$2) ON CONFLICT DO NOTHING`, install, state)
		if err != nil {
			return out, err
		}
		err = tx.QueryRow(ctx, `SELECT revision,generation FROM task_control_installation_versions WHERE installation_id=$1`, install).Scan(&out.Revision, &out.Generation)
		if err != nil {
			return out, err
		}
		out.InstallationID = &install
		out.IntegrationID = &integration
		if state == "uninstalled" {
			out.State = "notInstalled"
		} else if state == "active" && productState == "active" && enabled {
			out.State = "installed"
		}
	}
	// Bounded cleanup: replay rows outlive the entire signature window.
	_, err = tx.Exec(ctx, `DELETE FROM task_control_service_nonces WHERE (key_id,nonce) IN (SELECT key_id,nonce FROM task_control_service_nonces WHERE expires_at<$1 LIMIT 100)`, now)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func Signature(secret, key, method, uri, company, account, stamp, nonce string) string {
	body := sha256.Sum256(nil)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strings.Join([]string{key, method, uri, company, account, Product, stamp, nonce, hex.EncodeToString(body[:])}, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

type Handler struct {
	Store Reader
	Keys  map[string]string
	Now   func() time.Time
}

func single(r *http.Request, k string) string {
	v := r.Header.Values("X-Task-Control-" + k)
	if len(v) != 1 {
		return ""
	}
	return v[0]
}
func failure(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"status": status, "message": "Не удалось подтвердить установку Контроля задач"}})
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/internal/v1/task-control/accounts/"
	if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/installation") {
		http.NotFound(w, r)
		return
	}
	account := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/installation")
	id, e := strconv.ParseInt(account, 10, 64)
	key := single(r, "Key-Id")
	company, ce := uuid.Parse(single(r, "Company"))
	nonce, ne := uuid.Parse(single(r, "Nonce"))
	stamp := single(r, "Timestamp")
	unix, se := strconv.ParseInt(stamp, 10, 64)
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now()
	}
	signature, he := hex.DecodeString(single(r, "Signature"))
	expected, _ := hex.DecodeString(Signature(h.Keys[key], key, r.Method, r.URL.RequestURI(), company.String(), account, stamp, nonce.String()))
	if e != nil || id <= 0 || strconv.FormatInt(id, 10) != account || ce != nil || company == uuid.Nil || ne != nil || nonce == uuid.Nil || se != nil || he != nil || len(h.Keys[key]) < 32 || single(r, "Account") != account || single(r, "Product") != Product || r.URL.RawQuery != "" || r.ContentLength > 0 || now.Sub(time.Unix(unix, 0)) > 60*time.Second || time.Unix(unix, 0).Sub(now) > 60*time.Second || !hmac.Equal(signature, expected) {
		failure(w, 401)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out, err := h.Store.Read(ctx, key, company, id, nonce, now)
	if errors.Is(err, ErrDenied) {
		failure(w, 403)
		return
	}
	if err != nil {
		failure(w, 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
