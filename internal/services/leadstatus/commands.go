package leadstatus

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const (
	LeadSetStatusJobType           = "workflow.lead.set_status"
	LeadStatusRuleConfigureJobType = "workflow.rule.lead_status.configure"
	leadStatusScope                = "widget.lead.set_status:v1"
	leadStatusRuleScope            = "widget.workflow_rule.lead_status.configure:v1"
	leadResourceType               = "lead"
	leadStatusRuleResourceType     = "lead_status_workflow_rule"
)

var (
	ErrInvalidLeadStatus     = errors.New("invalid lead status command")
	ErrInvalidLeadStatusRule = errors.New("invalid lead status rule command")
)

type ActionResult = widgetapi.ActionResult
type ActionStore struct{ *widgetapi.ActionStore }

func NewActionStore(pool *pgxpool.Pool, jobStore *jobs.Store) *ActionStore {
	return &ActionStore{widgetapi.NewActionStore(pool, jobStore)}
}

type LeadStatusCommand struct {
	LeadID     int64 `json:"lead_id"`
	PipelineID int64 `json:"pipeline_id"`
	StatusID   int64 `json:"status_id"`
}

// EnqueueLeadSetStatus records declarative desired state so worker retries
// compare the current lead before writing again.
func (s *ActionStore) EnqueueLeadSetStatus(
	ctx context.Context,
	principal widgetauth.Principal,
	idempotencyKey string,
	command LeadStatusCommand,
) (ActionResult, error) {
	if command.LeadID <= 0 || command.PipelineID <= 0 || command.StatusID <= 0 {
		return ActionResult{}, ErrInvalidLeadStatus
	}
	return s.Enqueue(ctx, widgetapi.ActionAdmission{
		Principal: principal, IdempotencyKey: idempotencyKey,
		Scope: leadStatusScope, RequestHash: leadStatusRequestHash(principal, command),
		JobType: LeadSetStatusJobType, ResourceType: leadResourceType,
		ResourceID: strconv.FormatInt(command.LeadID, 10), Payload: command,
		Priority: 40, MaxAttempts: 5,
	})
}

func leadStatusRequestHash(principal widgetauth.Principal, command LeadStatusCommand) [sha256.Size]byte {
	canonical := fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%s\x00%d\x00%d\x00%d",
		leadStatusScope, principal.InstallationID, principal.AccountID,
		principal.UserID, principal.ClientUUID, command.LeadID,
		command.PipelineID, command.StatusID,
	)
	return sha256.Sum256([]byte(canonical))
}
