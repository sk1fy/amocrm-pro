package amocrm

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestWidgetActivationProfileWithoutRole(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RequestURI(); got != "/api/v4/users/7" {
			t.Errorf("unexpected profile request: %s", got)
		}
		fmt.Fprint(w, `{"id":7,"email":"employee@example.invalid","rights":{"is_active":true,"is_admin":true,"role_id":123}}`)
	})
	profile, err := client.WidgetActivationProfile(context.Background(), uuid.New(), 7, false)
	if err != nil || profile.Email != "employee@example.invalid" || profile.Role != "" {
		t.Fatalf("activation profile = %+v, err = %v", profile, err)
	}
}
