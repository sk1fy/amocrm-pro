package amostart

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func billPage(period string) string {
	return fmt.Sprintf(`<div><div><label for="payment_period_end">Период</label></div>
<div class="row_field"><input value="%s"></div></div>
<div class="row_field row_field2 tariff_value"><input value="Расширенный"></div>`, period)
}

func TestLookupUsesPartnerSessionAndConfirmedDate(t *testing.T) {
	signIns := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cabinet:
			fmt.Fprint(w, `<input type="hidden" name="csrf_token" value="token">`)
		case "/oauth2/authorize":
			signIns++
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("unexpected authorization request")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "mock", Path: "/"})
		case bill, cabinet + "shop/addbillpartners":
			if _, err := r.Cookie("session"); err != nil {
				t.Error("partner session cookie missing")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == bill {
				return
			}
			if r.Method != http.MethodPost || r.FormValue("USER_CONTROL") != "1" {
				t.Error("unexpected billing preview request")
			}
			switch r.FormValue("account_id") {
			case "10001":
				fmt.Fprint(w, billPage("с 01.01.2026 до 15.03.2027 00:00:00"))
			case "10002":
				fmt.Fprint(w, billPage("15.03.2027"))
			default:
				t.Error("unexpected account ID")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer mock.Close()
	client, err := newClient(Config{Username: "partner", Password: "mock", Contract: "1", PartnerID: "2", UserID: "3"}, mock.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.limit = rate.NewLimiter(rate.Inf, 1)
	license, err := client.Lookup(context.Background(), 10001)
	if err != nil || license.Until.Format(time.RFC3339) != "2027-03-15T00:00:00+03:00" || license.Tariff != "Расширенный" {
		t.Fatalf("unexpected license %+v: %v", license, err)
	}
	if _, err := client.Lookup(context.Background(), 10002); !errors.Is(err, ErrNoLicense) {
		t.Fatalf("bare date was accepted: %v", err)
	}
	if signIns != 1 {
		t.Fatalf("partner session was not reused, sign-ins: %d", signIns)
	}
}
