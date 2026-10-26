package activitybridge

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"net/http"
	"time"
)

func (b *Bridge) SessionWriteHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var input struct {
		ExpectedSessionID *string                   `json:"expected_session_id"`
		Action            string                    `json:"action"`
		SessionID         string                    `json:"session_id"`
		Context           serviceapi.SessionContext `json:"context"`
	}
	if err := decode(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	commandID, err := uuidIdempotencyKey(r)
	if err != nil {
		writeError(w, err)
		return
	}
	command := serviceapi.SessionCommand{CommandID: commandID, Action: input.Action, SessionID: input.SessionID, Context: input.Context}
	if input.ExpectedSessionID != nil {
		command.ExpectedSessionID = *input.ExpectedSessionID
		command.HasExpectedSession = true
	}
	if err := serviceapi.ValidateSessionCommand(command); err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	auth, err := b.policy.Issue(ctx, serviceapi.IssueRequest{Scope: serviceapi.Scope{InstallationID: p.InstallationID, IntegrationID: p.IntegrationID}, ActorID: p.UserID, Kind: serviceapi.PrincipalKindEmployee, Consumer: serviceapi.ActivityService, RequestID: uuid.NewString(), Grants: []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}}})
	if err != nil {
		writeError(w, err)
		return
	}
	command.Auth = auth
	result, err := b.activity.ApplySession(ctx, command)
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) ViewSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathInt64(r, "employeeId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	q, err := decodeViewerQuery(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	q.Auth = auth
	q.UserIDs = []int64{id}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ViewSessions(ctx, q)
	respond(w, http.StatusOK, result, shareErr(err))
}
func (b *Bridge) TaskPanelHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueOperator(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.TaskPanel(ctx, auth)
	respond(w, http.StatusOK, result, shareErr(err))
}
func (b *Bridge) TaskPanelSaveHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueOperator(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	var settings serviceapi.TaskPanelSettings
	if err := decode(w, r, &settings); err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.SaveTaskPanel(ctx, serviceapi.TaskPanelCommand{Auth: auth, Settings: settings})
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) employeeAuth(ctx context.Context, p widgetauth.Principal) (serviceapi.Auth, error) {
	return b.policy.Issue(ctx, serviceapi.IssueRequest{Scope: serviceapi.Scope{InstallationID: p.InstallationID, IntegrationID: p.IntegrationID}, ActorID: p.UserID, Kind: serviceapi.PrincipalKindEmployee, Consumer: serviceapi.ActivityService, RequestID: uuid.NewString(), Grants: []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}}})
}
func (b *Bridge) OwnActivityStateHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	auth, err := b.employeeAuth(ctx, p)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := b.activity.OwnActivityState(ctx, auth)
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) OwnSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	q, err := decodeQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	q.Auth, err = b.employeeAuth(ctx, p)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := b.activity.OwnSessions(ctx, q)
	respond(w, http.StatusOK, result, err)
}
