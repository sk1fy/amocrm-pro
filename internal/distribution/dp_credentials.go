package distribution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// dpCredentialAAD binds a Digital Pipeline credential to its installation and
// the exact TeamOS group it was issued for, so a sealed copy cannot be replayed
// under another scope.
func dpCredentialAAD(installationID, groupID uuid.UUID) []byte {
	out := make([]byte, 0, 32+64)
	out = append(out, []byte("distribution-dp-credential\x00")...)
	out = append(out, []byte(installationID.String())...)
	out = append(out, 0)
	out = append(out, []byte(groupID.String())...)
	return out
}

// DPCredentialBinding is the resolved per-group credential used by the public
// Digital Pipeline receiver. It never carries the plaintext key.
type DPCredentialBinding struct {
	InstallationID  uuid.UUID
	AccountID       int64
	GroupID         uuid.UUID
	BindingRevision int64
}

// FindDPCredentialByHash resolves SHA-256(key) against an active installation.
func (s *Store) FindDPCredentialByHash(ctx context.Context, keyHash []byte) (DPCredentialBinding, error) {
	var out DPCredentialBinding
	e := s.pool.QueryRow(ctx, `
		SELECT c.installation_id, c.account_id, c.group_id, c.binding_revision
		FROM distribution_dp_credentials c
		JOIN installations i ON i.id = c.installation_id
		WHERE c.key_hash = $1 AND i.status = 'active'`, keyHash).
		Scan(&out.InstallationID, &out.AccountID, &out.GroupID, &out.BindingRevision)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, e
}

// ActiveBindingRevision returns the current active binding revision for the
// installation/account scope, or ErrNotFound when none is active.
func (s *Store) ActiveBindingRevision(ctx context.Context, installationID uuid.UUID, accountID int64) (int64, error) {
	var revision int64
	e := s.pool.QueryRow(ctx, `
		SELECT b.revision FROM distribution_bindings b
		JOIN installations i ON i.id = b.installation_id
		JOIN integrations p ON p.id = b.integration_id
		JOIN integration_services c ON c.integration_id = p.id AND c.service_code = 'lead-distribution'
		WHERE b.installation_id = $1 AND b.account_id = $2
		  AND b.state = 'active' AND i.status = 'active' AND p.status = 'active' AND c.enabled
		ORDER BY b.revision DESC LIMIT 1`, installationID, accountID).Scan(&revision)
	if errors.Is(e, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, e
}

// dpCredentialIssue returns the stored key for (installation, account, group)
// or mints a fresh random one. Storage keeps only sha256 + sealed ciphertext,
// so issuance stays idempotent without persisting plaintext at rest.
func (h *Handler) dpCredentialIssue(ctx context.Context, b Binding, groupID uuid.UUID) (string, error) {
	if h.Cipher == nil {
		return "", ErrUnavailable
	}
	load := func() (uuid.UUID, []byte, int, error) {
		var id uuid.UUID
		var ciphertext []byte
		var version int
		e := h.Store.pool.QueryRow(ctx, `SELECT id, key_ciphertext, key_key_version FROM distribution_dp_credentials WHERE installation_id=$1 AND account_id=$2 AND group_id=$3`, b.InstallationID, b.AccountID, groupID).Scan(&id, &ciphertext, &version)
		return id, ciphertext, version, e
	}
	decrypt := func(id uuid.UUID, ciphertext []byte, version int) (string, error) {
		if _, e := h.Store.pool.Exec(ctx, `UPDATE distribution_dp_credentials SET binding_id=$2, binding_revision=$3, updated_at=now() WHERE id=$1`, id, b.ID, b.Revision); e != nil {
			return "", e
		}
		plain, e := h.Cipher.Open(version, ciphertext, dpCredentialAAD(b.InstallationID, groupID))
		if e != nil {
			return "", ErrUnavailable
		}
		return string(plain), nil
	}
	if id, ciphertext, version, e := load(); e == nil {
		return decrypt(id, ciphertext, version)
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return "", e
	}
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return "", e
	}
	key := "dp_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	sealed, version, e := h.Cipher.Seal([]byte(key), dpCredentialAAD(b.InstallationID, groupID))
	if e != nil {
		return "", e
	}
	tag, e := h.Store.pool.Exec(ctx, `INSERT INTO distribution_dp_credentials(id,installation_id,account_id,group_id,binding_id,binding_revision,key_hash,key_ciphertext,key_key_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (installation_id,account_id,group_id) DO NOTHING`, uuid.New(), b.InstallationID, b.AccountID, groupID, b.ID, b.Revision, sum[:], sealed, version)
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() == 0 {
		id, ciphertext, version, e := load()
		if e != nil {
			return "", e
		}
		return decrypt(id, ciphertext, version)
	}
	return key, nil
}

// dpCredential is the signed TeamOS bridge endpoint that issues the per-group
// Digital Pipeline credential. It never exposes the shared installation key and
// always answers with Cache-Control: no-store.
func (h *Handler) dpCredential(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	var body struct {
		GroupID uuid.UUID `json:"groupId"`
	}
	if decode(r, &body) != nil || body.GroupID == uuid.Nil {
		fail(w, 400, "validation_failed")
		return
	}
	key, e := h.dpCredentialIssue(r.Context(), b, body.GroupID)
	if e != nil {
		h.resultError(w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, 200, map[string]any{"key": key, "groupId": body.GroupID.String(), "bindingRevision": b.Revision})
}
