package serviceapi

import "encoding/json"

// MaxResponseBytes leaves room below the transport's 4 MiB ceiling for protobuf
// framing and the panel envelope. Domain owners enforce this in both modes.
const MaxResponseBytes = 3 << 20

// ValidateResponseSize applies the same finite response budget before an owner
// returns its result to either a local caller or a transport adapter.
func ValidateResponseSize(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return Fail(Internal, "invalid service response")
	}
	if len(encoded) > MaxResponseBytes {
		return Fail(ResourceExhausted, "response exceeds size limit; choose a smaller page or employee batch")
	}
	return nil
}
