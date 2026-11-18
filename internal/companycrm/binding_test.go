package companycrm

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type fakeTokens struct {
	token amocrm.AccessToken
	calls int
}

func (f *fakeTokens) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	f.calls++
	return f.token, nil
}
func (f *fakeTokens) RefreshIfCurrent(context.Context, amocrm.AccessToken) (amocrm.AccessToken, error) {
	f.calls++
	return f.token, nil
}
func (f *fakeTokens) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	f.calls++
	return nil
}
func TestScopedCredentialRejectsWrongBindingBeforeTransportAndAfterRefresh(t *testing.T) {
	b := binding()
	f := &fakeTokens{token: amocrm.AccessToken{InstallationID: b.InstallationID, IntegrationID: b.IntegrationID, AccountID: b.AccountID}}
	scoped := ScopedTokens{Provider: f, Binding: b}
	access, err := scoped.Token(t.Context(), b.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	f.token.AccountID++
	if _, err := scoped.RefreshIfCurrent(t.Context(), access); err == nil {
		t.Fatal("wrong refresh account accepted")
	}
	if _, err := scoped.Token(t.Context(), uuid.New()); err == nil {
		t.Fatal("caller installation accepted")
	}
	if f.calls != 2 {
		t.Fatal(f.calls)
	}
}
func TestConfigDisabledAndPartialConfigurationRejected(t *testing.T) {
	for _, name := range []string{"COMPANY_CRM_HTTPS_ADDRESS", "COMPANY_CRM_IDENTITY_DIR", "COMPANY_CRM_ACCOUNT_ID", "COMPANY_CRM_INSTALLATION_ID", "COMPANY_CRM_INTEGRATION_ID", "COMPANY_CRM_BINDING_REVISION", "COMPANY_CRM_ACCOUNT_FIELD_ID"} {
		t.Setenv(name, "")
	}
	if c, err := LoadConfig(); err != nil || c.Address != "" {
		t.Fatal(c, err)
	}
	t.Setenv("COMPANY_CRM_ACCOUNT_ID", "1")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("partial config accepted")
	}
}
