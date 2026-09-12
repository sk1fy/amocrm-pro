package adminread

import (
	"testing"
	"time"
)

func TestMapAuthorization(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	earlier := now.Add(-time.Hour)
	leaseActive := now.Add(time.Minute)
	leaseExpired := now.Add(-time.Minute)

	tests := []struct {
		name        string
		facts       AuthorizationFacts
		state       string
		leaseActive bool
		present     bool
	}{
		{name: "missing credentials", facts: AuthorizationFacts{InstallationStatus: "active"}, state: authMissing},
		{name: "pending missing", facts: AuthorizationFacts{InstallationStatus: "pending"}, state: authMissing},
		{name: "uninstalled missing", facts: AuthorizationFacts{InstallationStatus: "uninstalled"}, state: authMissing},
		{name: "unknown status missing", facts: AuthorizationFacts{InstallationStatus: "unexpected"}, state: authMissing},
		{
			name: "reauth_required",
			facts: AuthorizationFacts{
				InstallationStatus: authReauthRequired, Present: true, ExpiresAt: &later, TokenVersion: 3, KeyVersion: 1,
			},
			state: authReauthRequired, present: true,
		},
		{
			name: "reauth beats lease",
			facts: AuthorizationFacts{
				InstallationStatus: authReauthRequired, Present: true, ExpiresAt: &later, LeaseUntil: &leaseActive,
			},
			state: authReauthRequired, present: true, leaseActive: true,
		},
		{
			name: "refreshing",
			facts: AuthorizationFacts{
				InstallationStatus: "active", Present: true, ExpiresAt: &later, LeaseUntil: &leaseActive,
			},
			state: authRefreshing, present: true, leaseActive: true,
		},
		{
			name: "expired_refreshable",
			facts: AuthorizationFacts{
				InstallationStatus: "active", Present: true, ExpiresAt: &earlier,
			},
			state: authExpiredRefreshable, present: true,
		},
		{
			name: "expired at exactly now",
			facts: AuthorizationFacts{
				InstallationStatus: "active", Present: true, ExpiresAt: &now,
			},
			state: authExpiredRefreshable, present: true,
		},
		{
			name: "expired lease does not refresh",
			facts: AuthorizationFacts{
				InstallationStatus: "active", Present: true, ExpiresAt: &later, LeaseUntil: &leaseExpired,
			},
			state: authValid, present: true,
		},
		{
			name: "valid",
			facts: AuthorizationFacts{
				InstallationStatus: "active", Present: true, ExpiresAt: &later, TokenVersion: 2, KeyVersion: 1,
			},
			state: authValid, present: true,
		},
		{
			name: "unknown status still maps valid",
			facts: AuthorizationFacts{
				InstallationStatus: "unexpected", Present: true, ExpiresAt: &later,
			},
			state: authValid, present: true,
		},
		{
			name: "disabled with valid credentials",
			facts: AuthorizationFacts{
				InstallationStatus: "disabled", Present: true, ExpiresAt: &later,
			},
			state: authValid, present: true,
		},
		{
			name: "error expired",
			facts: AuthorizationFacts{
				InstallationStatus: "error", Present: true, ExpiresAt: &earlier,
			},
			state: authExpiredRefreshable, present: true,
		},
		{
			name: "authorizing valid",
			facts: AuthorizationFacts{
				InstallationStatus: "authorizing", Present: true, ExpiresAt: &later,
			},
			state: authValid, present: true,
		},
		{
			name:  "present without expiry is expired_refreshable",
			facts: AuthorizationFacts{InstallationStatus: "active", Present: true},
			state: authExpiredRefreshable, present: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MapAuthorization(test.facts, now)
			if got.State != test.state {
				t.Fatalf("state=%q want %q", got.State, test.state)
			}
			if got.CredentialsPresent != test.present {
				t.Fatalf("present=%t want %t", got.CredentialsPresent, test.present)
			}
			if got.LeaseActive != test.leaseActive {
				t.Fatalf("lease_active=%t want %t", got.LeaseActive, test.leaseActive)
			}
			if !got.Unverified {
				t.Fatal("unverified must be true on stage 1")
			}
		})
	}
}

func TestPilotState(t *testing.T) {
	enabled := true
	disabled := false
	if pilotState(nil) != pilotNotConfigured || pilotState(&enabled) != pilotEnabled || pilotState(&disabled) != pilotDisabled {
		t.Fatal("pilot mapping")
	}
}
