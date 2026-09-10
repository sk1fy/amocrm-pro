package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/oauthlimit"
)

type fakeFlow struct {
	start    func(context.Context, string, string) (string, error)
	callback func(context.Context, string, string, string) (InstallationResult, error)
}

func (f fakeFlow) Start(ctx context.Context, code, returnURL string) (string, error) {
	return f.start(ctx, code, returnURL)
}

func (f fakeFlow) Callback(ctx context.Context, state, code, referer string) (InstallationResult, error) {
	return f.callback(ctx, state, code, referer)
}

func testHandler(flow fakeFlow, consume func(context.Context, string)) *Handler {
	if consume == nil {
		consume = func(context.Context, string) {}
	}
	return &Handler{flow: flow, consume: consume, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func requestIDRecorder() *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "11111111-1111-1111-1111-111111111111")
	return w
}

func decodeOAuthError(t *testing.T, body []byte) (code, message, requestID string, retryable bool) {
	t.Helper()
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, body)
	}
	return payload.Error.Code, payload.Error.Message, payload.Error.RequestID, payload.Error.Retryable
}

func TestStartKeepsUnknownIntegrationIndistinguishable(t *testing.T) {
	var seen []string
	h := testHandler(fakeFlow{start: func(_ context.Context, code, _ string) (string, error) {
		seen = append(seen, code)
		if code == "known" {
			return "", errors.New("database unavailable")
		}
		return "", ErrIntegrationNotFound
	}}, nil)
	bodies := map[string]string{}
	for _, code := range []string{"known", "unknown"} {
		req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code="+code, nil)
		w := requestIDRecorder()
		h.Start(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", code, w.Code, w.Body.String())
		}
		bodies[code] = w.Body.String()
		gotCode, message, requestID, retryable := decodeOAuthError(t, w.Body.Bytes())
		if gotCode != "invalid_argument" || message != "cannot start authorization" || retryable ||
			requestID != "11111111-1111-1111-1111-111111111111" || strings.Contains(w.Body.String(), code) {
			t.Fatalf("%s envelope=%s", code, w.Body.String())
		}
	}
	if bodies["known"] != bodies["unknown"] {
		t.Fatalf("start error bodies differ: %q vs %q", bodies["known"], bodies["unknown"])
	}
	if len(seen) != 2 {
		t.Fatal("start was not invoked")
	}
}

