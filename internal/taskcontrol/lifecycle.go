package taskcontrol

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
)

type LifecycleCRM interface {
	CheckTaskControlAuthorization(context.Context, uuid.UUID, int64) error
}
type Lifecycle struct {
	Store *Store
	Keys  *cryptox.KeyRing
	CRM   LifecycleCRM
}

// The documented signature has no event time or generation. It is only a hint
// to check CURRENT credentials, never authority to uninstall a later generation.
func validDisconnectSignature(client, account, signature string, secret []byte) bool {
	got, err := hex.DecodeString(signature)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(client + "|" + account))
	return hmac.Equal(got, m.Sum(nil))
}

func (h *Lifecycle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	q, err := r.URL.Query(), error(nil)
	for _, k := range []string{"client_uuid", "account_id", "signature"} {
		if len(q[k]) != 1 {
			w.WriteHeader(400)
			return
		}
	}
	client, err := uuid.Parse(q.Get("client_uuid"))
	if err != nil || client.String() != q.Get("client_uuid") {
		w.WriteHeader(400)
		return
	}
	account, err := strconv.ParseInt(q.Get("account_id"), 10, 64)
	if err != nil || account <= 0 || strconv.FormatInt(account, 10) != q.Get("account_id") {
		w.WriteHeader(400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var integration, installation uuid.UUID
	var encrypted []byte
	var key int
	err = h.Store.Pool.QueryRow(ctx, `SELECT p.id,i.id,p.client_secret_ciphertext,p.client_secret_key_version FROM integrations p JOIN integration_services s ON s.integration_id=p.id AND s.service_code='task-control' JOIN installations i ON i.integration_id=p.id WHERE p.client_id=$1 AND i.account_id=$2`, client.String(), account).Scan(&integration, &installation, &encrypted, &key)
	if err != nil {
		w.WriteHeader(403)
		return
	}
	secret, err := h.Keys.Open(key, encrypted, cryptox.IntegrationSecretAAD(integration))
	if err != nil {
		w.WriteHeader(503)
		return
	}
	defer clear(secret)
	if !validDisconnectSignature(client.String(), strconv.FormatInt(account, 10), q.Get("signature"), secret) {
		w.WriteHeader(403)
		return
	}
	// Persist before acknowledging; duplicates do not postpone an outstanding check.
	_, err = h.Store.Pool.Exec(ctx, `INSERT INTO task_control_installation_versions(installation_id,last_status) SELECT id,status FROM installations WHERE id=$1 ON CONFLICT DO NOTHING`, installation)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	_, err = h.Store.Pool.Exec(ctx, `UPDATE task_control_installation_versions SET signal_pending=true,revision=revision+1,next_check_at=CASE WHEN signal_pending THEN next_check_at ELSE now() END WHERE installation_id=$1 AND evidence<>'disconnect_confirmed'`, installation)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	w.WriteHeader(202)
}

func (h *Lifecycle) Reconcile(ctx context.Context, id uuid.UUID) error {
	var generation, account, revision int64
	err := h.Store.Pool.QueryRow(ctx, `SELECT v.generation,i.account_id,v.revision FROM task_control_installation_versions v JOIN installations i ON i.id=v.installation_id WHERE v.installation_id=$1 AND v.signal_pending`, id).Scan(&generation, &account, &revision)
	if err != nil {
		return err
	}
	err = h.CRM.CheckTaskControlAuthorization(ctx, id, account)
	state := "disconnect_unverified"
	var api *amocrm.APIError
	if err == nil {
		state = "registry"
	}
	if errors.As(err, &api) && api.Kind == amocrm.ErrorUnauthorized {
		state = "disconnect_confirmed"
	}
	// No generic 400/403/timeout/reauth_required inference: these have other causes.
	_, saveErr := h.Store.Pool.Exec(ctx, `UPDATE task_control_installation_versions SET evidence=$3,signal_pending=($3='disconnect_unverified'),next_check_at=now()+interval '60 seconds',revision=revision+CASE WHEN evidence<>$3 THEN 1 ELSE 0 END,generation=generation+CASE WHEN $3='disconnect_confirmed' THEN 1 ELSE 0 END WHERE installation_id=$1 AND generation=$2 AND signal_pending AND revision=$4`, id, generation, state, revision)
	return saveErr
}

func (h *Lifecycle) Run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// Claim one job briefly, leaving the network call outside DB locks. The
			// generation CAS prevents an old request changing a fresh OAuth installation.
			var id uuid.UUID
			err := h.Store.Pool.QueryRow(ctx, `UPDATE task_control_installation_versions SET next_check_at=now()+interval '60 seconds' WHERE installation_id=(SELECT installation_id FROM task_control_installation_versions WHERE signal_pending AND next_check_at<=now() ORDER BY next_check_at LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING installation_id`).Scan(&id)
			if err != nil {
				continue
			}
			call, cancel := context.WithTimeout(ctx, 15*time.Second)
			_ = h.Reconcile(call, id)
			cancel()
		}
	}
}
