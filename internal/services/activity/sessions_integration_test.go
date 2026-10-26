package activity

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionsLifecycleReplayConcurrencyAndIsolation(t *testing.T) {
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("requires _test")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7, Kind: serviceapi.PrincipalKindEmployee}
	s := New(store, panelPolicy{principal: p}, nil, nil)
	auth := serviceapi.Auth{Token: "verified"}
	command := serviceapi.SessionCommand{Auth: auth, CommandID: uuid.NewString(), Action: "work", Context: serviceapi.SessionContext{TaskID: 1, TaskName: "Synthetic task", EntityID: 100, EntityType: "leads", PipelineID: 10, StatusID: 11}}
	if _, err = s.ApplySession(ctx, command); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
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
