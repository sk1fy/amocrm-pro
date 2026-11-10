package companyentry

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct{ Address, TeamOSURL, TeamOSToken, FrontendOrigin, SubjectCheckKey string }

func httpsOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && u.RawPath == "" && !strings.HasSuffix(value, "/")
}
func LoadConfig() (Config, error) {
	c := Config{Address: os.Getenv("COMPANY_ENTRY_HTTP_ADDRESS"), TeamOSURL: os.Getenv("COMPANY_ENTRY_TEAM_OS_URL"), TeamOSToken: os.Getenv("COMPANY_ENTRY_TEAM_OS_TOKEN"), FrontendOrigin: os.Getenv("COMPANY_ENTRY_FRONTEND_ORIGIN"), SubjectCheckKey: os.Getenv("COMPANY_ENTRY_SUBJECT_CHECK_KEY")}
	if c.Address == "" && c.TeamOSURL == "" && c.TeamOSToken == "" && c.FrontendOrigin == "" && c.SubjectCheckKey == "" {
		return c, nil
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, errors.New("invalid COMPANY_ENTRY_HTTP_ADDRESS")
	}
	if os.Getenv("COMPANY_ENTRY_TLS_PROXY") != "true" {
		return c, errors.New("company entry requires trusted TLS proxy")
	}
	if !httpsOrigin(c.TeamOSURL) || !httpsOrigin(c.FrontendOrigin) {
		return c, errors.New("company entry requires fixed HTTPS origins")
	}
	if len(c.TeamOSToken) < 32 || len(c.SubjectCheckKey) < 32 || c.TeamOSToken == c.SubjectCheckKey || strings.ContainsAny(c.TeamOSToken+c.SubjectCheckKey, "\r\n ") {
		return c, errors.New("company entry requires distinct service credentials")
	}
	return c, nil
}