func TestStartRedirectsWhenAuthorizationBegins(t *testing.T) {
	h := testHandler(fakeFlow{start: func(context.Context, string, string) (string, error) {
		return "https://www.amocrm.ru/oauth?client_id=x&state=y&mode=popup", nil
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code=known", nil)
	w := requestIDRecorder()
	h.Start(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://www.amocrm.ru/oauth?client_id=x&state=y&mode=popup" {
		t.Fatalf("redirect: status=%d location=%s", w.Code, w.Header().Get("Location"))
	}
}

func TestStartRejectsOversizedQueryWithoutCallingFlow(t *testing.T) {
	called := false
	h := testHandler(fakeFlow{start: func(context.Context, string, string) (string, error) {
		called = true
		return "", nil
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code="+strings.Repeat("a", maxIntegrationCode+1), nil)
	w := requestIDRecorder()
	h.Start(w, req)
	if called || w.Code != http.StatusBadRequest {
		t.Fatalf("oversized start: called=%t status=%d", called, w.Code)
	}
	code, message, _, retryable := decodeOAuthError(t, w.Body.Bytes())
	if code != "invalid_argument" || message != "cannot start authorization" || retryable {
		t.Fatalf("oversized envelope=%s", w.Body.String())
	}
}

func TestCallbackCreatesInstallationAndMapsErrors(t *testing.T) {
	result := InstallationResult{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), AccountID: 7, Status: "active"}
	h := testHandler(fakeFlow{callback: func(_ context.Context, state, code, referer string) (InstallationResult, error) {
		if state != "state-1" || code != "auth-code" || referer != "https://example.amocrm.ru" {
			t.Fatalf("callback args %q %q %q", state, code, referer)
		}
		return result, nil
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=state-1&code=auth-code&referer=https://example.amocrm.ru", nil)
	w := requestIDRecorder()
	h.Callback(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("created: %d %s", w.Code, w.Body.String())
	}
	var got InstallationResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got != result {
		t.Fatalf("result=%+v err=%v", got, err)
	}

	h = testHandler(fakeFlow{callback: func(context.Context, string, string, string) (InstallationResult, error) {
		return InstallationResult{}, ErrInvalidState
	}}, nil)
	w = requestIDRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=used&code=x&referer=https://example.amocrm.ru", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid state status=%d", w.Code)
	}
	code, message, _, retryable := decodeOAuthError(t, w.Body.Bytes())
	if code != "invalid_argument" || message != "authorization failed" || retryable {
		t.Fatalf("invalid state envelope=%s", w.Body.String())
	}

	h = testHandler(fakeFlow{callback: func(context.Context, string, string, string) (InstallationResult, error) {
		return InstallationResult{}, errors.New("token endpoint 502")
	}}, nil)
	w = requestIDRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=ok&code=x&referer=https://example.amocrm.ru", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("upstream status=%d", w.Code)
	}
	code, message, _, retryable = decodeOAuthError(t, w.Body.Bytes())
	if code != "unavailable" || message != "authorization failed" || !retryable || strings.Contains(w.Body.String(), "token endpoint") {
		t.Fatalf("upstream envelope=%s", w.Body.String())
	}
}

func TestCallbackDeniedConsumesStateAndUsesGenericEnvelope(t *testing.T) {
	consumed := ""
	h := testHandler(fakeFlow{}, func(_ context.Context, state string) { consumed = state })
	w := requestIDRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=once&error=access_denied", nil))
	if consumed != "once" || w.Code != http.StatusBadRequest {
		t.Fatalf("denied: consumed=%q status=%d", consumed, w.Code)
	}
	code, message, _, retryable := decodeOAuthError(t, w.Body.Bytes())
	if code != "invalid_argument" || message != "authorization failed" || retryable || strings.Contains(w.Body.String(), "access_denied") {
		t.Fatalf("denied envelope=%s", w.Body.String())
	}
}

func TestCallbackRejectsOversizedStateWithoutConsume(t *testing.T) {
	called := false
	consumed := false
	h := testHandler(fakeFlow{callback: func(context.Context, string, string, string) (InstallationResult, error) {
		called = true
		return InstallationResult{}, nil
	}}, func(context.Context, string) { consumed = true })
	w := requestIDRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state="+strings.Repeat("s", maxOAuthState+1)+"&code=x", nil))
	if called || consumed || w.Code != http.StatusBadRequest {
		t.Fatalf("oversized state: called=%t consumed=%t status=%d", called, consumed, w.Code)
	}
}

func TestLimiterHidesIntegrationExistenceOnStartAndCallback(t *testing.T) {
	flow := fakeFlow{
		start: func(_ context.Context, code, _ string) (string, error) {
			if code == "known" {
				return "https://www.amocrm.ru/oauth?state=y", nil
			}
			return "", ErrIntegrationNotFound
		},
		callback: func(_ context.Context, state, _, _ string) (InstallationResult, error) {
			if state == "known" {
				return InstallationResult{AccountID: 1, Status: "active"}, nil
			}
			return InstallationResult{}, ErrInvalidState
		},
	}
	h := testHandler(flow, nil)
	limiter, err := oauthlimit.New(oauthlimit.Config{
		IPRate: 1, IPBurst: 1, IdentityRate: 1, IdentityBurst: 1,
		InactiveTTL: 10 * time.Second, MaxEntries: 100,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := limiter.Middleware(http.HandlerFunc(h.Start))
	knownStart := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code=known", nil)
	knownStart.RemoteAddr = "203.0.113.10:1"
	w := requestIDRecorder()
	start.ServeHTTP(w, knownStart)
	if w.Code != http.StatusFound {
		t.Fatalf("unlimited start: %d %s", w.Code, w.Body.String())
	}
	var startBodies []string
	for _, code := range []string{"known", "unknown"} {
		req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code="+code, nil)
		req.RemoteAddr = "203.0.113.10:1"
		rec := requestIDRecorder()
		start.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("start %s status=%d body=%s", code, rec.Code, rec.Body.String())
		}
		gotCode, message, _, retryable := decodeOAuthError(t, rec.Body.Bytes())
		if gotCode != "rate_limited" || message != "rate limited" || !retryable || strings.Contains(rec.Body.String(), code) {
			t.Fatalf("start limited envelope=%s", rec.Body.String())
		}
		startBodies = append(startBodies, rec.Body.String())
	}
	if startBodies[0] != startBodies[1] {
		t.Fatalf("start 429 bodies differ: %q vs %q", startBodies[0], startBodies[1])
	}

	limiter, err = oauthlimit.New(oauthlimit.Config{
		IPRate: 1, IPBurst: 1, IdentityRate: 1, IdentityBurst: 1,
		InactiveTTL: 10 * time.Second, MaxEntries: 100,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	callback := limiter.Middleware(http.HandlerFunc(h.Callback))
	knownCallback := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=known&code=x&referer=https://example.amocrm.ru", nil)
	knownCallback.RemoteAddr = "198.51.100.9:1"
	w = requestIDRecorder()
	callback.ServeHTTP(w, knownCallback)
	if w.Code != http.StatusCreated {
		t.Fatalf("unlimited callback: %d %s", w.Code, w.Body.String())
	}
	var callbackBodies []string
	for _, state := range []string{"known", "unknown"} {
		req := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state="+state+"&code=x&referer=https://example.amocrm.ru", nil)
		req.RemoteAddr = "198.51.100.9:1"
		rec := requestIDRecorder()
		callback.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("callback %s status=%d body=%s", state, rec.Code, rec.Body.String())
		}
		callbackBodies = append(callbackBodies, rec.Body.String())
	}
	if callbackBodies[0] != callbackBodies[1] {
		t.Fatalf("callback 429 bodies differ: %q vs %q", callbackBodies[0], callbackBodies[1])
	}
}
