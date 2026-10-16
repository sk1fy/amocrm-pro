package taskcontrol

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/crmreferences"
)

type ReferenceIdentity struct {
	Scope                               crmreferences.Scope
	GrantRevision, InstallationRevision int64
	AccountDomain, ClientID             string
}

func (i ReferenceIdentity) cacheKey() string {
	return fmt.Sprintf("%s/%s/%s/%s/%d/%s/1/%d/%d/%s/%s", i.Scope.CompanyID, i.Scope.AccountID, i.Scope.IntegrationID, i.Scope.InstallationID, i.Scope.Generation, i.Scope.Product, i.GrantRevision, i.InstallationRevision, i.AccountDomain, i.ClientID)
}

type ReferenceAuthorizer interface {
	AuthorizeReferences(context.Context, string, uuid.UUID, int64, *uuid.UUID, time.Time) (ReferenceIdentity, error)
}

// AuthorizeReferences intentionally has no Distribution grant/service dependency.
// A non-nil nonce denotes first admission and is consumed durably exactly once.
func (s *Store) AuthorizeReferences(ctx context.Context, key string, company uuid.UUID, account int64, nonce *uuid.UUID, now time.Time) (ReferenceIdentity, error) {
	var out ReferenceIdentity
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var integration, installation uuid.UUID
	var generation int64
	err = tx.QueryRow(ctx, `SELECT g.integration_id,i.id,v.generation,g.grant_revision,v.revision,i.account_domain,p.client_id::text
 FROM task_control_service_grants g JOIN integrations p ON p.id=g.integration_id
 JOIN integration_services svc ON svc.integration_id=p.id AND svc.service_code='task-control'
 JOIN installations i ON i.integration_id=p.id AND i.account_id=g.account_id
 JOIN task_control_installation_versions v ON v.installation_id=i.id
 WHERE g.key_id=$1 AND g.company_id=$2 AND g.account_id=$3 AND g.enabled AND g.references_read
 AND p.status='active' AND svc.enabled AND i.status='active' AND NOT v.signal_pending AND v.evidence<>'disconnect_confirmed'
 FOR SHARE OF g,p,svc,i,v`, key, company, account).Scan(&integration, &installation, &generation, &out.GrantRevision, &out.InstallationRevision, &out.AccountDomain, &out.ClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrDenied
	}
	if err != nil {
		return out, err
	}
	if nonce != nil {
		tag, e := tx.Exec(ctx, `INSERT INTO task_control_service_nonces(key_id,nonce,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, key, *nonce, now.Add(2*time.Minute))
		if e != nil {
			return out, e
		}
		if tag.RowsAffected() != 1 {
			return out, ErrDenied
		}
		if _, e = tx.Exec(ctx, `DELETE FROM task_control_service_nonces WHERE (key_id,nonce) IN (SELECT key_id,nonce FROM task_control_service_nonces WHERE expires_at<$1 LIMIT 100)`, now); e != nil {
			return out, e
		}
	}
	out.Scope = crmreferences.Scope{CompanyID: company.String(), AccountID: strconv.FormatInt(account, 10), IntegrationID: integration.String(), InstallationID: installation.String(), Generation: generation, Product: Product}
	return out, tx.Commit(ctx)
}

type ReferencesHandler struct {
	Store    ReferenceAuthorizer
	Keys     map[string]string
	Provider *crmreferences.Provider
	CRM      crmreferences.Reader
	Now      func() time.Time
}

func referenceFailure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"status": status, "message": "Не удалось получить справочники CRM", "code": code}})
}
func (h *ReferencesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/internal/task-control/accounts/"
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/references") {
		http.NotFound(w, r)
		return
	}
	account := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/references")
	id, e := strconv.ParseInt(account, 10, 64)
	key := single(r, "Key-Id")
	company, ce := uuid.Parse(single(r, "Company"))
	nonce, ne := uuid.Parse(single(r, "Nonce"))
	stamp := single(r, "Timestamp")
	unix, se := strconv.ParseInt(stamp, 10, 64)
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	sig, he := hex.DecodeString(single(r, "Signature"))
	expected, _ := hex.DecodeString(Signature(h.Keys[key], key, r.Method, r.URL.RequestURI(), company.String(), account, stamp, nonce.String()))
	bodyPresent := r.ContentLength != 0 || len(r.TransferEncoding) > 0
	if !bodyPresent && r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1))
		bodyPresent = err != nil || len(b) != 0
	}
	if e != nil || id <= 0 || id > 9007199254740991 || strconv.FormatInt(id, 10) != account || ce != nil || company == uuid.Nil || ne != nil || nonce == uuid.Nil || se != nil || he != nil || len(h.Keys[key]) < 32 || single(r, "Account") != account || single(r, "Product") != Product || r.URL.RawQuery != "" || r.URL.ForceQuery || bodyPresent || now.Sub(time.Unix(unix, 0)) > 60*time.Second || time.Unix(unix, 0).Sub(now) > 60*time.Second || !hmac.Equal(sig, expected) {
		referenceFailure(w, 401, "invalid_signature")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	identity, err := h.Store.AuthorizeReferences(ctx, key, company, id, &nonce, now)
	if err != nil {
		h.fail(w, err)
		return
	}
	fence := func(ctx context.Context) error {
		current, err := h.Store.AuthorizeReferences(ctx, key, company, id, nil, now)
		if err != nil {
			return err
		}
		if current != identity {
			return ErrDenied
		}
		return nil
	}
	out, err := h.Provider.Get(ctx, identity.cacheKey(), identity.Scope, h.CRM, fence)
	if err != nil {
		h.fail(w, err)
		return
	}
	payload, err := json.Marshal(out)
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(payload) > crmreferences.MaxBytes {
		h.fail(w, crmreferences.ErrTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(payload)
}
func (h *ReferencesHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDenied):
		referenceFailure(w, 403, "references_denied")
	case errors.Is(err, crmreferences.ErrTooLarge):
		referenceFailure(w, 413, "references_too_large")
	case errors.Is(err, crmreferences.ErrIncomplete):
		referenceFailure(w, 503, "references_incomplete")
	default:
		referenceFailure(w, 503, "references_unavailable")
	}
}
