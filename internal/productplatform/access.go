package productplatform

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

const accessTimeout = time.Second

type accessProjection struct {
	account, product, integration, installation uuid.UUID
	generation, authorizationSequence, version  int64
	authorized                                  bool
	until, packageUntil                         *time.Time
	source                                      *string
}

type accessState struct {
	Account     uuid.UUID  `json:"account_id"`
	Product     uuid.UUID  `json:"product_id"`
	Active      bool       `json:"active"`
	ValidUntil  *time.Time `json:"valid_until"`
	Source      string     `json:"source"`
	ReasonCodes []string   `json:"reason_codes"`
	OwnUntil    *time.Time `json:"own_until"`
	Version     int64      `json:"entitlement_version"`
	ComputedAt  time.Time  `json:"computed_at"`
}

func (p accessProjection) state(now time.Time) accessState {
	result := accessState{
		Account: p.account, Product: p.product, Source: "none", ReasonCodes: []string{"term_missing"},
		Version: p.version, ComputedAt: now.UTC(),
	}
	result.OwnUntil = p.until

	if p.until != nil {
		result.ReasonCodes = []string{"term_unavailable"}
		if p.source != nil {
			switch *p.source {
			case "own_trial", "free", "own_paid", "manual":
				until := p.until.UTC()
				result.ValidUntil, result.Source = &until, *p.source
				result.Active = now.Before(until)
				result.ReasonCodes = []string{"term_expired"}
				if result.Active {
					result.ReasonCodes = []string{}
				}
			}
		}
	}
	if p.packageUntil != nil && now.Before(*p.packageUntil) &&
		(result.ValidUntil == nil || p.packageUntil.After(*result.ValidUntil)) {
		result.ValidUntil, result.Source, result.Active = p.packageUntil, "subscription", true
		result.ReasonCodes = []string{}
	}
	return result
}

func AccessHandler(pool *pgxpool.Pool, verifier *platformaccess.Verifier, origin string) http.Handler {
	return httpmiddleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !accessCORS(w, r, origin) {
			return
		}
		account, err := wire.AccountID(r.PathValue("amo_account_id"))
		product := r.PathValue("product_code")
		if err != nil || !wire.ValidProductCode(product) {
			platformaccess.WriteError(w, r.Context(), http.StatusBadRequest, "invalid_argument", 0)
			return
		}
		auth := r.Header.Values("Authorization")
		var proof string
		if len(auth) == 1 && len(auth[0]) <= 2500 {
			scheme, value, _ := strings.Cut(auth[0], " ")
			if strings.EqualFold(scheme, "Bearer") {
				proof = value
			}
		}
		claims, err := verifier.Verify(proof, time.Now())
		if err != nil {
			platformaccess.WriteError(w, r.Context(), http.StatusUnauthorized, "unauthenticated", 0)
			return
		}
		if claims.Account != r.PathValue("amo_account_id") || claims.Product != product {
			platformaccess.WriteError(w, r.Context(), http.StatusForbidden, "permission_denied", 0)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), accessTimeout)
		defer cancel()
		var snapshot accessProjection
		err = pool.QueryRow(ctx, `
			SELECT account_id,product_id,integration_id,installation_id,generation,authorization_sequence,
				authorized,own_until,source,entitlement_version,package_until
			FROM platform_access.current_access LEFT JOIN platform_access.subscription_terms USING(account_id,product_id) WHERE amo_account_id=$1 AND product_code=$2`, account, product).
			Scan(&snapshot.account, &snapshot.product, &snapshot.integration, &snapshot.installation,
				&snapshot.generation, &snapshot.authorizationSequence, &snapshot.authorized, &snapshot.until,
				&snapshot.source, &snapshot.version, &snapshot.packageUntil)
		now := time.Now()
		switch {
		case now.Unix() >= claims.ExpiresAt:
			platformaccess.WriteError(w, r.Context(), http.StatusUnauthorized, "unauthenticated", 0)
		case errors.Is(err, pgx.ErrNoRows):
			platformaccess.WriteError(w, r.Context(), http.StatusNotFound, "not_found", 0)
		case err != nil:
			platformaccess.WriteError(w, r.Context(), http.StatusServiceUnavailable, "unavailable", 1)
		case !snapshot.authorized || claims.Integration != snapshot.integration || claims.Installation != snapshot.installation ||
			claims.Generation != snapshot.generation || claims.AuthorizationSequence != snapshot.authorizationSequence:
			platformaccess.WriteError(w, r.Context(), http.StatusForbidden, "permission_denied", 0)
		default:
			platformaccess.WriteJSON(w, http.StatusOK, snapshot.state(now))
		}
	}))
}

func accessCORS(w http.ResponseWriter, r *http.Request, allowedOrigin string) bool {
	h := w.Header()
	h.Add("Vary", "Origin")
	origin := r.Header.Values("Origin")
	if len(origin) == 0 && r.Method != http.MethodOptions {
		return true
	}
	if len(origin) != 1 || allowedOrigin == "" || origin[0] != allowedOrigin {
		platformaccess.WriteError(w, r.Context(), http.StatusForbidden, "permission_denied", 0)
		return false
	}
	if r.Method != http.MethodOptions {
		h.Set("Access-Control-Allow-Origin", allowedOrigin)
		h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
		return true
	}
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	method := r.Header.Values("Access-Control-Request-Method")
	headers := r.Header.Values("Access-Control-Request-Headers")
	if len(method) != 1 || method[0] != http.MethodGet || len(headers) != 1 || !validAccessCORSHeaders(headers[0]) {
		platformaccess.WriteError(w, r.Context(), http.StatusForbidden, "permission_denied", 0)
		return false
	}
	h.Set("Access-Control-Allow-Origin", allowedOrigin)
	h.Set("Access-Control-Allow-Methods", http.MethodGet)
	h.Set("Access-Control-Allow-Headers", "Authorization, X-Request-ID")
	w.WriteHeader(http.StatusNoContent)
	return false
}

func validAccessCORSHeaders(raw string) bool {
	var authorization, requestID bool
	for header := range strings.SplitSeq(raw, ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "authorization":
			if authorization {
				return false
			}
			authorization = true
		case "x-request-id":
			if requestID {
				return false
			}
			requestID = true
		default:
			return false
		}
	}
	return authorization
}
