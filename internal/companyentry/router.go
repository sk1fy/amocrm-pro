package companyentry

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

func Router(ctx context.Context, pool *pgxpool.Pool, keys *cryptox.KeyRing, crm IdentityReader, c Config) (http.Handler, error) {
	auth, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys, widgetauth.WithMaxLifetime(30*time.Minute), widgetauth.WithLeeway(5*time.Second), widgetauth.WithLogger(slog.Default()))
	if err != nil {
		return nil, err
	}
	limiter, err := widgetlimit.New(widgetlimit.Config{IntegrationRate: 10, IntegrationBurst: 10, InstallationRate: 5, InstallationBurst: 5, InactiveTTL: 5 * time.Minute, MaxEntries: 10000}, nil)
	if err != nil {
		return nil, err
	}
	go limiter.Run(ctx)
	h := &Handler{Store: &Store{Pool: pool}, CRM: crm, Authority: &Client{BaseURL: c.TeamOSURL, Token: c.TeamOSToken, HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, FrontendOrigin: c.FrontendOrigin, SubjectCheckKey: c.SubjectCheckKey}
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(httpmiddleware.Recover(slog.Default()))
	router.Use(httpmiddleware.AccessLog(slog.Default()))
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	for path, handler := range map[string]http.HandlerFunc{"/api/v1/widget/company/status": h.Status, "/api/v1/widget/company/entries": h.Enter} {
		router.Method(http.MethodOptions, path, cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
		router.Method(http.MethodPost, path, cors(widgetauth.Middleware(auth)(widgetcors.BindPrincipalIssuer(limiter.Middleware(handler)))))
	}
	router.Post("/internal/v1/company-entry/subject-check", h.SubjectCheck)
	return router, nil
}
