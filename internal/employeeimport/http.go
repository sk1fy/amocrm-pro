package employeeimport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/admincrm"
	"github.com/sk1fy/amocrm-pro/internal/adminread"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Reader interface {
	CRMUsers(context.Context, uuid.UUID) ([]amocrm.CRMUser, error)
}

type Resolver interface {
	ResolveActivityConnection(context.Context, int64) (admincrm.Connection, error)
}

type employee struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	IsActive bool   `json:"is_active"`
}

func Mount(mux *http.ServeMux, token string, resolver Resolver, crm Reader) {
	mux.Handle("GET /admin/v1/accounts/{account}/employee-import", adminread.Authenticate(token)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if adminread.ActorFromContext(r.Context()) != "service:teamos-employee-import" {
			http.Error(w, "employee directory unavailable", http.StatusForbidden)
			return
		}
		account := r.PathValue("account")
		accountID, err := strconv.ParseInt(account, 10, 64)
		if err != nil || accountID <= 0 || strconv.FormatInt(accountID, 10) != account {
			http.Error(w, "invalid account", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		connection, err := resolver.ResolveActivityConnection(ctx, accountID)
		if err != nil {
			http.Error(w, "employee directory unavailable", http.StatusServiceUnavailable)
			return
		}
		started := time.Now().UTC()
		users, err := crm.CRMUsers(ctx, connection.InstallationID)
		if err != nil || ctx.Err() != nil {
			http.Error(w, "employee directory unavailable", http.StatusServiceUnavailable)
			return
		}
		current, err := resolver.ResolveActivityConnection(ctx, accountID)
		if err != nil || current != connection {
			http.Error(w, "employee directory unavailable", http.StatusServiceUnavailable)
			return
		}
		items := make([]employee, 0, len(users))
		for _, user := range users {
			item := employee{ID: user.ID, Name: user.Name, IsActive: user.Rights.IsActive != nil && *user.Rights.IsActive}
			if item.IsActive {
				item.Email = user.Email
			}
			items = append(items, item)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "ok", "account_id": accountID,
			"connection": map[string]string{
				"installation_id":  connection.InstallationID.String(),
				"integration_code": connection.IntegrationCode,
			},
			"fresh_until": started.Add(admincrm.FreshFor),
			"total":       len(items), "items": items,
		})
	})))
}
