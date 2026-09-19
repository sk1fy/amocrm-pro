package corepolicy

import (
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

func TestInvalidGrantIsReauthorizationAcrossServiceBoundary(t *testing.T) {
	for _, tc := range []struct {
		kind amocrm.ErrorKind
		want serviceapi.Code
	}{{amocrm.ErrorInvalidGrant, serviceapi.ReauthRequired}, {amocrm.ErrorUnauthorized, serviceapi.ReauthRequired}, {amocrm.ErrorValidation, serviceapi.InvalidArgument}, {amocrm.ErrorTemporary, serviceapi.Unavailable}} {
		if got := serviceapi.ErrorCode(MapUpstreamError(&amocrm.APIError{Kind: tc.kind})); got != tc.want {
			t.Fatalf("%s got %s want %s", tc.kind, got, tc.want)
		}
	}
}
