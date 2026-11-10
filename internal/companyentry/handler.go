package companyentry

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

var canonicalID = regexp.MustCompile(`^[1-9][0-9]*$`)
var opaqueCode = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

type Handler struct {
	Store                           Repository
	CRM                             IdentityReader
	Authority                       Authority
	FrontendOrigin, SubjectCheckKey string
	Now                             func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil && decoder.Decode(new(any)) == io.EOF
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int) {
	message := "Вход недоступен. Вернитесь в виджет и повторите попытку."
	if status == 403 {
		message = "Вход доступен только действующему администратору amoCRM."
	}
	if status == 422 {
		message = "Не удалось получить email из профиля amoCRM. Проверьте профиль и повторите вход из виджета."
	}
	if status == 409 {
		message = "Состояние компании изменилось. Обновите виджет."
	}
	reply(w, status, map[string]any{"error": map[string]any{"status": status, "message": message}})
}
func (h *Handler) live(ctx context.Context, p widgetauth.Principal) (amocrm.CompanyEntryProfile, error) {
	if err := h.Store.Active(ctx, p); err != nil {
		return amocrm.CompanyEntryProfile{}, err
	}
	profile, err := h.CRM.CompanyEntryProfile(ctx, p.InstallationID, p.UserID)
	if err != nil {
		return profile, err
	}
	if profile.ID != p.UserID || profile.Rights.IsActive == nil || !*profile.Rights.IsActive || profile.Rights.IsFree || !profile.Rights.IsAdmin {
		return profile, ErrDenied
	}
	return profile, h.Store.Active(ctx, p)
}
func respondError(w http.ResponseWriter, err error) {
	status := 503
	var upstream *UpstreamError
	if errors.Is(err, ErrDenied) {
		status = 403
	} else if errors.Is(err, ErrConflict) {
		status = 409
	} else if errors.As(err, &upstream) && (upstream.Status == 403 || upstream.Status == 409 || upstream.Status == 422) {
		status = upstream.Status
	}
	failure(w, status)
}
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !decode(w, r, &in) {
		failure(w, 400)
		return
	}
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		failure(w, 401)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if _, err := h.live(ctx, p); err != nil {
		respondError(w, err)
		return
	}
	result, err := h.Authority.Status(ctx, strconv.FormatInt(p.AccountID, 10))
	if err != nil {
		respondError(w, err)
		return
	}
	if result.State == "present" {
		if err = h.Store.Associate(ctx, p.AccountID, result.CompanyID); err != nil {
			respondError(w, err)
			return
		}
	} else if result.State == "absent" {
		associated, e := h.Store.Associated(ctx, p.AccountID)
		if e != nil {
			respondError(w, e)
			return
		}
		if associated != uuid.Nil {
			result.State = "blocked"
		}
	}
	reply(w, 200, result)
}
func (h *Handler) Enter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Intent string `json:"intent"`
	}
	if !decode(w, r, &in) || (in.Intent != "create" && in.Intent != "enter") {
		failure(w, 400)
		return
	}
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		failure(w, 401)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	profile, err := h.live(ctx, p)
	if err != nil {
		respondError(w, err)
		return
	}
	if in.Intent == "create" {
		previous, e := h.Store.Associated(ctx, p.AccountID)
		if e != nil {
			respondError(w, e)
			return
		}
		if previous != uuid.Nil {
			in.Intent = "enter"
		}
	}
	result, err := h.Authority.Enter(ctx, EntryInput{Subject: Subject{AccountID: strconv.FormatInt(p.AccountID, 10), ActorID: strconv.FormatInt(p.UserID, 10), IntegrationID: p.IntegrationID, InstallationID: p.InstallationID}, Intent: in.Intent, Name: profile.Name, Email: profile.Email})
	if err != nil {
		respondError(w, err)
		return
	}
	now := h.now()
	if result.CompanyID == uuid.Nil || result.UserID == uuid.Nil || !opaqueCode.MatchString(result.Code) || !result.ExpiresAt.After(now) || result.ExpiresAt.After(now.Add(65*time.Second)) {
		failure(w, 503)
		return
	}
	if err = h.Store.Associate(ctx, p.AccountID, result.CompanyID); err != nil {
		respondError(w, err)
		return
	}
	reply(w, 200, map[string]any{"redirectUrl": h.FrontendOrigin + "/auth/amo#code=" + url.QueryEscape(result.Code), "expiresAt": result.ExpiresAt})
}
func (h *Handler) SubjectCheck(w http.ResponseWriter, r *http.Request) {
	expected := sha256.Sum256([]byte("Service " + h.SubjectCheckKey))
	actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if len(h.SubjectCheckKey) < 32 || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		failure(w, 401)
		return
	}
	var in Subject
	if !decode(w, r, &in) || !canonicalID.MatchString(in.AccountID) || !canonicalID.MatchString(in.ActorID) || in.IntegrationID == uuid.Nil || in.InstallationID == uuid.Nil {
		failure(w, 400)
		return
	}
	account, e1 := strconv.ParseInt(in.AccountID, 10, 64)
	actor, e2 := strconv.ParseInt(in.ActorID, 10, 64)
	if e1 != nil || e2 != nil {
		failure(w, 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := h.live(ctx, widgetauth.Principal{AccountID: account, UserID: actor, IntegrationID: in.IntegrationID, InstallationID: in.InstallationID}); err != nil {
		respondError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"allowed": true})
}
