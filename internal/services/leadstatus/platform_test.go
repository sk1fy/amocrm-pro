package leadstatus

import (
	"crypto/sha256"
	"fmt"

	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const (
	PingJobType          = widgetapi.PingJobType
	pingIdempotencyScope = "widget.ping:v1"
	widgetActorType      = "widget_user"
)

var (
	ErrInvalidIdempotencyKey = widgetapi.ErrInvalidIdempotencyKey
	ErrIdempotencyConflict   = widgetapi.ErrIdempotencyConflict
	ErrIdempotencyInProgress = widgetapi.ErrIdempotencyInProgress
	ErrInactiveTenant        = widgetapi.ErrInactiveTenant
)

func pingRequestHash(principal widgetauth.Principal) [sha256.Size]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", pingIdempotencyScope, principal.InstallationID, principal.AccountID, principal.UserID, principal.ClientUUID)))
}

type testWidgetHandler = widgetapi.Handler
type testHandler struct {
	*testWidgetHandler
	*Handler
}

func newTestHandler(jobStore *jobs.Store, actions *ActionStore) *testHandler {
	platform := widgetapi.NewHandler(jobStore, actions.ActionStore)
	RegisterResults(platform)
	return &testHandler{testWidgetHandler: platform, Handler: NewHandler(actions)}
}
