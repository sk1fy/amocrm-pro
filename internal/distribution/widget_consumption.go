package distribution

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// widgetRequestConsumesToken reports whether the verified disposable token must
// be spent (single-use jti) for this widget request.
//
// The amoCRM Web SDK caches one disposable token per widget session and reuses
// it for the bootstrap/runtime reads. Spending the jti on reads makes every
// request after the first fail with a replay 401, although every read is
// otherwise fully verified. Therefore only mutating calls (runtime with
// write=true) consume the one-time jti; read-only calls are fully verified
// without spending it.
//
// Fail-closed: a body that cannot be buffered or parsed is treated as a
// mutation so replay protection is never silently skipped.
func widgetRequestConsumesToken(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/widget/distribution/runtime" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if r.Body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if err != nil || len(body) > MaxBody {
		return true
	}
	var probe struct {
		Write bool `json:"write"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return true
	}
	return probe.Write
}
