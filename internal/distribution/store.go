package distribution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"sort"
	"time"
)

type Binding struct {
	ID             uuid.UUID `json:"bindingId"`
	CompanyID      uuid.UUID `json:"companyId"`
	InstallationID uuid.UUID `json:"installationId"`
	IntegrationID  uuid.UUID `json:"integrationId"`
	AccountID      int64     `json:"accountId,string"`
	Revision       int64     `json:"bindingRevision"`
	IntentID       uuid.UUID `json:"intentId"`
	State          string    `json:"state"`
}
type Mapping struct {
	EmployeeID uuid.UUID `json:"employeeId"`
	UserID     int64     `json:"userId,string"`
}
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool} }
func (s *Store) AuthorizeRequest(ctx context.Context, scope Scope, nonce uuid.UUID, until time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var marker int
	err = tx.QueryRow(ctx, `SELECT 1 FROM distribution_service_grants WHERE key_id=$1 AND company_id=$2 AND installation_id=$3 AND enabled FOR SHARE`, scope.KeyID, scope.CompanyID, scope.InstallationID).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	// Bounded cleanup cannot revive signatures: retention exceeds auth window.
	_, err = tx.Exec(ctx, `DELETE FROM distribution_service_nonces WHERE (key_id,nonce) IN (SELECT key_id,nonce FROM distribution_service_nonces WHERE expires_at < now() LIMIT 100)`)
	if err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `INSERT INTO distribution_service_nonces(key_id,nonce,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, scope.KeyID, nonce, until)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return ErrDenied
	}
	return tx.Commit(ctx)
}
func (s *Store) Require(ctx context.Context, b Binding) error {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_bindings WHERE id=$1 AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 AND state='active')`, b.ID, b.CompanyID, b.InstallationID, b.IntegrationID, b.AccountID, b.Revision).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return ErrDenied
	}
	return services.RequireEnabled(ctx, s.pool, b.InstallationID, services.LeadDistribution, false)
}
func (s *Store) Get(ctx context.Context, scope Scope, id uuid.UUID) (Binding, error) {
	b := Binding{}
	err := s.pool.QueryRow(ctx, `SELECT b.id,b.company_id,b.installation_id,b.integration_id,b.account_id,b.revision,b.intent_id,CASE WHEN b.state='revoked' THEN 'revoked' WHEN i.status='active' AND p.status='active' AND c.enabled THEN 'active' WHEN i.status='reauth_required' THEN 'reauth_required' ELSE 'disabled' END FROM distribution_bindings b JOIN installations i ON i.id=b.installation_id JOIN integrations p ON p.id=b.integration_id LEFT JOIN integration_services c ON c.integration_id=p.id AND c.service_code='lead-distribution' WHERE b.id=$1 AND b.company_id=$2 AND b.installation_id=$3`, id, scope.CompanyID, scope.InstallationID).Scan(&b.ID, &b.CompanyID, &b.InstallationID, &b.IntegrationID, &b.AccountID, &b.Revision, &b.IntentID, &b.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}
func (s *Store) ForPrincipal(ctx context.Context, p widgetauth.Principal) (Binding, error) {
	var company, id uuid.UUID
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT id,company_id,count(*) OVER () FROM distribution_bindings WHERE installation_id=$1 AND integration_id=$2 AND account_id=$3 AND state='active'`, p.InstallationID, p.IntegrationID, p.AccountID).Scan(&id, &company, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, err
	}
	if count != 1 {
		return Binding{}, ErrConflict
	}
	return s.Get(ctx, Scope{CompanyID: company, InstallationID: p.InstallationID}, id)
}
func consume(ctx context.Context, tx pgx.Tx, p widgetauth.Principal) error {
	ct, err := tx.Exec(ctx, `INSERT INTO used_widget_tokens(integration_id,jti,issuer,account_id,user_id,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, p.IntegrationID, p.TokenID, p.Issuer, p.AccountID, p.UserID, p.TokenRetainUntil)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return widgetauth.ErrReplay
	}
	return nil
}
func (s *Store) Bind(ctx context.Context, scope Scope, b Binding, p widgetauth.Principal, expires time.Time) (Binding, error) {
	if expires.IsZero() || !expires.After(time.Now()) || expires.After(time.Now().Add(15*time.Minute)) {
		return Binding{}, ErrDenied
	}
	if b.ID == uuid.Nil || b.IntentID == uuid.Nil || b.Revision != 1 || b.CompanyID != scope.CompanyID || b.InstallationID != scope.InstallationID || b.InstallationID != p.InstallationID || b.IntegrationID != p.IntegrationID || b.AccountID != p.AccountID {
		return Binding{}, ErrDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback(ctx)
	if err = services.RequireEnabled(ctx, tx, b.InstallationID, services.LeadDistribution, true); err != nil {
		return Binding{}, err
	}
	// Serialize conflicting company/installation links in canonical order.
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, b.CompanyID.String())
	if err != nil {
		return Binding{}, err
	}
	var revoked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_binding_tombstones WHERE id=$1)`, b.ID).Scan(&revoked); err != nil {
		return Binding{}, err
	}
	if revoked || !expires.After(time.Now()) {
		return Binding{}, ErrConflict
	}
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM distribution_bindings WHERE (state='active' AND (company_id=$1 OR installation_id=$2)) OR intent_id=$3`, b.CompanyID, b.InstallationID, b.IntentID).Scan(&existing)
	if err == nil {
		return Binding{}, ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, err
	}
	if err = consume(ctx, tx, p); err != nil {
		return Binding{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, b.ID, b.CompanyID, b.InstallationID, b.IntegrationID, b.AccountID, b.Revision, b.IntentID, p.UserID)
	if err != nil {
		return Binding{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'widget',$2,'distribution.binding_confirm','distribution_binding',$3,jsonb_build_object('company_id',$4::text,'revision',$5::bigint))`, b.InstallationID, fmt.Sprint(p.UserID), b.ID.String(), b.CompanyID.String(), b.Revision); err != nil {
		return Binding{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Binding{}, err
	}
	b.State = "active"
	return b, nil
}
func (s *Store) ReplaceMappings(ctx context.Context, b Binding, rows []Mapping, mappingRevision int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = services.RequireEnabled(ctx, tx, b.InstallationID, services.LeadDistribution, true); err != nil {
		return err
	}

	if mappingRevision <= 0 || mappingRevision > 9007199254740991 {
		return ErrConflict
	}
	canonical := append([]Mapping(nil), rows...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].EmployeeID.String() < canonical[j].EmployeeID.String() })
	raw, _ := json.Marshal(canonical)
	hash := sha256.Sum256(raw)
	var rev, current int64
	var state string
	var currentHash []byte
	if err = tx.QueryRow(ctx, `SELECT revision,state,mapping_revision,mapping_hash FROM distribution_bindings WHERE id=$1 AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 FOR UPDATE`, b.ID, b.CompanyID, b.InstallationID, b.IntegrationID, b.AccountID).Scan(&rev, &state, &current, &currentHash); err != nil {
		return err
	}
	if rev != b.Revision || state != "active" || mappingRevision < current {
		return ErrConflict
	}
	if mappingRevision == current {
		if bytes.Equal(hash[:], currentHash) {
			return tx.Commit(ctx)
		}
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE distribution_bindings SET mapping_revision=$2,mapping_hash=$3 WHERE id=$1`, b.ID, mappingRevision, hash[:]); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM distribution_actor_mappings WHERE binding_id=$1`, b.ID); err != nil {
		return err
	}
	for _, m := range rows {
		if _, err = tx.Exec(ctx, `INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,$3)`, b.ID, m.EmployeeID, m.UserID); err != nil {
			return ErrConflict
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) Mapped(ctx context.Context, b Binding, employee uuid.UUID, user int64) error {
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_actor_mappings WHERE binding_id=$1 AND employee_id=$2 AND user_id=$3)`, b.ID, employee, user).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

