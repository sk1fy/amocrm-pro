package adminread

import (
	"encoding/json"
	"github.com/sk1fy/amocrm-pro/internal/connectioncheck"
	"time"
)

// The projection is valid only for the current installation and credential
// version. A new OAuth exchange invalidates it immediately, before any check.
const currentCheckSQL = `SELECT ck.classification,ck.observed_at,ck.retry_after
 FROM installation_checks ck JOIN admin_commands receipt ON receipt.id=ck.receipt_id
 WHERE ck.installation_id=i.id AND ck.installation_status=i.status
 AND ck.credential_version=COALESCE((SELECT token_version FROM oauth_credentials WHERE installation_id=i.id),0)
 AND receipt.state='succeeded' AND receipt.outcome=ck.classification`
const checkJSONSQL = `(SELECT row_to_json(verification) FROM (` + currentCheckSQL + `) verification)`

func decodeCheck(data []byte, now time.Time) connectioncheck.Snapshot {
	var raw struct {
		Classification string     `json:"classification"`
		ObservedAt     *time.Time `json:"observed_at"`
		RetryAfter     int64      `json:"retry_after"`
	}
	_ = json.Unmarshal(data, &raw)
	return connectioncheck.New(raw.Classification, raw.ObservedAt, raw.RetryAfter, now)
}
