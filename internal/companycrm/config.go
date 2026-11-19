package companycrm

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Config struct {
	Address, IdentityDir string
	Binding              Binding
	DealsHost            string
}

func LoadConfig() (Config, error) {
	c := Config{Address: os.Getenv("COMPANY_CRM_HTTPS_ADDRESS"), IdentityDir: os.Getenv("COMPANY_CRM_IDENTITY_DIR")}
	names := []string{"COMPANY_CRM_ACCOUNT_ID", "COMPANY_CRM_INSTALLATION_ID", "COMPANY_CRM_INTEGRATION_ID", "COMPANY_CRM_BINDING_REVISION", "COMPANY_CRM_ACCOUNT_FIELD_ID"}
	host := os.Getenv("COMPANY_CRM_DEALS_HOST")
	configured := c.Address != "" || c.IdentityDir != "" || host != ""
	for _, n := range names {
		configured = configured || os.Getenv(n) != ""
	}
	if !configured {
		return c, nil
	}
	var err error
	if _, _, err = net.SplitHostPort(c.Address); err != nil || c.IdentityDir == "" {
		return Config{}, errors.New("company CRM requires HTTPS address and identity directory")
	}
	c.Binding.AccountID, err = strconv.ParseInt(os.Getenv(names[0]), 10, 64)
	if err != nil || c.Binding.AccountID <= 0 {
		return Config{}, errors.New("company CRM requires positive numeric account ID")
	}
	c.Binding.InstallationID, err = uuid.Parse(os.Getenv(names[1]))
	if err != nil || c.Binding.InstallationID == uuid.Nil {
		return Config{}, errors.New("company CRM requires installation UUID")
	}
	c.Binding.IntegrationID, err = uuid.Parse(os.Getenv(names[2]))
	if err != nil || c.Binding.IntegrationID == uuid.Nil {
		return Config{}, errors.New("company CRM requires integration UUID")
	}
	c.Binding.Revision = os.Getenv(names[3])
	if len(c.Binding.Revision) < 1 || len(c.Binding.Revision) > 128 || strings.ContainsAny(c.Binding.Revision, " \r\n\t") {
		return Config{}, errors.New("company CRM requires explicit binding revision")
	}
	if value := os.Getenv(names[4]); value != "" {
		c.Binding.AccountFieldID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.Binding.AccountFieldID <= 0 {
			return Config{}, errors.New("company CRM account field ID must be positive")
		}
	}
	if host != "" {
		base, err := amocrm.AccountBaseURL(host)
		if err != nil || base.Hostname() != host {
			return Config{}, errors.New("company CRM deals require an exact amoCRM hostname")
		}
		c.DealsHost = host
	}
	return c, nil
}
