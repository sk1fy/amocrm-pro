package activity

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func sessionTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("requires _test")
	}
	ctx := t.Context()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	return NewPostgres(pool)
}

func TestSessionsLifecycleReplayConcurrencyAndIsolation(t *testing.T) {
	ctx := t.Context()
	store := sessionTestStore(t)
	pool := store.pool
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7, Kind: serviceapi.PrincipalKindEmployee}
	s := New(store, panelPolicy{principal: p}, nil, nil)
	auth := serviceapi.Auth{Token: "verified"}
	command := serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "work", Context: serviceapi.SessionContext{TaskID: 1, TaskName: "Synthetic task", EntityID: 100, EntityType: "leads", PipelineID: 10, StatusID: 11}}
	if _, err := s.ApplySession(ctx, command); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("disabled panel accepted %v", err)
	}
	settings, err := store.SaveTaskPanel(ctx, p.Scope, serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveTaskPanel(ctx, p.Scope, serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7}}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatal("initial task settings CAS not checked")
	}
	first, err := s.ApplySession(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ApplySession(ctx, command)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("start replay %+v %v", replay, err)
	}
	changed := command
	changed.Context.TaskName = "changed"
	if _, err = s.ApplySession(ctx, changed); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatal("mutated replay accepted")
	}
	// Concurrent starts serialize per employee; exactly one stays open.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.ApplySession(ctx, serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "activity", Context: serviceapi.SessionContext{ActivityName: "Synthetic dialog", ActivityType: "imbox", EntityID: 100, EntityType: "leads"}})
			if e != nil {
				t.Errorf("concurrent start %v", e)
			}
		}()
	}
	wg.Wait()
	page, err := store.ReadSessions(ctx, p.Scope, 7, time.Now().Add(-time.Hour).Unix(), time.Now().Unix(), "")
	if err != nil {
		t.Fatal(err)
	}
	var active serviceapi.WorkSession
	open := 0
	for _, v := range page.Items {
		if v.StoppedAt == 0 {
			open++
			active = v
		}
	}
	if open != 1 || len(page.Items) != 6 {
		t.Fatalf("overlap/duplication %+v", page)
	}
	if _, err = s.ApplySession(ctx, serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "stop", SessionID: first.ID}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatal("stale stop closed a newer session")
	}
	// Disabling collection must still permit closing an existing session.
	settings.Enabled = false
	if _, err = store.SaveTaskPanel(ctx, p.Scope, settings); err != nil {
		t.Fatal(err)
	}
	stop := serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "stop", SessionID: active.ID, Context: serviceapi.SessionContext{TaskResult: "Synthetic result"}}
	closed, err := s.ApplySession(ctx, stop)
	if err != nil || closed.StoppedAt == 0 || closed.Context.TaskResult != "Synthetic result" {
		t.Fatalf("stop %+v %v", closed, err)
	}
	if _, err = s.ApplySession(ctx, stop); err != nil {
		t.Fatalf("stop replay %v", err)
	}
	foreign := p.Scope
	foreign.InstallationID = uuid.New()
	other, err := store.ReadSessions(ctx, foreign, 7, 0, time.Now().Unix(), "")
	if err != nil || len(other.Items) > 0 {
		t.Fatal("cross-installation session leak")
	}
	other, err = store.ReadSessions(ctx, p.Scope, 9, 0, time.Now().Unix(), "")
	if err != nil || len(other.Items) > 0 {
		t.Fatal("cross-employee session leak")
	}
	// A large history is paginated rather than truncated. Cursor cannot cross scope or employee.
	if _, err = pool.Exec(ctx, `INSERT INTO work_sessions(id,installation_id,integration_id,employee_id,kind,started_at,stopped_at,context) SELECT gen_random_uuid(),$1,$2,7,'work',now()-interval '10 minutes',now()-interval '9 minutes','{}'::jsonb FROM generate_series(1,450)`, p.InstallationID, p.IntegrationID); err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Second).Unix()
	firstPage, err := store.ReadSessions(ctx, p.Scope, 7, from, to, "")
	if err != nil || len(firstPage.Items) != 200 || firstPage.NextCursor == "" {
		t.Fatalf("first page %+v %v", firstPage, err)
	}
	if _, err = store.ReadSessions(ctx, p.Scope, 9, from, to, firstPage.NextCursor); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatal("cross employee cursor accepted")
	}
	seen := map[string]bool{}
	cursor := ""
	count := 0
	for {
		page, err := store.ReadSessions(ctx, p.Scope, 7, from, to, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate session across pages")
			}
			seen[item.ID] = true
			count++
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if count != 456 {
		t.Fatalf("session pagination lost records: %d", count)
	}

	// Own state never exposes another employee's configuration or session.
	own, err := s.OwnActivityState(ctx, auth)
	if err != nil || own.Enabled || own.Current.ID != "" {
		t.Fatalf("own state %+v %v", own, err)
	}
	settings.Enabled = true
	settings.Revision++ // The disable update advanced the persisted revision.
	if _, err = store.SaveTaskPanel(ctx, p.Scope, settings); err != nil {
		t.Fatal(err)
	}
	guarded := serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "work", HasExpectedSession: true, Context: serviceapi.SessionContext{TaskID: 8}}
	fresh, err := s.ApplySession(ctx, guarded)
	if err != nil {
		t.Fatal(err)
	}
	guarded.CommandID = uuid.NewString()
	if _, err = s.ApplySession(ctx, guarded); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatal("stale tab replaced active session")
	}
	own, err = s.OwnActivityState(ctx, auth)
	if err != nil || own.Current.ID != fresh.ID || !own.Enabled {
		t.Fatalf("restore own state %+v %v", own, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE work_sessions SET started_at=now()-interval '9 hours' WHERE id=$1`, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.maintainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	own, err = s.OwnActivityState(ctx, auth)
	if err != nil || own.Current.ID != "" {
		t.Fatalf("watchdog failed %+v %v", own, err)
	}
	var duration float64
	if err = pool.QueryRow(ctx, `SELECT extract(epoch from stopped_at-started_at) FROM work_sessions WHERE id=$1`, fresh.ID).Scan(&duration); err != nil || duration < 9*3600 {
		t.Fatal("watchdog truncated duration", duration, err)
	}

}

type taskStartGateway struct {
	serviceapi.Gateway
	serviceapi.EmployeeTasks
	resolve func(context.Context, serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error)
}

func (g taskStartGateway) OwnTask(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	return g.resolve(ctx, r)
}

func taskStartFixture() (serviceapi.Principal, serviceapi.SessionCommand) {
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7, Kind: serviceapi.PrincipalKindEmployee}
	c := serviceapi.SessionCommand{
		Auth: serviceapi.Auth{Token: "verified"}, CommandID: uuid.NewString(), Action: serviceapi.SessionActionStartTask,
		HasExpectedSession: true, Context: serviceapi.SessionContext{TaskID: 9, ClientID: uuid.NewString()},
	}
	return p, c
}

func TestTaskStartUsesLiveContextAndReplaysReceipt(t *testing.T) {
	ctx := t.Context()
	store := sessionTestStore(t)
	p, c := taskStartFixture()
	task := serviceapi.TaskDetail{
		ID: 9, ResponsibleUserID: 7, Text: "a\x00" + strings.Repeat("я", 1001), TaskTypeID: 2, TaskTypeName: "Call", EntityType: "contacts", EntityID: 26,
		Entity: serviceapi.EntityContext{ID: 26, EntityType: "contacts", Name: "Resolved card"},
	}
	reads := 0
	s := New(store, panelPolicy{principal: p}, nil, taskStartGateway{resolve: func(_ context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
		reads++
		if !r.WithContext || r.Auth != c.Auth || r.TaskID != task.ID {
			t.Fatalf("wrong live lookup: %+v", r)
		}
		if task.ResponsibleUserID != p.ActorID {
			return serviceapi.TaskDetail{}, serviceapi.Fail(serviceapi.PermissionDenied, "task belongs to another employee")
		}
		return task, nil
	}})
	if _, err := s.ApplySession(ctx, c); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || reads != 0 {
		t.Fatalf("disabled panel reached CRM: %v, reads=%d", err, reads)
	}
	settings, err := store.SaveTaskPanel(ctx, p.Scope, serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{7, 8}})
	if err != nil {
		t.Fatal(err)
	}
	invalid := c
	invalid.Context.EntityID = 100
	if _, err := s.ApplySession(ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument || reads != 0 {
		t.Fatalf("caller entity data reached CRM: %v, reads=%d", err, reads)
	}
	task.IsCompleted = true
	if _, err := s.ApplySession(ctx, c); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("completed task accepted: %v", err)
	}
	task.IsCompleted, task.ResponsibleUserID = false, 8
	if _, err := s.ApplySession(ctx, c); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("foreign task accepted: %v", err)
	}
	if current, err := store.CurrentSession(ctx, p.Scope, p.ActorID); err != nil || current.ID != "" {
		t.Fatalf("rejected start wrote a session: %+v %v", current, err)
	}

	task.ResponsibleUserID = p.ActorID
	first, err := s.ApplySession(ctx, c)
	if err != nil || first.ID == "" || first.Kind != "work" || first.EmployeeID != p.ActorID || first.StoppedAt != 0 ||
		first.Context.ClientID != c.Context.ClientID || first.Context.TaskID != 9 || first.Context.TaskTypeName != "Call" ||
		first.Context.EntityID != 26 || first.Context.EntityType != "contacts" || first.Context.EntityName != "Resolved card" ||
		len(first.Context.TaskName) > serviceapi.SessionContextTextLimit || !utf8.ValidString(first.Context.TaskName) {
		t.Fatalf("wrong work receipt: %+v %v", first, err)
	}
	if reads != 3 {
		t.Fatalf("new commands read CRM %d times", reads)
	}
	task.ResponsibleUserID = 8
	settings.Enabled = false
	if _, err := store.SaveTaskPanel(ctx, p.Scope, settings); err != nil {
		t.Fatal(err)
	}
	restarted := New(store, panelPolicy{principal: p}, nil, nil)
	c.Auth.Token = "fresh-delegation"
	if replay, err := restarted.ApplySession(ctx, c); err != nil || replay != first {
		t.Fatalf("lost response did not replay original receipt: %+v %v", replay, err)
	}
	for _, change := range []func(*serviceapi.SessionCommand){
		func(c *serviceapi.SessionCommand) { c.Context.TaskID++ },
		func(c *serviceapi.SessionCommand) { c.Context.ClientID = uuid.NewString() },
		func(c *serviceapi.SessionCommand) { c.ExpectedSessionID = first.ID },
		func(c *serviceapi.SessionCommand) { c.Action = "work" },
	} {
		modified := c
		change(&modified)
		if receipt, err := restarted.ApplySession(ctx, modified); serviceapi.ErrorCode(err) != serviceapi.Conflict || receipt.ID != "" {
			t.Fatalf("changed intent replay accepted: %+v %v", receipt, err)
		}
	}
	foreign := p
	foreign.ActorID = 8
	if receipt, err := New(store, panelPolicy{principal: foreign}, nil, nil).ApplySession(ctx, c); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || receipt.ID != "" {
		t.Fatalf("receipt was exposed to another employee: %+v %v", receipt, err)
	}
	if receipt, err := restarted.ApplySession(ctx, serviceapi.SessionCommand{CommandID: c.CommandID, Action: c.Action, HasExpectedSession: true, Context: c.Context}); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated || receipt.ID != "" {
		t.Fatalf("unauthenticated receipt was exposed: %+v %v", receipt, err)
	}
	page, err := store.ReadSessions(ctx, p.Scope, p.ActorID, 0, time.Now().Add(time.Second).Unix(), "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID || page.Items[0].StoppedAt != 0 {
		t.Fatalf("retry changed session history: %+v %v", page, err)
	}

	settings.Enabled = true
	settings.Revision++
	settings, err = store.SaveTaskPanel(ctx, p.Scope, settings)
	if err != nil {
		t.Fatal(err)
	}
	switchTask := c
	switchTask.CommandID = uuid.NewString()
	switchTask.ExpectedSessionID = first.ID
	switchTask.Context.TaskID = 10
	revokeDuringRead := New(store, panelPolicy{principal: p}, nil, taskStartGateway{resolve: func(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
		settings.Enabled = false
		var err error
		settings, err = store.SaveTaskPanel(ctx, p.Scope, settings)
		return serviceapi.TaskDetail{ID: r.TaskID}, err
	}})
	if _, err := revokeDuringRead.ApplySession(ctx, switchTask); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("start bypassed task panel revocation: %v", err)
	}
	if current, err := store.CurrentSession(ctx, p.Scope, p.ActorID); err != nil || current.ID != first.ID {
		t.Fatalf("denied start closed the existing interval: %+v %v", current, err)
	}
	settings.Enabled = true
	if _, err := store.SaveTaskPanel(ctx, p.Scope, settings); err != nil {
		t.Fatal(err)
	}
	partial := New(store, panelPolicy{principal: p}, nil, taskStartGateway{resolve: func(_ context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
		return serviceapi.TaskDetail{ID: r.TaskID, EntityType: "leads", EntityID: 42}, nil
	}})
	next, err := partial.ApplySession(ctx, switchTask)
	if err != nil || next.ID == "" || next.ID == first.ID || next.Context.EntityType != "leads" || next.Context.EntityID != 42 {
		t.Fatalf("guarded switch lost authoritative card identity: %+v %v", next, err)
	}
	var stopped bool
	if err := store.pool.QueryRow(ctx, `SELECT stopped_at IS NOT NULL FROM work_sessions WHERE id=$1`, first.ID).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("guarded switch did not close previous interval: %v", err)
	}
}

func TestConcurrentTaskStartsKeepOneInterval(t *testing.T) {
	store := sessionTestStore(t)
	for _, tc := range []struct {
		name         string
		duplicate    bool
		preflightErr error
	}{
		{name: "different commands"},
		{name: "same command", duplicate: true},
		{name: "same command after preflight denial", duplicate: true, preflightErr: serviceapi.Fail(serviceapi.PermissionDenied, "task was reassigned")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			p, first := taskStartFixture()
			if _, err := store.SaveTaskPanel(ctx, p.Scope, serviceapi.TaskPanelSettings{Enabled: true, EmployeeIDs: []int64{p.ActorID}}); err != nil {
				t.Fatal(err)
			}
			second := first
			if !tc.duplicate {
				second.CommandID = uuid.NewString()
			}
			firstReading, release := make(chan struct{}), make(chan struct{})
			firstRead := taskStartGateway{resolve: func(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
				close(firstReading)
				select {
				case <-release:
					return serviceapi.TaskDetail{ID: r.TaskID, Text: "first CRM snapshot"}, tc.preflightErr
				case <-ctx.Done():
					return serviceapi.TaskDetail{}, ctx.Err()
				}
			}}
			secondRead := taskStartGateway{resolve: func(_ context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
				return serviceapi.TaskDetail{ID: r.TaskID, Text: "second CRM snapshot"}, nil
			}}
			type outcome struct {
				receipt serviceapi.WorkSession
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				receipt, err := New(store, panelPolicy{principal: p}, nil, firstRead).ApplySession(ctx, first)
				done <- outcome{receipt, err}
			}()
			select {
			case <-firstReading:
			case out := <-done:
				t.Fatalf("first command did not reach live lookup: %+v", out)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			winner, err := New(store, panelPolicy{principal: p}, nil, secondRead).ApplySession(ctx, second)
			close(release)
			if err != nil || winner.Context.TaskName != "second CRM snapshot" {
				t.Fatalf("second command blocked by in-flight CRM lookup: %+v %v", winner, err)
			}
			out := <-done
			if first.CommandID == second.CommandID {
				if out.err != nil || out.receipt != winner {
					t.Fatalf("concurrent retry did not return original receipt: %+v", out)
				}
			} else if serviceapi.ErrorCode(out.err) != serviceapi.Conflict || out.receipt.ID != "" {
				t.Fatalf("stale command replaced winner: %+v", out)
			}
			page, err := store.ReadSessions(ctx, p.Scope, p.ActorID, 0, time.Now().Add(time.Second).Unix(), "")
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != winner.ID || page.Items[0].StoppedAt != 0 {
				t.Fatalf("concurrent starts wrote extra intervals: %+v %v", page, err)
			}
		})
	}
}
