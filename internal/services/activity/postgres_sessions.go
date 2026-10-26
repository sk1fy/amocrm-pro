package activity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"slices"
	"strconv"
	"time"
)

const sessionSelect = `SELECT id::text,employee_id,kind,extract(epoch from started_at)::bigint,coalesce(extract(epoch from stopped_at)::bigint,0),context FROM work_sessions`

func scanSession(row panelRow, now int64) (serviceapi.WorkSession, error) {
	var s serviceapi.WorkSession
	err := row.Scan(&s.ID, &s.EmployeeID, &s.Kind, &s.StartedAt, &s.StoppedAt, &s.Context)
	end := s.StoppedAt
	if end == 0 {
		end = now
	}
	if s.Kind != "pause" {
		s.WorkTimeSeconds = max(0, end-s.StartedAt)
	}
	return s, err
}
func (s *Postgres) TaskPanel(ctx context.Context, scope serviceapi.Scope) (serviceapi.TaskPanelSettings, error) {
	result := serviceapi.TaskPanelSettings{IncludeRelatedDeals: true, EmployeeIDs: []int64{}, PriorityKeys: []string{"urgent", "overdue", "overdue_no_time", "today_no_time", "today_timed", "future"}}
	var raw []byte
	var revision int64
	err := s.pool.QueryRow(ctx, `SELECT settings,revision FROM task_panel_settings WHERE installation_id=$1 AND integration_id=$2`, scope.InstallationID, scope.IntegrationID).Scan(&raw, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	result.Revision = revision
	return result, nil
}
func (s *Postgres) SaveTaskPanel(ctx context.Context, scope serviceapi.Scope, settings serviceapi.TaskPanelSettings) (serviceapi.TaskPanelSettings, error) {
	expected := settings.Revision
	settings.Revision++
	raw, err := json.Marshal(settings)
	if err != nil {
		return serviceapi.TaskPanelSettings{}, err
	}
	var tagRows int64
	if expected == 0 {
		tag, err := s.pool.Exec(ctx, `INSERT INTO task_panel_settings(installation_id,integration_id,settings,revision) VALUES($1,$2,$3,1) ON CONFLICT DO NOTHING`, scope.InstallationID, scope.IntegrationID, raw)
		if err != nil {
			return serviceapi.TaskPanelSettings{}, err
		}
		tagRows = tag.RowsAffected()
	} else {
		tag, err := s.pool.Exec(ctx, `UPDATE task_panel_settings SET settings=$1,revision=revision+1 WHERE installation_id=$2 AND integration_id=$3 AND revision=$4`, raw, scope.InstallationID, scope.IntegrationID, expected)
		if err != nil {
			return serviceapi.TaskPanelSettings{}, err
		}
		tagRows = tag.RowsAffected()
	}
	if tagRows != 1 {
		return serviceapi.TaskPanelSettings{}, serviceapi.Fail(serviceapi.Conflict, "task panel revision mismatch")
	}
	return settings, nil
}
func (s *Postgres) ApplySession(ctx context.Context, p serviceapi.Principal, c serviceapi.SessionCommand) (serviceapi.WorkSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.WorkSession{}, err
	}
	defer rollback(tx)
	lock := sha256.Sum256([]byte("session:" + p.InstallationID.String() + ":" + p.IntegrationID.String() + ":" + strconv.FormatInt(p.ActorID, 10)))
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(lock[:8]))); err != nil {
		return serviceapi.WorkSession{}, err
	}
	canonical, _ := json.Marshal(struct {
		Action, SessionID, Expected string
		HasExpected                 bool
		Context                     serviceapi.SessionContext
	}{c.Action, c.SessionID, c.ExpectedSessionID, c.HasExpectedSession, c.Context})
	hash := sha256.Sum256(canonical)
	var oldHash, result []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,result FROM session_commands WHERE installation_id=$1 AND integration_id=$2 AND employee_id=$3 AND command_id=$4`, p.InstallationID, p.IntegrationID, p.ActorID, c.CommandID).Scan(&oldHash, &result)
	if err == nil {
		if !bytes.Equal(hash[:], oldHash) {
			return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
		var replay serviceapi.WorkSession
		if err = json.Unmarshal(result, &replay); err != nil {
			return replay, err
		}
		return replay, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.WorkSession{}, err
	}
	var settingsRaw []byte
	err = tx.QueryRow(ctx, `SELECT settings FROM task_panel_settings WHERE installation_id=$1 AND integration_id=$2 FOR SHARE`, p.InstallationID, p.IntegrationID).Scan(&settingsRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.WorkSession{}, err
	}
	if c.Action != "stop" {
		if errors.Is(err, pgx.ErrNoRows) {
			return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.PermissionDenied, "task panel is disabled")
		}
		if err != nil {
			return serviceapi.WorkSession{}, err
		}
		var settings serviceapi.TaskPanelSettings
		if err = json.Unmarshal(settingsRaw, &settings); err != nil {
			return serviceapi.WorkSession{}, err
		}
		if !settings.Enabled || !slices.Contains(settings.EmployeeIDs, p.ActorID) {
			return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.PermissionDenied, "task panel is disabled for employee")
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	var currentID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM work_sessions WHERE installation_id=$1 AND integration_id=$2 AND employee_id=$3 AND stopped_at IS NULL FOR UPDATE`, p.InstallationID, p.IntegrationID, p.ActorID).Scan(&currentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.WorkSession{}, err
	}
	if c.Action != "stop" && c.HasExpectedSession && c.ExpectedSessionID != currentID {
		return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.Conflict, "active session changed in another tab")
	}
	if c.Action == "stop" && currentID != c.SessionID {
		return serviceapi.WorkSession{}, serviceapi.Fail(serviceapi.Conflict, "session is no longer active")
	}
	if currentID != "" {
		if _, err = tx.Exec(ctx, `UPDATE work_sessions SET stopped_at=greatest(started_at,$1),context=CASE WHEN $3::text='' THEN context ELSE jsonb_set(context,'{task_result}',to_jsonb($3::text)) END WHERE id=$2`, now, currentID, c.Context.TaskResult); err != nil {
			return serviceapi.WorkSession{}, err
		}
	}
	id := currentID
	if c.Action != "stop" {
		id = uuid.NewString()
		if _, err = tx.Exec(ctx, `INSERT INTO work_sessions(id,installation_id,integration_id,employee_id,kind,started_at,context) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, p.InstallationID, p.IntegrationID, p.ActorID, c.Action, now, c.Context); err != nil {
			return serviceapi.WorkSession{}, err
		}
	}
	session, err := scanSession(tx.QueryRow(ctx, sessionSelect+` WHERE id=$1`, id), now.Unix())
	if err != nil {
		return serviceapi.WorkSession{}, err
	}
	result, err = json.Marshal(session)
	if err != nil {
		return serviceapi.WorkSession{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO session_commands(installation_id,integration_id,employee_id,command_id,request_hash,result) VALUES($1,$2,$3,$4,$5,$6)`, p.InstallationID, p.IntegrationID, p.ActorID, c.CommandID, hash[:], result); err != nil {
		return serviceapi.WorkSession{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return serviceapi.WorkSession{}, err
	}
	return session, nil
}
func (s *Postgres) ReadSessions(ctx context.Context, scope serviceapi.Scope, employee, from, to int64, cursor string) (serviceapi.SessionPage, error) {
	after, afterID := time.Unix(0, 0).UTC(), uuid.Nil
	if cursor != "" {
		var c sessionCursor
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(raw) > 512 || json.Unmarshal(raw, &c) != nil || c.Scope != scope || c.Employee != employee || c.From != from || c.To != to || c.AfterID == uuid.Nil {
			return serviceapi.SessionPage{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid session cursor")
		}
		after, afterID = c.After, c.AfterID
	}
	now := time.Now().Unix()
	out := serviceapi.SessionPage{SnapshotAt: now, Items: []serviceapi.WorkSession{}}
	rows, err := s.pool.Query(ctx, sessionSelect+` WHERE installation_id=$1 AND integration_id=$2 AND employee_id=$3 AND started_at<=to_timestamp($5) AND coalesce(stopped_at,now())>=to_timestamp($4) AND (started_at,id)>($6::timestamptz,$7) ORDER BY started_at,id LIMIT 201`, scope.InstallationID, scope.IntegrationID, employee, from, to, after, afterID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanSession(rows, now)
		if err != nil {
			return serviceapi.SessionPage{}, err
		}
		out.Items = append(out.Items, v)
	}
	if err = rows.Err(); err != nil {
		return serviceapi.SessionPage{}, err
	}
	if len(out.Items) > 200 {
		out.Items = out.Items[:200]
		last := out.Items[len(out.Items)-1]
		var exactStart time.Time
		if err = s.pool.QueryRow(ctx, `SELECT started_at FROM work_sessions WHERE id=$1 AND installation_id=$2 AND integration_id=$3`, last.ID, scope.InstallationID, scope.IntegrationID).Scan(&exactStart); err != nil {
			return serviceapi.SessionPage{}, err
		}
		raw, _ := json.Marshal(sessionCursor{Scope: scope, Employee: employee, From: from, To: to, After: exactStart, AfterID: uuid.MustParse(last.ID)})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

type sessionCursor struct {
	Scope              serviceapi.Scope
	Employee, From, To int64
	After              time.Time
	AfterID            uuid.UUID
}

func (s *Postgres) CurrentSession(ctx context.Context, scope serviceapi.Scope, employee int64) (serviceapi.WorkSession, error) {
	// Match legacy watchdog: stale work tasks close at observation time after8h;
	// activity/Imbox intervals are not silently capped.
	if _, err := s.pool.Exec(ctx, `UPDATE work_sessions SET stopped_at=greatest(started_at,now()) WHERE installation_id=$1 AND integration_id=$2 AND employee_id=$3 AND kind='work' AND stopped_at IS NULL AND started_at<now()-interval '8 hours'`, scope.InstallationID, scope.IntegrationID, employee); err != nil {
		return serviceapi.WorkSession{}, err
	}
	current, err := scanSession(s.pool.QueryRow(ctx, sessionSelect+` WHERE installation_id=$1 AND integration_id=$2 AND employee_id=$3 AND stopped_at IS NULL`, scope.InstallationID, scope.IntegrationID, employee), time.Now().Unix())
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.WorkSession{}, nil
	}
	return current, err
}
