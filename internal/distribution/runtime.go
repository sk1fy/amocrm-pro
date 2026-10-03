package distribution

import (
	"context"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	JWTLeeway, JWTMaxLifetime       time.Duration
	Address, TeamOSURL, TeamOSKeyID string
	TeamOSPublicURL                 string
	Keys                            map[string]string
}

func LoadConfig() (Config, error) {
	c := Config{TeamOSPublicURL: strings.TrimRight(os.Getenv("DISTRIBUTION_TEAMOS_PUBLIC_URL"), "/"), Address: os.Getenv("DISTRIBUTION_HTTP_ADDRESS"), TeamOSURL: strings.TrimRight(os.Getenv("DISTRIBUTION_TEAMOS_URL"), "/"), TeamOSKeyID: os.Getenv("DISTRIBUTION_TEAMOS_KEY_ID")}
	if c.TeamOSPublicURL != "" {
		u, e := url.Parse(c.TeamOSPublicURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return c, errors.New("DISTRIBUTION_TEAMOS_PUBLIC_URL must be an HTTPS origin")
		}
	}
	if c.Address == "" {
		if os.Getenv("DISTRIBUTION_SERVICE_KEYS") != "" || c.TeamOSURL != "" || c.TeamOSKeyID != "" {
			return c, errors.New("distribution listener settings require DISTRIBUTION_HTTP_ADDRESS")
		}
		return c, nil
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, errors.New("invalid distribution listener address")
	}
	c.JWTLeeway = 5 * time.Second
	c.JWTMaxLifetime = 15 * time.Minute
	if raw := os.Getenv("WIDGET_JWT_LEEWAY"); raw != "" {
		v, e := time.ParseDuration(raw)
		if e != nil {
			return c, errors.New("invalid WIDGET_JWT_LEEWAY")
		}
		c.JWTLeeway = v
	}
	if raw := os.Getenv("WIDGET_JWT_MAX_LIFETIME"); raw != "" {
		v, e := time.ParseDuration(raw)
		if e != nil {
			return c, errors.New("invalid WIDGET_JWT_MAX_LIFETIME")
		}
		c.JWTMaxLifetime = v
	}
	if c.JWTLeeway <= 0 || c.JWTLeeway > time.Minute || c.JWTMaxLifetime <= 0 || c.JWTMaxLifetime > time.Hour || c.JWTMaxLifetime < c.JWTLeeway {
		return c, errors.New("invalid widget JWT policy bounds")
	}
	keys, err := ParseKeys(os.Getenv("DISTRIBUTION_SERVICE_KEYS"))
	if err != nil {
		return c, err
	}
	c.Keys = keys
	u, err := url.Parse(c.TeamOSURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return c, errors.New("DISTRIBUTION_TEAMOS_URL must be an HTTPS origin")
	}
	if c.Keys[c.TeamOSKeyID] == "" {
		return c, errors.New("distribution TeamOS key id is unavailable")
	}
	// Listener is private; TLS termination is required at the deployment proxy.
	if os.Getenv("DISTRIBUTION_TLS_PROXY") != "true" {
		return c, errors.New("distribution listener requires DISTRIBUTION_TLS_PROXY=true and private TLS proxy")
	}
	return c, nil
}
func Router(ctx context.Context, pool *pgxpool.Pool, keys *cryptox.KeyRing, crm CRM, c Config) (http.Handler, error) {
	return routerWithHTTPClient(ctx, pool, keys, crm, c, nil)
}
func routerWithHTTPClient(ctx context.Context, pool *pgxpool.Pool, keys *cryptox.KeyRing, crm CRM, c Config, httpClient *http.Client) (http.Handler, error) {
	authenticator, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys, widgetauth.WithLeeway(c.JWTLeeway), widgetauth.WithMaxLifetime(c.JWTMaxLifetime))
	if err != nil {
		return nil, err
	}
	store := NewStore(pool)
	handler := &Handler{Store: store, CRM: crm, Verifier: authenticator, TeamOSURL: c.TeamOSURL, TeamOSKeyID: c.TeamOSKeyID, Keys: c.Keys, HTTP: httpClient, TeamOSPublicURL: c.TeamOSPublicURL}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(requestCtx))
		})
	})
	handler.RegisterService(router, Auth{Keys: c.Keys, Store: store})
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	limiter, err := widgetlimit.New(widgetlimit.Config{IntegrationRate: 10, IntegrationBurst: 10, InstallationRate: 5, InstallationBurst: 5, InactiveTTL: 5 * time.Minute, MaxEntries: 10000}, nil)
	if err != nil {
		return nil, err
	}
	go limiter.Run(ctx)
	protect := func(next http.Handler) http.Handler {
		return cors(widgetauth.VerificationMiddleware(authenticator)(widgetcors.BindPrincipalIssuer(limiter.Middleware(widgetauth.ConsumptionMiddleware(authenticator)(next)))))
	}
	handler.RegisterWidget(router, protect, cors)
	router.Get("/components/distribution/ready", func(w http.ResponseWriter, r *http.Request) {
		if pool.Ping(r.Context()) != nil {
			fail(w, 503, "source_unavailable")
			return
		}
		write(w, 200, map[string]string{"state": "ready", "scope": "connection-only"})
	})
	return router, nil
}
