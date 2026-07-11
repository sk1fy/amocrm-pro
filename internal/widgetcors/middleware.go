package widgetcors

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const (
	allowOriginHeader   = "Access-Control-Allow-Origin"
	allowMethodsHeader  = "Access-Control-Allow-Methods"
	allowHeadersHeader  = "Access-Control-Allow-Headers"
	exposeHeadersHeader = "Access-Control-Expose-Headers"
)

var allowedRequestHeaders = map[string]string{
	"authorization":   "Authorization",
	"content-type":    "Content-Type",
	"idempotency-key": "Idempotency-Key",
	"x-request-id":    "X-Request-ID",
}

// Middleware authorizes browser origins and answers strict widget preflights.
// It must be mounted only on widget routes. Requests without Origin are
// treated as non-browser clients and pass through unchanged.
func Middleware(authorizer OriginAuthorizer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			addVary(response.Header(), "Origin")
			rawOrigin := request.Header.Get("Origin")
			if rawOrigin == "" {
				next.ServeHTTP(response, request)
				return
			}

			origin, err := widgetauth.NormalizeHTTPSOrigin(rawOrigin)
			if err != nil || origin != rawOrigin || authorizer == nil {
				http.Error(response, "forbidden origin", http.StatusForbidden)
				return
			}
			active, err := authorizer.IsActiveOrigin(request.Context(), origin)
			if err != nil {
				http.Error(response, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			if !active {
				http.Error(response, "forbidden origin", http.StatusForbidden)
				return
			}

			if request.Method == http.MethodOptions {
				handlePreflight(response, request, origin)
				return
			}
			if request.Method != http.MethodGet && request.Method != http.MethodPost {
				http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			setActualResponseHeaders(response.Header(), origin)
			next.ServeHTTP(response, request)
		})
	}
}

// BindPrincipalIssuer rejects an authenticated browser request when its exact
// Origin differs from the cryptographically verified JWT issuer. Mount it
// after widgetauth Middleware or VerificationMiddleware.
func BindPrincipalIssuer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawOrigin := request.Header.Get("Origin")
		if rawOrigin == "" {
			next.ServeHTTP(response, request)
			return
		}
		principal, ok := widgetauth.PrincipalFromContext(request.Context())
		origin, err := widgetauth.NormalizeHTTPSOrigin(rawOrigin)
		issuer, issuerErr := widgetauth.NormalizeHTTPSOrigin(principal.Issuer)
		if !ok || err != nil || issuerErr != nil || origin != rawOrigin || origin != issuer {
			http.Error(response, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func handlePreflight(response http.ResponseWriter, request *http.Request, origin string) {
	addVary(response.Header(), "Access-Control-Request-Method")
	addVary(response.Header(), "Access-Control-Request-Headers")
	method := request.Header.Get("Access-Control-Request-Method")
	if method != http.MethodGet && method != http.MethodPost {
		http.Error(response, "invalid CORS preflight", http.StatusForbidden)
		return
	}
	headers, err := parseRequestedHeaders(request.Header.Get("Access-Control-Request-Headers"))
	if err != nil {
		http.Error(response, "invalid CORS preflight", http.StatusForbidden)
		return
	}
	response.Header().Set(allowOriginHeader, origin)
	response.Header().Set(allowMethodsHeader, method)
	if len(headers) > 0 {
		response.Header().Set(allowHeadersHeader, strings.Join(headers, ", "))
	}
	response.Header().Set("Access-Control-Max-Age", "300")
	response.WriteHeader(http.StatusNoContent)
}

func setActualResponseHeaders(header http.Header, origin string) {
	header.Set(allowOriginHeader, origin)
	header.Set(exposeHeadersHeader, "X-Request-ID, Idempotency-Replayed")
}

func parseRequestedHeaders(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	result := make([]string, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		canonical, ok := allowedRequestHeaders[name]
		if !ok || name == "" {
			return nil, errors.New("request header is not allowed")
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, canonical)
	}
	sort.Strings(result)
	return result, nil
}

func addVary(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		for _, item := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}
