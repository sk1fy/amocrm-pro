package taskcontrol

import (
	"context"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
	"log/slog"
	"net/http"
	"time"
)

func Router(ctx context.Context, pool *pgxpool.Pool, secrets *cryptox.KeyRing, crm CRMIdentity, c Config) (http.Handler, error) {
	// amoCRM disposable JWTs live for 30 minutes; handoff remains 60 seconds.
	if c.JWTMaxLifetime == 0 {
		c.JWTMaxLifetime = 30 * time.Minute
	}
	if c.JWTLeeway == 0 {
		c.JWTLeeway = 5 * time.Second
	}
	auth, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), secrets,
		widgetauth.WithMaxLifetime(c.JWTMaxLifetime), widgetauth.WithLeeway(c.JWTLeeway), widgetauth.WithLogger(slog.Default()))
	if err != nil {
		return nil, err
	}
	limit, err := widgetlimit.New(widgetlimit.Config{IntegrationRate: 10, IntegrationBurst: 10, InstallationRate: 5, InstallationBurst: 5, InactiveTTL: 5 * time.Minute, MaxEntries: 10000}, nil)
	if err != nil {
		return nil, err
	}
	go limit.Run(ctx)
	store := &Store{Pool: pool}
	router := chi.NewRouter()
	router.Handle("/internal/v1/task-control/accounts/*", &Handler{Store: store, Keys: c.Keys})
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	handoff := &HandoffHandler{Store: store, CRM: crm, KeyID: c.AssertionKeyID, Key: c.AssertionKey}
	router.Method("OPTIONS", "/api/v1/widget/task-control/auth", cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	router.Method("POST", "/api/v1/widget/task-control/auth", cors(widgetauth.VerificationMiddleware(auth)(widgetcors.BindPrincipalIssuer(limit.Middleware(handoff)))))
	return router, nil
}
