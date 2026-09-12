package adminread

import "time"

func MapAuthorization(facts AuthorizationFacts, now time.Time) Authorization {
	leaseActive := facts.LeaseUntil != nil && facts.LeaseUntil.After(now)
	out := Authorization{
		CredentialsPresent: facts.Present,
		ExpiresAt:          facts.ExpiresAt,
		RefreshedAt:        facts.RefreshedAt,
		LeaseActive:        leaseActive,
		Unverified:         true,
	}
	if facts.Present {
		out.CredentialVersion = facts.TokenVersion
		out.KeyVersion = facts.KeyVersion
	}
	switch {
	case !facts.Present:
		out.State = authMissing
	case facts.InstallationStatus == authReauthRequired:
		out.State = authReauthRequired
	case leaseActive:
		out.State = authRefreshing
	case facts.ExpiresAt == nil || !facts.ExpiresAt.After(now):
		out.State = authExpiredRefreshable
	default:
		out.State = authValid
	}
	return out
}

func installationOrigin(isFixture bool) string {
	if isFixture {
		return originFixture
	}
	return originReal
}

func pilotState(enabled *bool) string {
	if enabled == nil {
		return pilotNotConfigured
	}
	if *enabled {
		return pilotEnabled
	}
	return pilotDisabled
}
