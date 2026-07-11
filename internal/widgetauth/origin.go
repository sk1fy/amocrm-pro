package widgetauth

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// AudienceForRedirectURI returns the normalized base origin amoCRM puts in the
// aud claim for an integration redirect_uri.
func AudienceForRedirectURI(redirectURI string) (string, error) {
	if redirectURI == "" || redirectURI != strings.TrimSpace(redirectURI) {
		return "", fmt.Errorf("redirect URI is empty or has surrounding whitespace")
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("parse redirect URI: %w", err)
	}
	if parsed.Opaque != "" || !parsed.IsAbs() {
		return "", fmt.Errorf("redirect URI must be an absolute hierarchical URL")
	}
	return originFromURL(parsed, false)
}

// IssuerForAccountDomain normalizes an installation account_domain into the
// HTTPS origin amoCRM puts in the iss claim. accountDomain may be either a bare
// host or an HTTPS origin.
func IssuerForAccountDomain(accountDomain string) (string, error) {
	if accountDomain == "" || accountDomain != strings.TrimSpace(accountDomain) {
		return "", fmt.Errorf("account domain is empty or has surrounding whitespace")
	}
	if !strings.Contains(accountDomain, "://") {
		accountDomain = "https://" + accountDomain
	}
	return normalizeClaimOrigin(accountDomain, true)
}

// NormalizeHTTPSOrigin validates and canonicalizes a browser Origin value.
// Unlike IssuerForAccountDomain, it requires the caller to provide an
// absolute HTTPS origin rather than accepting a bare account domain.
func NormalizeHTTPSOrigin(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		return "", fmt.Errorf("origin must be an absolute URL")
	}
	return normalizeClaimOrigin(raw, true)
}

func normalizeClaimOrigin(raw string, httpsOnly bool) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", fmt.Errorf("origin is empty or has surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse origin: %w", err)
	}
	if parsed.Opaque != "" || !parsed.IsAbs() {
		return "", fmt.Errorf("origin must be an absolute hierarchical URL")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("origin must not contain a path")
	}
	if parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("origin must not contain an encoded path, query, or fragment")
	}
	origin, err := originFromURL(parsed, httpsOnly)
	if err != nil {
		return "", err
	}
	return origin, nil
}

func originFromURL(parsed *url.URL, httpsOnly bool) (string, error) {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("URL scheme must be http or https")
	}
	if httpsOnly && scheme != "https" {
		return "", fmt.Errorf("account issuer must use https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("URL must not contain user information")
	}

	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || strings.ContainsAny(hostname, " \t\r\n%") {
		return "", fmt.Errorf("URL host is invalid")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("URL port is empty")
	}

	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("URL port is invalid")
		}
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}

	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, nil
}
