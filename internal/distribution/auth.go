package distribution

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxBody = 65536

var ErrDenied = errors.New("permission denied")
var ErrUnavailable = errors.New("source unavailable")
var ErrConflict = errors.New("binding conflict")
var ErrNotFound = errors.New("resource not found")

type Scope struct {
	KeyID          string
	CompanyID      uuid.UUID
	InstallationID uuid.UUID
}
type scopeKey struct{}
type AuthStore interface {
	AuthorizeRequest(context.Context, Scope, uuid.UUID, time.Time) error
}
type Auth struct {
	Keys  map[string]string
	Store AuthStore
	Clock func() time.Time
}

func ParseKeys(raw string) (map[string]string, error) {
	var keys map[string]string
	if json.Unmarshal([]byte(raw), &keys) != nil || len(keys) == 0 || len(keys) > 8 {
		return nil, errors.New("invalid distribution service keys")
	}
	for k, v := range keys {
		if len(k) == 0 || len(k) > 64 || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(k) || len(v) < 32 || len(v) > 512 {
			return nil, errors.New("invalid distribution service keys")
		}
	}
	return keys, nil
}
func Signature(secret, keyID, method, uri, company, installation, timestamp, nonce string, body []byte) string {
	hash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join([]string{keyID, method, uri, company, installation, timestamp, nonce, hex.EncodeToString(hash[:])}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}
func Sign(r *http.Request, scope Scope, secret string, body []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := uuid.NewString()
	headers := map[string]string{"X-Distribution-Key-Id": scope.KeyID, "X-Distribution-Company": scope.CompanyID.String(), "X-Distribution-Installation": scope.InstallationID.String(), "X-Distribution-Timestamp": timestamp, "X-Distribution-Nonce": nonce}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r.Header.Set("X-Distribution-Signature", Signature(secret, scope.KeyID, r.Method, r.URL.RequestURI(), scope.CompanyID.String(), scope.InstallationID.String(), timestamp, nonce, body))
}
func single(r *http.Request, name string) string {
	v := r.Header.Values(name)
	if len(v) != 1 {
		return ""
	}
	return v[0]
}
func (a Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			fail(w, 400, "validation_failed")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		company, ce := uuid.Parse(single(r, "X-Distribution-Company"))
		install, ie := uuid.Parse(single(r, "X-Distribution-Installation"))
		nonce, ne := uuid.Parse(single(r, "X-Distribution-Nonce"))
		stamp := single(r, "X-Distribution-Timestamp")
		sec, se := strconv.ParseInt(stamp, 10, 64)
		kid := single(r, "X-Distribution-Key-Id")
		secret := a.Keys[kid]
		sig, de := hex.DecodeString(single(r, "X-Distribution-Signature"))
		now := time.Now()
		if a.Clock != nil {
			now = a.Clock()
		}
		expected, _ := hex.DecodeString(Signature(secret, kid, r.Method, r.URL.RequestURI(), single(r, "X-Distribution-Company"), single(r, "X-Distribution-Installation"), stamp, single(r, "X-Distribution-Nonce"), body))
		if ce != nil || ie != nil || ne != nil || se != nil || de != nil || company == uuid.Nil || install == uuid.Nil || nonce == uuid.Nil || secret == "" || now.Sub(time.Unix(sec, 0)) > 60*time.Second || time.Unix(sec, 0).Sub(now) > 60*time.Second || !hmac.Equal(sig, expected) {
			fail(w, 401, "unauthenticated")
			return
		}
		scope := Scope{kid, company, install}
		if a.Store == nil {
			fail(w, 503, "policy_unavailable")
			return
		}
		if err = a.Store.AuthorizeRequest(r.Context(), scope, nonce, now.Add(5*time.Minute)); err != nil {
			if errors.Is(err, ErrDenied) {
				fail(w, 401, "unauthenticated")
			} else {
				fail(w, 503, "policy_unavailable")
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, scope)))
	})
}
func scopeFrom(r *http.Request) Scope { s, _ := r.Context().Value(scopeKey{}).(Scope); return s }
func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": "Запрос не выполнен. Проверьте подключение и права доступа."}})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
