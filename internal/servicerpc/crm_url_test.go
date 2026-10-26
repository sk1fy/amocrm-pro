package servicerpc

import (
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

func TestCRMBaseURLRoundTrip(t *testing.T) {
	for _, base := range []string{"https://alpha.amocrm.ru", "https://bravo.kommo.com", ""} {
		d := serviceapi.Directory{CRMBaseURL: base}
		if got := fromDirectory(toDirectory(d)); got.CRMBaseURL != base {
			t.Fatalf("directory domain lost: %+v", got)
		}
		p := serviceapi.ViewerPanel{CRMBaseURL: base}
		if got := fromViewerPanel(toViewerPanel(p)); got.CRMBaseURL != base {
			t.Fatalf("viewer domain lost: %+v", got)
		}
	}
}
