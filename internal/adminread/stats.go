package adminread

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (h *handler) stats(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	from, to, period, err := parseStatsPeriod(r.URL.Query().Get("period"), observed)
	if err != nil {
		writeError(w, r, err)
		return
	}
	summary, err := h.store.statsSummary(r.Context(), from, to)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	summary.Source = sourceCore
	summary.ObservedAt = observed
	summary.Period = period
	summary.From = from
	summary.To = to
	summary.PeriodStart = from
	summary.PeriodEnd = to
	writeJSON(w, http.StatusOK, summary)
}

func (h *handler) statsAccounts(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	from, to, _, err := parseStatsPeriod(r.URL.Query().Get("period"), observed)
	if err != nil {
		writeError(w, r, err)
		return
	}
	metric := r.URL.Query().Get("metric")
	switch metric {
	case "auth_problems", "sync_problems", "connected", "disconnected", "active":
	default:
		writeError(w, r, errInvalid("metric must be auth_problems, sync_problems, connected, disconnected or active"))
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursorAccount, cursorID, err := decodeStatsCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, next, total, err := h.store.statsAccounts(r.Context(), metric, from, to, limit, cursorAccount, cursorID)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next, Total: total,
	})
}

func (s *store) installationScope(ctx context.Context, id uuid.UUID) (serviceapi.Scope, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var scope serviceapi.Scope
	err := s.pool.QueryRow(ctx, `SELECT i.id, i.integration_id FROM installations i JOIN integrations n ON n.id=i.integration_id WHERE i.id=$1`, id).Scan(&scope.InstallationID, &scope.IntegrationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return serviceapi.Scope{}, errNotFound("installation not found")
		}
		return serviceapi.Scope{}, err
	}
	return scope, nil
}

