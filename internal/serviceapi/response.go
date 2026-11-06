package serviceapi

import (
	"encoding/json"
	"errors"
)

// MaxResponseBytes leaves room below the transport's 4 MiB ceiling for protobuf
// framing and the panel envelope. Domain owners enforce this in both modes.
const MaxResponseBytes = 3 << 20

const responseSizeMessage = "response exceeds size limit; choose a smaller page or employee batch"

func IsResponseSizeError(err error) bool {
	var domain *Error
	return errors.As(err, &domain) && domain.Code == ResourceExhausted && domain.Message == responseSizeMessage
}

// ValidateResponseSize applies the same finite response budget before an owner
// returns its result to either a local caller or a transport adapter.
func ValidateResponseSize(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return Fail(Internal, "invalid service response")
	}
	if len(encoded) > MaxResponseBytes {
		return Fail(ResourceExhausted, responseSizeMessage)
	}
	return nil
}
