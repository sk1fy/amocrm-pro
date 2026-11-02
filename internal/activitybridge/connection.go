package activitybridge

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"net/http"
	"time"
)

type activityConnection struct {
	InstallationID string `json:"installation_id"`
	IntegrationID  string `json:"integration_id"`
	AccountID      string `json:"account_id"`
	State          string `json:"state"`
}

func (b *Bridge) scopeAccount(ctx context.Context, scope serviceapi.Scope) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var account string
	err := b.pool.QueryRow(ctx, `SELECT account_id::text FROM installations WHERE id=$1 AND integration_id=$2`, scope.InstallationID, scope.IntegrationID).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", serviceapi.Fail(serviceapi.NotFound, "installation not found")
	}
	if err != nil {
		return "", serviceapi.Fail(serviceapi.Unavailable, "installation unavailable")
	}
	return account, nil
}

func (b *Bridge) ConnectionHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	scope, ok := managementScopeFrom(r.Context())
	if !ok {
		writeShareError(w, serviceapi.Fail(serviceapi.PermissionDenied, "operator scope required"))
		return
	}
	if _, err := b.issueOperator(r); err != nil {
		writeShareError(w, err)
		return
	}
	account, err := b.scopeAccount(r.Context(), scope)
	if err != nil {
		writeShareError(w, err)
		return
	}
	respond(w, http.StatusOK, activityConnection{scope.InstallationID.String(), scope.IntegrationID.String(), account, "active"}, nil)
}
