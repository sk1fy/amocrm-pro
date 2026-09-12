package adminread

import (
	"testing"
	"time"
)

func TestNormalizeAccountQuery(t *testing.T) {
	tests := []struct {
		in        string
		accountID int64
		domain    string
		subdomain string
	}{
		{in: ""},
		{in: "   "},
		{in: "31415926", accountID: 31415926},
		{in: "  42  ", accountID: 42},
		{in: "https://fixture-a.amocrm.test/leads", domain: "fixture-a.amocrm.test"},
		{in: "http://Fixture-B.kommo.test", domain: "fixture-b.kommo.test"},
		{in: "https://fixture-c.amocrm.test:443/path", domain: "fixture-c.amocrm.test"},
		{in: "fixture-d.amocrm.test", domain: "fixture-d.amocrm.test"},
		{in: "acme", subdomain: "acme"},
		{in: "  ACME ", subdomain: "acme"},
	}
	for _, test := range tests {
		got := normalizeAccountQuery(test.in)
		var accountID int64
		if got.AccountID != nil {
			accountID = *got.AccountID
		}
		if accountID != test.accountID || got.Domain != test.domain || got.Subdomain != test.subdomain {
			t.Fatalf("q=%q got account=%d domain=%q subdomain=%q", test.in, accountID, got.Domain, got.Subdomain)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 12, 10, 5, 0, 123456789, time.UTC)
	encoded := encodeCursor(at, "11111111-1111-1111-1111-111111111111")
	gotTime, gotID, err := decodeCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(at) || gotID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("round trip %v %q", gotTime, gotID)
	}
	if _, _, err := decodeCursor("%%%"); err == nil {
		t.Fatal("expected invalid cursor")
	}
	if _, _, err := decodeCursor(""); err != nil {
		t.Fatal(err)
	}
}

func TestParseLimitClamps(t *testing.T) {
	got, err := parseLimit("")
	if err != nil || got != 25 {
		t.Fatalf("default=%d %v", got, err)
	}
	got, err = parseLimit("101")
	if err != nil || got != 100 {
		t.Fatalf("clamped=%d %v", got, err)
	}
	if _, err := parseLimit("0"); err == nil {
		t.Fatal("expected invalid limit")
	}
}

func TestParseJobsSinceRejectsWindowOverSevenDays(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if _, err := parseJobsSince(now.Add(-jobsWindow-time.Second).Format(time.RFC3339), now); err == nil {
		t.Fatal("expected window rejection")
	}
	got, err := parseJobsSince(now.Add(-jobsWindow).Format(time.RFC3339), now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now.Add(-jobsWindow)) {
		t.Fatalf("boundary since=%v", got)
	}
	got, err = parseJobsSince("", now)
	if err != nil || !got.Equal(now.Add(-jobsWindow)) {
		t.Fatalf("default since=%v %v", got, err)
	}
	if _, err := parseJobsSince("not-a-time", now); err == nil {
		t.Fatal("expected invalid since")
	}
}
