package activitybridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"net/http"
	"net/url"
	"time"
)

type browserAuthKey struct{}

const browserSessionTTL = 30 * time.Minute

// Only a freshly authenticated amoCRM administrator can mint a browser session.
// The opaque credential is stored hashed in Core, never in Activity or a URL query.
func (b *Bridge) BrowserSessionHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var body struct {
		AppOrigin string `json:"app_origin"`
	}
	if err := decode(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if _, ok := b.appOrigins[body.AppOrigin]; !ok {
		writeError(w, serviceapi.Fail(serviceapi.PermissionDenied, "app origin is not configured"))
		return
	}
	origin, err := url.Parse(body.AppOrigin)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" {
		writeError(w, serviceapi.Fail(serviceapi.InvalidArgument, "invalid app origin"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionPanels); err != nil {
		writeError(w, err)
		return
	}
	if b.pool == nil {
		writeError(w, serviceapi.Fail(serviceapi.Unavailable, "browser session store unavailable"))
		return
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		writeError(w, serviceapi.Fail(serviceapi.Internal, "generate browser session"))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	hash := sha256.Sum256([]byte(token))
	expires := time.Now().Add(browserSessionTTL)
	// Bound live sessions per installation; expiry cleanup does not revoke active work.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `DELETE FROM activity_browser_sessions WHERE expires_at<now()`); err != nil {
		writeError(w, err)
		return
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "activity-browser:"+p.InstallationID.String()); err != nil {
		writeError(w, err)
		return
	}
	var liveCount int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM activity_browser_sessions WHERE installation_id=$1 AND actor_id=$2 AND expires_at>now()`, p.InstallationID, p.UserID).Scan(&liveCount); err != nil {
		writeError(w, err)
		return
	}
	if liveCount >= 10 {
		writeError(w, serviceapi.Fail(serviceapi.ResourceExhausted, "too many active browser sessions"))
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO activity_browser_sessions(token_hash,installation_id,integration_id,actor_id,expires_at) VALUES($1,$2,$3,$4,$5)`, hash[:], p.InstallationID, p.IntegrationID, p.UserID, expires); err != nil {
		writeError(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	respond(w, http.StatusOK, map[string]any{"url": origin.Scheme + "://" + origin.Host + "/#/settings/panels?admin_session=" + url.QueryEscape(token), "expires_at": expires.Unix()}, nil)
}
func (b *Bridge) browserManagementAuth(ctx context.Context, token string) (serviceapi.Auth, serviceapi.Scope, error) {
	if b.pool == nil || len(token) != 43 {
		return serviceapi.Auth{}, serviceapi.Scope{}, serviceapi.Fail(serviceapi.Unauthenticated, "management authentication required")
	}
	hash := sha256.Sum256([]byte(token))
	var scope serviceapi.Scope
	var actor int64
	err := b.pool.QueryRow(ctx, `SELECT installation_id,integration_id,actor_id FROM activity_browser_sessions WHERE token_hash=$1 AND expires_at>now()`, hash[:]).Scan(&scope.InstallationID, &scope.IntegrationID, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.Auth{}, scope, serviceapi.Fail(serviceapi.Unauthenticated, "browser session expired or invalid")
	}
	if err != nil {
		return serviceapi.Auth{}, scope, err
	}
	// Every browser operation rechecks the actual administrator through Policy.Issue.
	auth, err := b.policy.Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: actor, Consumer: serviceapi.ActivityService, RequestID: uuid.NewString(), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels)})
	return auth, scope, err
}
func (b *Bridge) browserManagementAccess(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if len(b.appOrigins) == 0 {
		writeShareError(w, serviceapi.Fail(serviceapi.Unauthenticated, "management authentication required"))
		return
	}
	if !b.allowViewerOrigin(w, r) {
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, Idempotency-Key")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := bearerOrEmpty(r)
	if b.viewerLimiter != nil && (!b.viewerLimiter.AllowGlobal() || !b.viewerLimiter.Allow(sha256.Sum256([]byte(token)))) {
		writeShareError(w, errCapacity)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	auth, scope, err := b.browserManagementAuth(ctx, token)
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx = context.WithValue(withManagementScope(ctx, scope), browserAuthKey{}, auth)
	next.ServeHTTP(w, r.WithContext(ctx))
}
