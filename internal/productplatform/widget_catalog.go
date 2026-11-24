package productplatform

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

// Only the authenticated Core service can attest a technical integration.
func (s *LifecycleStore) catalog(w http.ResponseWriter, r *http.Request) {
	var registration wire.IntegrationRegistration
	var id uuid.UUID
	ctx := r.Context()
	switch r.Method {
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		if err != nil || wire.Decode(raw, &registration) != nil || registration.ID == uuid.Nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id = registration.ID
		if _, err := s.pool.Exec(ctx, `INSERT INTO platform_lifecycle.core_integrations AS c(id,active)
			VALUES ($1,$2) ON CONFLICT(id) DO UPDATE SET active=excluded.active WHERE c.active<>excluded.active`, id, registration.Active); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	case http.MethodGet:
		var err error
		id, err = uuid.Parse(r.URL.Query().Get("integration_id"))
		if err != nil || id == uuid.Nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var p wire.CatalogPolicy
	err := s.pool.QueryRow(ctx, `SELECT p.code FROM platform_lifecycle.integrations i
		JOIN platform_lifecycle.products p ON p.id=i.product_id
		WHERE i.id=$1 AND p.catalog_version IS NOT NULL`, id).Scan(&p.Product)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost && p.Product != "" && registration.Active {
		after := uuid.Nil
		if registration.After != nil {
			after = *registration.After
		}
		rows, err := s.pool.Query(ctx, `SELECT e.installation_id,e.generation
            FROM platform_lifecycle.account_products pair JOIN platform_lifecycle.episodes e USING(installation_id)
            JOIN platform_manager.installation_contacts c ON c.episode_id=e.id
            WHERE pair.integration_id=$1 AND e.installation_id>$2 AND e.closed_at IS NULL
            ORDER BY e.installation_id LIMIT $3`, id, after, wire.ConfirmationPageSize)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		p.Confirmed, err = pgx.CollectRows(rows, pgx.RowToStructByPos[wire.ConfirmedInstallation])
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	writeLifecycleJSON(w, http.StatusOK, p)
}
