package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const widgetActivationPath = "/api/v1/widget/activation"

func registerWidgetActivation(router chi.Router, store *corelifecycle.Store, crm *amocrm.Client, client *corelifecycle.ActivationClient,
	protect func(bool, http.Handler) http.Handler, cors func(http.Handler) http.Handler,
) {
	handler := protect(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) { platformaccess.WriteError(w, r.Context(), status, code, 0) }
		var input struct {
			Phone   string `json:"phone"`
			Consent string `json:"consent_version"`
		}
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
			if err != nil || wire.Decode(raw, &input) != nil || input.Consent != wire.ContactConsent {
				fail(http.StatusBadRequest, "invalid_activation_contact")
				return
			}
			input.Phone, err = wire.ContactPhone(input.Phone)
			if err != nil {
				fail(http.StatusBadRequest, "invalid_activation_contact")
				return
			}
		}
		principal, ok := widgetauth.PrincipalFromContext(r.Context())
		if !ok {
			fail(http.StatusUnauthorized, "unauthenticated")
			return
		}
		scope, err := store.AccessProofScope(r.Context(), principal)
		if errors.Is(err, corelifecycle.ErrAccessProofPolicy) || errors.Is(err, corelifecycle.ErrAccessProofDenied) {
			fail(http.StatusNotFound, "activation_unavailable")
			return
		}
		if err != nil {
			fail(http.StatusServiceUnavailable, "unavailable")
			return
		}
		request := wire.ActivationRequest{Scope: wire.ActivationScope{
			Installation: scope.Installation, Integration: scope.Integration, Account: principal.AccountID,
			Generation: scope.Generation, AuthorizationSequence: scope.AuthorizationSequence,
		}}
		result, err := client.Apply(r.Context(), request)
		if errors.Is(err, corelifecycle.ErrActivationUnavailable) {
			fail(http.StatusNotFound, "activation_unavailable")
			return
		}
		if err != nil {
			fail(http.StatusServiceUnavailable, "unavailable")
			return
		}
		confirmed := func() {
			if err := store.ConfirmActivation(r.Context(), principal.InstallationID, scope.Generation); err != nil {
				fail(http.StatusServiceUnavailable, "unavailable")
				return
			}
			platformaccess.WriteJSON(w, http.StatusOK, result)
		}
		if result.Activated {
			confirmed()
			return
		}
		profile, err := crm.WidgetActivationProfile(r.Context(), principal.InstallationID, principal.UserID)
		if err != nil {
			fail(http.StatusServiceUnavailable, "activation_profile_unavailable")
			return
		}
		if r.Method == http.MethodGet {
			platformaccess.WriteJSON(w, http.StatusOK, struct {
				Activated bool                     `json:"activated"`
				Profile   amocrm.ActivationProfile `json:"profile"`
			}{Profile: profile})
			return
		}
		request.Contact = &wire.ActivationContact{UserID: principal.UserID, Name: profile.Name, Role: profile.Role, Email: profile.Email,
			Phone: input.Phone, Consent: input.Consent}
		result, err = client.Apply(r.Context(), request)
		if err != nil || !result.Activated {
			fail(http.StatusServiceUnavailable, "unavailable")
			return
		}
		confirmed()
	}))
	router.Method(http.MethodGet, widgetActivationPath, handler)
	router.Method(http.MethodPost, widgetActivationPath, handler)
	router.Method(http.MethodOptions, widgetActivationPath, cors(http.NotFoundHandler()))
}