func (s *store) listLeadStatusRules(ctx context.Context, installationID uuid.UUID) ([]LeadStatusRule, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT id, source_pipeline_id, source_status_id, target_pipeline_id, target_status_id, enabled, revision, created_at, updated_at
		FROM lead_status_workflow_rules WHERE installation_id=$1 ORDER BY source_pipeline_id, source_status_id`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]LeadStatusRule, 0)
	for rows.Next() {
		var item LeadStatusRule
		var id uuid.UUID
		if err := rows.Scan(&id, &item.SourcePipelineID, &item.SourceStatusID, &item.TargetPipelineID, &item.TargetStatusID, &item.Enabled, &item.Revision, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.ID = id.String()
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *store) listLeadStatusRuns(ctx context.Context, installationID uuid.UUID, limit int, cursorTime time.Time, cursorID string) ([]LeadStatusRun, *string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var cursorAt any
	var cursorUUID any
	if cursorID != "" {
		id, err := uuid.Parse(cursorID)
		if err != nil {
			return nil, nil, errInvalid("invalid cursor")
		}
		cursorAt = cursorTime
		cursorUUID = id
	}
	rows, err := s.pool.Query(ctx, `
		SELECT run.id, run.workflow_type, run.status, run.rule_id, run.created_at, run.finished_at,
			effect.state, effect.last_error, effect.effect_type
		FROM workflow_runs run
		LEFT JOIN LATERAL (
			SELECT state, last_error, effect_type
			FROM outbound_effects
			WHERE installation_id=run.installation_id AND workflow_run_id=run.id
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		) effect ON true
		WHERE run.installation_id=$1
		  AND ($2::timestamptz IS NULL OR (run.created_at, run.id) < ($2::timestamptz, $3::uuid))
		ORDER BY run.created_at DESC, run.id DESC
		LIMIT $4`, installationID, cursorAt, cursorUUID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]LeadStatusRun, 0)
	for rows.Next() {
		var item LeadStatusRun
		var id uuid.UUID
		var ruleID *uuid.UUID
		if err := rows.Scan(&id, &item.WorkflowType, &item.Status, &ruleID, &item.CreatedAt, &item.FinishedAt, &item.EffectState, &item.EffectError, &item.EffectType); err != nil {
			return nil, nil, err
		}
		item.ID = id.String()
		if ruleID != nil {
			value := ruleID.String()
			item.RuleID = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		encoded := encodeCursor(last.CreatedAt, last.ID)
		next = &encoded
	}
	return items, next, nil
}

func (s *store) statsSummary(ctx context.Context, from, to time.Time) (StatsResponse, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	out := StatsResponse{
		Connections: []StatsConnection{},
		Queues:      []StatsQueue{},
		PeriodEvents: StatsPeriodEvents{
			Connected:    int64Ptr(0),
			Disconnected: int64Ptr(0),
		},
		ActiveAccounts:    int64Ptr(0),
		JobErrors:         int64Ptr(0),
		AuthProblemsCount: int64Ptr(0),
		SyncProblemsCount: int64Ptr(0),
	}
	rows, err := s.pool.Query(ctx, `
		SELECT n.code, i.status, count(*)
		FROM installations i
		JOIN integrations n ON n.id=i.integration_id
		GROUP BY n.code, i.status
		ORDER BY n.code, i.status`)
	if err != nil {
		return StatsResponse{}, err
	}
	for rows.Next() {
		var item StatsConnection
		if err := rows.Scan(&item.IntegrationCode, &item.Status, &item.Count); err != nil {
			rows.Close()
			return StatsResponse{}, err
		}
		item.Product = item.IntegrationCode
		out.Connections = append(out.Connections, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StatsResponse{}, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT installation_id) FILTER (WHERE action='installation.authorized'),
			count(DISTINCT installation_id) FILTER (WHERE action IN ('installation.disable','installation.uninstall','installation.revoke','disable-installation','uninstall','revoke'))
		FROM audit_log
		WHERE created_at >= $1 AND created_at < $2`, from, to).Scan(out.PeriodEvents.Connected, out.PeriodEvents.Disconnected); err != nil {
		return StatsResponse{}, err
	}
	out.Connected = out.PeriodEvents.Connected
	out.Disconnected = out.PeriodEvents.Disconnected
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT i.account_id)
		FROM installations i
		WHERE i.updated_at >= $1 AND i.updated_at < $2
		   OR EXISTS (
			SELECT 1 FROM jobs j WHERE j.installation_id=i.id AND j.updated_at >= $1 AND j.updated_at < $2
		   )`, from, to).Scan(out.ActiveAccounts); err != nil {
		return StatsResponse{}, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE status IN ('failed','dead') AND updated_at >= $1 AND updated_at < $2`, from, to).Scan(out.JobErrors); err != nil {
		return StatsResponse{}, err
	}
	var avg, p50 *float64
	if err := s.pool.QueryRow(ctx, `
		SELECT avg(duration_ms)::float8, percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)
		FROM job_attempts
		WHERE finished_at >= $1 AND finished_at < $2 AND duration_ms IS NOT NULL`, from, to).Scan(&avg, &p50); err != nil {
		return StatsResponse{}, err
	}
	out.Latency = StatsLatency{AvgMS: avg, P50MS: p50}
	queueRows, err := s.pool.Query(ctx, `
		SELECT type, status, count(*)
		FROM jobs
		WHERE updated_at > now() - interval '7 days'
		GROUP BY type, status
		ORDER BY type, status`)
	if err != nil {
		return StatsResponse{}, err
	}
	for queueRows.Next() {
		var item StatsQueue
		if err := queueRows.Scan(&item.Type, &item.Status, &item.Count); err != nil {
			queueRows.Close()
			return StatsResponse{}, err
		}
		out.Queues = append(out.Queues, item)
	}
	queueRows.Close()
	if err := queueRows.Err(); err != nil {
		return StatsResponse{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM installations WHERE status='reauth_required'`).Scan(out.AuthProblemsCount); err != nil {
		return StatsResponse{}, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT receipt.installation_id)
		FROM activity_command_outbox outbox
		JOIN activity_command_receipts receipt USING(command_id)
		WHERE outbox.status='failed' AND receipt.created_at > now() - interval '7 days'`).Scan(out.SyncProblemsCount); err != nil {
		return StatsResponse{}, err
	}
	out.AuthProblems = out.AuthProblemsCount
	out.SyncProblems = out.SyncProblemsCount
	if p50 != nil {
		value := int64(*p50)
		out.LatencyP50Ms = &value
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT GREATEST(
			(SELECT max(updated_at) FROM installations WHERE updated_at >= $1 AND updated_at < $2),
			(SELECT max(updated_at) FROM jobs WHERE updated_at >= $1 AND updated_at < $2)
		)`, from, to).Scan(&out.LastUseAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StatsResponse{}, err
	}
	return out, nil
}

func (s *store) statsAccounts(ctx context.Context, metric string, from, to time.Time, limit int, cursorAccount int64, cursorID uuid.UUID) ([]StatsAccount, *string, *int64, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	query, args := statsAccountsQuery(metric, from, to, false, 0, uuid.Nil, 0)
	var total int64
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&total); err != nil {
		return nil, nil, nil, err
	}
	listQuery, listArgs := statsAccountsQuery(metric, from, to, true, cursorAccount, cursorID, limit+1)
	rows, err := s.pool.Query(ctx, listQuery, listArgs...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	items := make([]StatsAccount, 0)
	for rows.Next() {
		var item StatsAccount
		var installation uuid.UUID
		if err := rows.Scan(&item.AccountID, &item.Domain, &installation, &item.IntegrationCode, &item.Reason); err != nil {
			return nil, nil, nil, err
		}
		item.InstallationID = installation.String()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		id := uuid.MustParse(last.InstallationID)
		encoded := encodeStatsCursor(last.AccountID, id)
		next = &encoded
	}
	return items, next, &total, nil
}

func statsAccountsQuery(metric string, from, to time.Time, list bool, cursorAccount int64, cursorID uuid.UUID, limit int) (string, []any) {
	var where string
	args := []any{}
	switch metric {
	case "auth_problems":
		where = `i.status='reauth_required'`
	case "sync_problems":
		where = `EXISTS (
			SELECT 1 FROM activity_command_outbox outbox
			JOIN activity_command_receipts receipt USING(command_id)
			WHERE receipt.installation_id=i.id AND outbox.status='failed'
			  AND receipt.created_at > now() - interval '7 days')`
	case "connected":
		where = `EXISTS (
			SELECT 1 FROM audit_log a
			WHERE a.installation_id=i.id AND a.action='installation.authorized'
			  AND a.created_at >= $1 AND a.created_at < $2)`
		args = append(args, from, to)
	case "disconnected":
		where = `EXISTS (
			SELECT 1 FROM audit_log a
			WHERE a.installation_id=i.id
			  AND a.action IN ('installation.disable','installation.uninstall','installation.revoke','disable-installation','uninstall','revoke')
			  AND a.created_at >= $1 AND a.created_at < $2)`
		args = append(args, from, to)
	case "active":
		where = `(i.updated_at >= $1 AND i.updated_at < $2) OR EXISTS (
			SELECT 1 FROM jobs j WHERE j.installation_id=i.id AND j.updated_at >= $1 AND j.updated_at < $2)`
		args = append(args, from, to)
	}
	reason := statsReasonExpr(metric)
	if !list {
		return `SELECT count(*) FROM installations i JOIN integrations n ON n.id=i.integration_id WHERE ` + where, args
	}
	cursorSQL := ""
	if cursorID != uuid.Nil {
		args = append(args, cursorAccount, cursorID)
		n := len(args)
		cursorSQL = ` AND (i.account_id, i.id) > ($` + strconv.Itoa(n-1) + `::bigint, $` + strconv.Itoa(n) + `::uuid)`
	}
	args = append(args, limit)
	limitPlaceholder := "$" + strconv.Itoa(len(args))
	return `SELECT i.account_id, i.account_domain, i.id, n.code, ` + reason + `
		FROM installations i JOIN integrations n ON n.id=i.integration_id
		WHERE ` + where + cursorSQL + `
		ORDER BY i.account_id, i.id LIMIT ` + limitPlaceholder, args
}

func statsReasonExpr(metric string) string {
	switch metric {
	case "auth_problems":
		return `'reauth_required'`
	case "sync_problems":
		return `COALESCE((
			SELECT outbox.error_code FROM activity_command_outbox outbox
			JOIN activity_command_receipts receipt USING(command_id)
			WHERE receipt.installation_id=i.id AND outbox.status='failed'
			  AND receipt.created_at > now() - interval '7 days'
			ORDER BY receipt.created_at DESC, receipt.command_id DESC LIMIT 1
		),'sync_failed')`
	case "connected":
		return `'installation.authorized'`
	case "disconnected":
		return `COALESCE((
			SELECT a.action FROM audit_log a
			WHERE a.installation_id=i.id
			  AND a.action IN ('installation.disable','installation.uninstall','installation.revoke','disable-installation','uninstall','revoke')
			ORDER BY a.created_at DESC, a.id DESC LIMIT 1
		),'disconnected')`
	default:
		return `'used'`
	}
}

func int64Ptr(v int64) *int64 { return &v }
