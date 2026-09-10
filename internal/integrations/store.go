// Package integrations provides audited operator provisioning. It never returns
// client secret plaintext or ciphertext to its caller.
package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

var (
	ErrNotFound = errors.New("integration not found")
	ErrConflict = errors.New("integration code or client id already exists")
	codePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}[a-z0-9]$`)
)

type Cipher interface {
	Seal([]byte, []byte) ([]byte, int, error)
}
type Store struct {
	pool   *pgxpool.Pool
	cipher Cipher
}

func NewStore(pool *pgxpool.Pool, cipher Cipher) *Store { return &Store{pool: pool, cipher: cipher} }

// Command keeps client identity immutable after creation. Register a new
// integration if amoCRM issues a new client ID.
type Command struct {
	Action         string
	Actor          string
	Code           string
	ClientID       string
	Secret         []byte
	RedirectURI    *string
	WebhookEvents  *[]string
	Services       []string
	Service        string
	Enabled        bool
	InstallationID uuid.UUID
}

type Result struct {
	ID             uuid.UUID  `json:"integration_id"`
	InstallationID *uuid.UUID `json:"installation_id,omitempty"`
	Code           string     `json:"code"`
	Status         string     `json:"status"`
	Action         string     `json:"action"`
	WebhookError   string     `json:"webhook_error,omitempty"`
}

func installationAction(action string) bool {
	switch action {
	case "disable-installation", "enable-installation", "uninstall", "revoke":
		return true
	}
	return false
}

func (c Command) Validate() error {
	if len(c.Actor) == 0 || len(c.Actor) > 128 || strings.TrimSpace(c.Actor) != c.Actor || strings.ContainsAny(c.Actor, "\r\n\t") {
		return errors.New("actor must be a non-empty identifier of at most 128 bytes")
	}
	if !codePattern.MatchString(c.Code) {
		return errors.New("code must contain 3-64 lowercase letters, digits, underscores, or hyphens")
	}
	switch c.Action {
	case "create":
		id, err := uuid.Parse(c.ClientID)
		if err != nil || id == uuid.Nil {
			return errors.New("client id must be a nonzero UUID")
		}
		if c.RedirectURI == nil || c.Services == nil {
			return errors.New("create requires redirect URI and an explicit services list")
		}
	case "update":
		if c.RedirectURI == nil && c.WebhookEvents == nil {
			return errors.New("update requires redirect URI or webhook events")
		}
	case "enable", "disable", "rotate-secret":
	case "disable-installation", "enable-installation", "uninstall", "revoke":
		if c.InstallationID == uuid.Nil {
			return errors.New("installation actions require a nonzero installation id")
		}
	case "set-service":
		if !services.Known(c.Service) {
			return errors.New("unknown service")
		}
	default:
		return errors.New("unknown operator action")
	}
	if c.Action == "create" || c.Action == "rotate-secret" {
		if len(c.Secret) == 0 || len(c.Secret) > 16384 || strings.TrimSpace(string(c.Secret)) == "" {
			return errors.New("client secret must contain 1-16384 bytes")
		}
	} else if len(c.Secret) > 0 {
		return errors.New("secret is only accepted for create or rotate-secret")
	}
	if c.Action != "create" && (c.ClientID != "" || c.Services != nil) {
		return errors.New("client id and services list are only accepted for create")
	}
	if c.Action != "create" && c.Action != "update" && (c.RedirectURI != nil || c.WebhookEvents != nil) {
		return errors.New("configuration is only accepted for create or update")
	}
	if c.Action != "set-service" && (c.Service != "" || c.Enabled) {
		return errors.New("service options are only accepted for set-service")
	}
	if !installationAction(c.Action) && c.InstallationID != uuid.Nil {
		return errors.New("installation id is only accepted for installation lifecycle actions")
	}
	if c.RedirectURI != nil {
		u, err := url.Parse(*c.RedirectURI)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("redirect URI must be an absolute HTTPS URL without user info or fragment")
		}
	}
	if c.WebhookEvents != nil {
		for _, event := range *c.WebhookEvents {
			if strings.TrimSpace(event) == "" || len(event) > 100 {
				return errors.New("webhook event names must contain 1-100 bytes")
			}
		}
	}
	for _, service := range c.Services {
		if !services.Known(service) {
			return errors.New("unknown service")
		}
	}
	return nil
}

// Apply commits each mutation and its audit record in one transaction. Audit
// metadata intentionally includes only field names, status, and service grants.
func (s *Store) Apply(ctx context.Context, c Command) (Result, error) {
	if err := c.Validate(); err != nil {
		return Result{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, errors.New("begin operator transaction failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockKey := "integration:" + c.Code
	if installationAction(c.Action) {
		lockKey = "installation:" + c.InstallationID.String()
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return Result{}, errors.New("lock operator mutation failed")
	}
	r := Result{Code: c.Code, Action: "integration." + c.Action}
	if installationAction(c.Action) {
		return applyInstallation(ctx, tx, c)
	}
	err = tx.QueryRow(ctx, `SELECT id,status FROM integrations WHERE code=$1 FOR UPDATE`, c.Code).Scan(&r.ID, &r.Status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, errors.New("read integration failed")
	}
	if c.Action == "create" && err == nil {
		return Result{}, ErrConflict
	}
	if c.Action != "create" && errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	metadata := map[string]any{}
	switch c.Action {
	case "create":
		r.ID = uuid.New()
		r.Status = "active"
		encrypted, version, sealErr := s.cipher.Seal(c.Secret, cryptox.IntegrationSecretAAD(r.ID))
		if sealErr != nil {
			return Result{}, errors.New("encrypt integration secret failed")
		}
		events := []string{}
		if c.WebhookEvents != nil {
			events = *c.WebhookEvents
		}
		if events == nil {
			events = []string{}
		}
		encoded, _ := json.Marshal(events)
		clientID, _ := uuid.Parse(c.ClientID)
		_, err = tx.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri,webhook_events) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.ID, c.Code, clientID.String(), encrypted, version, *c.RedirectURI, encoded)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return Result{}, ErrConflict
			}
			return Result{}, errors.New("create integration failed")
		}
		for _, service := range c.Services {
			if _, err = tx.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,$2,true) ON CONFLICT DO NOTHING`, r.ID, service); err != nil {
				return Result{}, errors.New("create service grant failed")
			}
		}
		metadata["services"] = c.Services
	case "update":
		fields := []string{}
		if c.RedirectURI != nil {
			_, err = tx.Exec(ctx, `UPDATE integrations SET redirect_uri=$2 WHERE id=$1`, r.ID, *c.RedirectURI)
			if err != nil {
				return Result{}, errors.New("update redirect URI failed")
			}
			fields = append(fields, "redirect_uri")
		}
		if c.WebhookEvents != nil {
			events := *c.WebhookEvents
			if events == nil {
				events = []string{}
			}
			encoded, _ := json.Marshal(events)
			_, err = tx.Exec(ctx, `UPDATE integrations SET webhook_events=$2 WHERE id=$1`, r.ID, encoded)
			if err != nil {
				return Result{}, errors.New("update webhook events failed")
			}
			fields = append(fields, "webhook_events")
		}
		metadata["fields"] = fields
	case "enable", "disable":
		metadata["previous_status"] = r.Status
		r.Status = "disabled"
		if c.Action == "enable" {
			r.Status = "active"
		}
		_, err = tx.Exec(ctx, `UPDATE integrations SET status=$2 WHERE id=$1`, r.ID, r.Status)
		metadata["status"] = r.Status
	case "rotate-secret":
		encrypted, version, sealErr := s.cipher.Seal(c.Secret, cryptox.IntegrationSecretAAD(r.ID))
		if sealErr != nil {
			return Result{}, errors.New("encrypt integration secret failed")
		}
		_, err = tx.Exec(ctx, `UPDATE integrations SET client_secret_ciphertext=$2,client_secret_key_version=$3 WHERE id=$1`, r.ID, encrypted, version)
		metadata["key_version"] = version
	case "set-service":
		_, err = tx.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,$2,$3) ON CONFLICT(integration_id,service_code) DO UPDATE SET enabled=EXCLUDED.enabled`, r.ID, c.Service, c.Enabled)
		metadata["service_code"] = c.Service
		metadata["enabled"] = c.Enabled
	}
	if err != nil {
		return Result{}, errors.New("update integration failed")
	}
	encoded, _ := json.Marshal(metadata)
	if _, err = tx.Exec(ctx, `INSERT INTO audit_log(actor_type,actor_id,action,object_type,object_id,metadata) VALUES('operator',$1,$2,'integration',$3,$4)`, c.Actor, r.Action, r.ID.String(), encoded); err != nil {
		return Result{}, errors.New("audit operator mutation failed")
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, errors.New("commit operator transaction failed")
	}
	return r, nil
}

func applyInstallation(ctx context.Context, tx pgx.Tx, c Command) (Result, error) {
	r := Result{Code: c.Code, InstallationID: &c.InstallationID}
	switch c.Action {
	case "disable-installation":
		r.Action = "installation.disable"
	case "enable-installation":
		r.Action = "installation.enable"
	case "uninstall":
		r.Action = "installation.uninstall"
	case "revoke":
		r.Action = "installation.revoke"
	}
	var integrationStatus string
	if err := tx.QueryRow(ctx, `SELECT id,status FROM integrations WHERE code=$1 FOR UPDATE`, c.Code).Scan(&r.ID, &integrationStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrNotFound
		}
		return Result{}, errors.New("read integration failed")
	}
	var current string
	err := tx.QueryRow(ctx, `
		SELECT status FROM installations
		WHERE id=$1 AND integration_id=$2 FOR UPDATE`, c.InstallationID, r.ID,
	).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, errors.New("read installation failed")
	}
	next, err := installationTransition(c.Action, current)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE installations SET status=$2 WHERE id=$1`, c.InstallationID, next); err != nil {
		return Result{}, errors.New("update installation failed")
	}
	r.Status = next
	metadata, _ := json.Marshal(map[string]any{"previous_status": current, "status": next, "integration_status": integrationStatus})
	if _, err = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'operator',$2,$3,'installation',$4,$5)`, c.InstallationID, c.Actor, r.Action, c.InstallationID.String(), metadata); err != nil {
		return Result{}, errors.New("audit operator mutation failed")
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, errors.New("commit operator transaction failed")
	}
	return r, nil
}

func installationTransition(action, current string) (string, error) {
	switch action {
	case "disable-installation":
		if current == "uninstalled" {
			return "", errors.New("cannot disable an uninstalled installation")
		}
		return "disabled", nil
	case "enable-installation":
		if current != "disabled" {
			return "", errors.New("enable-installation restores only a disabled installation; uninstalled tenants reauthorize through OAuth")
		}
		return "active", nil
	case "uninstall":
		return "uninstalled", nil
	case "revoke":
		if current == "disabled" || current == "uninstalled" {
			return "", errors.New("cannot revoke a disabled or uninstalled installation")
		}
		return "reauth_required", nil
	default:
		return "", errors.New("unknown operator action")
	}
}
