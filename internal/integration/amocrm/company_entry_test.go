package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"testing"
)

func TestCompanyEntryProfileDecodesServerEmailAndExactRights(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"profile", `{"id":7,"name":"Admin","email":"admin@example.test","rights":{"is_active":true,"is_admin":true,"is_free":false}}`, true},
		{"optional_email", `{"id":7,"name":"Admin","rights":{"is_active":true,"is_admin":true}}`, true},
		{"wrong_actor", `{"id":8,"email":"owner@example.test","rights":{"is_active":true,"is_admin":true}}`, false},
		{"missing_active", `{"id":7,"email":"admin@example.test","rights":{"is_admin":true}}`, false},
		{"malformed", `{"id":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/users/7" {
					t.Error(r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			})
			profile, err := c.CompanyEntryProfile(context.Background(), uuid.New(), 7)
			if (err == nil) != tc.ok {
				t.Fatal(profile, err)
			}
			if tc.name == "profile" && profile.Email != "admin@example.test" {
				t.Fatal("email lost")
			}
		})
	}
}
