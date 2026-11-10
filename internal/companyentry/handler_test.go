package companyentry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type memoryStore struct {
	company uuid.UUID
	fail    bool
	active  bool
}

func (s *memoryStore) Active(context.Context, widgetauth.Principal) error {
	if !s.active {
		return ErrDenied
	}
	return nil
}
func (s *memoryStore) Associated(context.Context, int64) (uuid.UUID, error) { return s.company, nil }
func (s *memoryStore) Associate(_ context.Context, _ int64, c uuid.UUID) error {
	if s.fail {
		return errors.New("db unavailable")
	}
	if s.company != uuid.Nil && s.company != c {
		return ErrConflict
	}
	s.company = c
	return nil
}

type profileReader struct {
	profile amocrm.CompanyEntryProfile
	err     error
}

func (s *profileReader) CompanyEntryProfile(context.Context, uuid.UUID, int64) (amocrm.CompanyEntryProfile, error) {
	return s.profile, s.err
}

type authority struct {
	company, user uuid.UUID
	calls         int
	last          EntryInput
	status        Status
	err           error
}

func (a *authority) Status(context.Context, string) (Status, error) { return a.status, a.err }
func (a *authority) Enter(_ context.Context, in EntryInput) (Entry, error) {
	a.calls++
	a.last = in
	return Entry{CompanyID: a.company, UserID: a.user, Code: strings.Repeat("x", 43), ExpiresAt: time.Now().Add(time.Minute)}, a.err
}
func testHandler() (*Handler, *memoryStore, *profileReader, *authority) {
	active := true
	s := &memoryStore{active: true}
	p := &profileReader{profile: amocrm.CompanyEntryProfile{ID: 7, Name: "CRM admin", Email: "admin@example.test", Rights: amocrm.CRMRights{IsAdmin: true, IsActive: &active}}}
	a := &authority{company: uuid.New(), user: uuid.New()}
	return &Handler{Store: s, CRM: p, Authority: a, FrontendOrigin: "https://teamos.example.test", SubjectCheckKey: strings.Repeat("k", 32)}, s, p, a
}
func callEntry(h *Handler, body string) *httptest.ResponseRecorder {
	p := widgetauth.Principal{AccountID: 42, UserID: 7, IntegrationID: uuid.New(), InstallationID: uuid.New()}
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	h.Enter(w, r)
	return w
}
func TestEntryUsesOnlyAuthenticatedOwnProfile(t *testing.T) {
	h, _, _, a := testHandler()
	w := callEntry(h, `{"intent":"create"}`)
	if w.Code != 200 || a.last.AccountID != "42" || a.last.ActorID != "7" || a.last.Email != "admin@example.test" || strings.Contains(w.Body.String(), a.last.Email) || !strings.Contains(w.Body.String(), "/auth/amo#code=") {
		t.Fatal(w.Code, w.Body.String(), a.last)
	}
	w = callEntry(h, `{"intent":"enter","actorId":"1","email":"attacker@test.invalid"}`)
	if w.Code != 400 || a.calls != 1 {
		t.Fatal("browser principal accepted", w.Code, a.calls)
	}
}
func TestEntryLiveRightsAndUnavailable(t *testing.T) {
	for _, kind := range []string{"wrong_actor", "missing_active", "inactive", "free", "nonadmin", "upstream", "installation"} {
		t.Run(kind, func(t *testing.T) {
			h, s, p, a := testHandler()
			want := 403
			switch kind {
			case "wrong_actor":
				p.profile.ID = 8
			case "missing_active":
				p.profile.Rights.IsActive = nil
			case "inactive":
				v := false
				p.profile.Rights.IsActive = &v
			case "free":
				p.profile.Rights.IsFree = true
			case "nonadmin":
				p.profile.Rights.IsAdmin = false
			case "upstream":
				p.err = errors.New("timeout")
				want = 503
			case "installation":
				s.active = false
			}
			w := callEntry(h, `{"intent":"create"}`)
			if w.Code != want || a.calls != 0 {
				t.Fatal(w.Code, a.calls)
			}
		})
	}
}
func TestEntryLostMappingCommitFreshRetryRepairsSameCompany(t *testing.T) {
	h, s, _, a := testHandler()
	s.fail = true
	w := callEntry(h, `{"intent":"create"}`)
	if w.Code != 503 || strings.Contains(w.Body.String(), "code=") {
		t.Fatal(w.Code, w.Body.String())
	}
	s.fail = false
	w = callEntry(h, `{"intent":"create"}`)
	if w.Code != 200 || s.company != a.company || a.calls != 2 {
		t.Fatal(w.Code, s.company, a.calls)
	}
	other := uuid.New()
	a.company = other
	w = callEntry(h, `{"intent":"create"}`)
	if w.Code != 409 || s.company == other || a.last.Intent != "enter" {
		t.Fatal("mapping overwritten or replacement requested", w.Code, s.company, a.last.Intent)
	}
}
func TestStatusNeverReplacesErrorWithAbsent(t *testing.T) {
	h, s, _, a := testHandler()
	a.err = errors.New("authority unavailable")
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	r = r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), widgetauth.Principal{AccountID: 42, UserID: 7}))
	w := httptest.NewRecorder()
	h.Status(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "absent") {
		t.Fatal(w.Code, w.Body.String())
	}
	a.err = nil
	a.status = Status{State: "absent"}
	s.company = uuid.New()
	w = httptest.NewRecorder()
	r.Body = http.NoBody
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{}`)).WithContext(r.Context())
	h.Status(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "blocked") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSubjectCheckRequiresDistinctServiceCredentialAndLiveRights(t *testing.T) {
	h, _, p, _ := testHandler()
	body := `{"accountId":"42","actorId":"7","integrationId":"` + uuid.NewString() + `","installationId":"` + uuid.NewString() + `"}`
	for _, credential := range []string{"", "Bearer " + h.SubjectCheckKey, "Service wrong", "Service " + h.SubjectCheckKey} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Authorization", credential)
		w := httptest.NewRecorder()
		h.SubjectCheck(w, r)
		want := 401
		if credential == "Service "+h.SubjectCheckKey {
			want = 200
		}
		if w.Code != want {
			t.Fatal(credential, w.Code)
		}
	}
	p.profile.Rights.IsAdmin = false
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Authorization", "Service "+h.SubjectCheckKey)
	w := httptest.NewRecorder()
	h.SubjectCheck(w, r)
	if w.Code != 403 {
		t.Fatal("revoked admin accepted", w.Code)
	}
}
