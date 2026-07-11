package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
)

func RegisterPublicSystemRoutes(router chi.Router) {
	router.Method(apicontract.Live.Method, apicontract.Live.Path, http.HandlerFunc(Live))
}

func RegisterManagementRoutes(
	router chi.Router,
	pool *pgxpool.Pool,
	databaseTimeout time.Duration,
	metricsHandler http.Handler,
) {
	router.Method(apicontract.Live.Method, apicontract.Live.Path, http.HandlerFunc(Live))
	router.Method(apicontract.Ready.Method, apicontract.Ready.Path, Ready(pool, databaseTimeout))
	router.Method(apicontract.Metrics.Method, apicontract.Metrics.Path, metricsHandler)
}

func Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func Ready(pool *pgxpool.Pool, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
