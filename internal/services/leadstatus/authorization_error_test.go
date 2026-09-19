package leadstatus

import (
	"errors"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"testing"
)

func TestInvalidGrantRetainsReauthorizationRetrySemantics(t *testing.T) {
	for _, kind := range []amocrm.ErrorKind{amocrm.ErrorInvalidGrant, amocrm.ErrorUnauthorized} {
		var result *jobs.Error
		if !errors.As(classifyMutationError(&amocrm.APIError{Kind: kind}), &result) || !result.Retryable {
			t.Fatalf("reauthorization failure became permanent: %s", kind)
		}
	}
}