func (s *Store) Revoke(ctx context.Context, scope Scope, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope.CompanyID.String()); err != nil {
		return err
	}
	var oldCompany, oldInstall uuid.UUID
	err = tx.QueryRow(ctx, `SELECT company_id,installation_id FROM distribution_bindings WHERE id=$1`, id).Scan(&oldCompany, &oldInstall)
	if err == nil && (oldCompany != scope.CompanyID || oldInstall != scope.InstallationID) {
		return ErrNotFound
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO distribution_binding_tombstones(id,company_id,installation_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, scope.CompanyID, scope.InstallationID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE distribution_bindings SET state='revoked',revoked_at=now() WHERE id=$1 AND company_id=$2 AND installation_id=$3`, id, scope.CompanyID, scope.InstallationID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'service',$2,'distribution.binding_revoke','distribution_binding',$3,jsonb_build_object('company_id',$4::text))`, scope.InstallationID, scope.KeyID, id.String(), scope.CompanyID.String()); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Readiness is safe connection diagnostics, not global Core readiness or a
// promise that the assignment engine (later stages) is already available.
func (s *Store) Readiness(ctx context.Context, b Binding) (map[string]any, error) {
	var installation, webhook string
	var creds, subscription bool
	err := s.pool.QueryRow(ctx, `SELECT i.status,i.webhook_status,EXISTS(SELECT 1 FROM oauth_credentials o WHERE o.installation_id=i.id),i.webhook_status='active' AND i.webhook_settings ? 'add_lead' AND i.webhook_settings ? 'status_lead' AND i.webhook_settings ? 'update_lead' AND i.webhook_settings ? 'responsible_lead' AND i.webhook_settings ? 'delete_lead' FROM installations i WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3`, b.InstallationID, b.IntegrationID, b.AccountID).Scan(&installation, &webhook, &creds, &subscription)
	if err != nil {
		return nil, err
	}
	action := "none"
	if !creds || installation == "reauth_required" {
		action = "reauthorize"
	} else if installation != "active" {
		action = "enable_installation"
	} else if !subscription {
		action = "reconcile_webhook_subscription"
	}
	enabled := s.Require(ctx, b) == nil
	return map[string]any{"state": b.State, "oauthReady": creds && installation == "active", "capabilityReady": enabled, "subscriptionReady": subscription, "webhookStatus": webhook, "recoveryAction": action, "assignmentReady": false}, nil
}
