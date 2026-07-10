package amocrm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var accountLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

var supportedAccountSuffixes = []string{"amocrm.ru", "amocrm.com", "kommo.com"}

func AccountBaseURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, errors.New("account domain is required")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse account domain: %w", err)
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return nil, errors.New("account URL must use HTTPS without user info or a custom port")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("account URL must not contain a path, query, or fragment")
	}

	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "localhost" || net.ParseIP(host) != nil {
		return nil, errors.New("account host cannot be localhost or an IP address")
	}

	validSuffix := false
	for _, suffix := range supportedAccountSuffixes {
		if strings.HasSuffix(host, "."+suffix) {
			validSuffix = true
			break
		}
	}
	if !validSuffix {
		return nil, errors.New("unsupported amoCRM account domain")
	}
	for _, label := range strings.Split(host, ".") {
		if !accountLabel.MatchString(label) {
			return nil, errors.New("account domain contains an invalid label")
		}
	}

	return &url.URL{Scheme: "https", Host: host}, nil
}
