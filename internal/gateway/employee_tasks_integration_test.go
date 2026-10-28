package gateway

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

// employeePolicy maps "kind:actor" tokens to principals of one scope.
type employeePolicy struct {
	serviceapi.Policy
	scope serviceapi.Scope
}

func (p employeePolicy) Validate(_ context.Context, a serviceapi.Auth, audience, _ string) (serviceapi.Principal, error) {
	kind, actor, ok := strings.Cut(a.Token, ":")
	id, err := strconv.ParseInt(actor, 10, 64)
	if !ok || err != nil || audience != serviceapi.GatewayService {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.Unauthenticated, "bad token")
	}
	if kind == "user" {
		kind = serviceapi.PrincipalKindUser
	}
	return serviceapi.Principal{Scope: p.scope, ActorID: id, Kind: kind}, nil
}

type taskAPI struct {
	API
	mu        sync.Mutex
	open      map[int64][]amocrm.Task
	tasks     map[int64]amocrm.Task
	pageCalls atomic.Int32
	patches   atomic.Int32
	delay     time.Duration
	pageErr   error
	patchErr  error
	// patchLands applies the write before patchErr is returned: a lost response.
	patchLands   bool
	patchDelay   time.Duration
	openErr      error
	renamed      map[int64]string
	names        atomic.Int32
	participants atomic.Int32
	openChecks   atomic.Int32
}

func (a *taskAPI) ListOpenTasksPage(_ context.Context, _ uuid.UUID, actor int64, page int) (amocrm.OpenTaskPage, error) {
	a.pageCalls.Add(1)
	time.Sleep(a.delay)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pageErr != nil {
		return amocrm.OpenTaskPage{}, a.pageErr
	}
	return amocrm.OpenTaskPage{Tasks: append([]amocrm.Task(nil), a.open[actor]...)}, nil
}
func (a *taskAPI) ListTasks(_ context.Context, _ uuid.UUID, ids []int64) ([]amocrm.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if task, ok := a.tasks[ids[0]]; ok {
		return []amocrm.Task{task}, nil
	}
	return []amocrm.Task{}, nil
}
func (a *taskAPI) HasOpenTask(_ context.Context, _ uuid.UUID, actor int64, entityType string, id int64) (bool, error) {
	a.openChecks.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.openErr != nil {
		return false, a.openErr
	}
	for _, task := range a.open[actor] {
		if task.EntityType == entityType && task.EntityID == id {
			return true, nil
		}
	}
	return false, nil
}
func (a *taskAPI) CompleteTask(_ context.Context, _ uuid.UUID, id int64, result string) error {
	a.patches.Add(1)
	time.Sleep(a.patchDelay)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.patchErr != nil && !a.patchLands {
		return a.patchErr
	}
	task := a.tasks[id]
	task.IsCompleted, task.ResultText = true, result
	a.tasks[id] = task
	return a.patchErr
}
func (a *taskAPI) ListTaskTypes(context.Context, uuid.UUID) ([]amocrm.TaskType, error) {
	return []amocrm.TaskType{{ID: 1, Name: "Звонок"}}, nil
}
func (a *taskAPI) ListEntities(_ context.Context, _ uuid.UUID, entityType string, ids []int64) ([]amocrm.EntityName, error) {
	a.names.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []amocrm.EntityName{}
	for _, id := range ids {
		name, ok := a.renamed[id]
		if !ok {
			name = "Deal " + strconv.FormatInt(id, 10)
		}
		out = append(out, amocrm.EntityName{ID: id, EntityType: entityType, Name: name})
	}
	return out, nil
}
func (a *taskAPI) ListPipelines(context.Context, uuid.UUID) ([]amocrm.Pipeline, error) {
	return []amocrm.Pipeline{{ID: 11, Name: "Sales", Statuses: []amocrm.PipelineStatus{{ID: 12, Name: "New", Color: "#fff"}}}}, nil
}
func (a *taskAPI) GetEntityContext(_ context.Context, _ uuid.UUID, entityType string, id int64) (amocrm.EntityContext, error) {
	owner := int64(7)
	if id >= 90 {
		owner = 8
	}
	return amocrm.EntityContext{ID: id, EntityType: entityType, Name: "Lead " + strconv.FormatInt(id, 10), ResponsibleUserID: owner, PipelineID: 11, StatusID: 12}, nil
}
func (a *taskAPI) LeadParticipants(context.Context, uuid.UUID, int64) ([]int64, []int64, error) {
	a.participants.Add(1)
	return []int64{4}, nil, nil
}
func (a *taskAPI) ParticipantLeads(context.Context, uuid.UUID, string, int64) (string, []int64, error) {
	return "Anna", []int64{3, 6}, nil
}

func taskFixture(t *testing.T) (*pgxpool.Pool, serviceapi.Scope, *taskAPI) {
	t.Helper()
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'secret','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	api := &taskAPI{
		open:  map[int64][]amocrm.Task{7: {{ID: 1, EntityID: 3, EntityType: "leads", ResponsibleUserID: 7, TaskTypeID: 1, Text: "Call"}, {ID: 2, EntityID: 6, EntityType: "leads", ResponsibleUserID: 7, Text: "Mail"}}, 8: {{ID: 5, EntityID: 9, EntityType: "leads", ResponsibleUserID: 8, Text: "Other"}}},
		tasks: map[int64]amocrm.Task{1: {ID: 1, EntityID: 3, EntityType: "leads", ResponsibleUserID: 7, TaskTypeID: 1, Text: "Call"}, 5: {ID: 5, ResponsibleUserID: 8, Text: "Secret"}},
	}
	return pool, scope, api
}

func replica(pool *pgxpool.Pool, scope serviceapi.Scope, api *taskAPI) *Service {
	return New(api, employeePolicy{scope: scope}).WithTaskQueues(NewTaskQueueStore(pool))
}

func TestTaskQueueCoalescesTabsAcrossReplicas(t *testing.T) {
	pool, scope, api := taskFixture(t)
	api.delay = 150 * time.Millisecond
	replicas := []*Service{replica(pool, scope, api), replica(pool, scope, api)}
	var wg sync.WaitGroup
	results := make([]serviceapi.TaskQueue, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = replicas[i%2].OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}})
		}(i)
	}
	wg.Wait()
	if got := api.pageCalls.Load(); got != 1 {
		t.Fatalf("8 tabs on 2 replicas made %d amoCRM queue reads", got)
	}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("tab %d: %v", i, errs[i])
		}
		if !r.Stale && (len(r.Tasks) != 2 || r.Tasks[0].TaskTypeName != "Звонок" || r.Tasks[0].EntityName != "Deal 3" || !r.Complete || r.FreshUntil <= r.FetchedAt) {
			t.Fatalf("tab %d queue %+v", i, r)
		}
	}
	// A warm snapshot is served without amoCRM, names come from the snapshot.
	names := api.names.Load()
	if _, err := replicas[1].OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}}); err != nil || api.pageCalls.Load() != 1 || api.names.Load() != names {
		t.Fatalf("warm read reached amoCRM: pages=%d names=%d %v", api.pageCalls.Load(), api.names.Load()-names, err)
	}
}

func TestTaskQueueNamesComeFromSnapshot(t *testing.T) {
	pool, scope, api := taskFixture(t)
	auth := serviceapi.Auth{Token: "employee:7"}
	if _, err := replica(pool, scope, api).OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth}); err != nil {
		t.Fatal(err)
	}
	// Another replica with a cold name cache serves the fresh snapshot as stored.
	names := api.names.Load()
	queue, err := replica(pool, scope, api).OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth})
	if err != nil || queue.Stale || len(queue.Tasks) != 2 || queue.Tasks[0].EntityName != "Deal 3" || queue.Tasks[1].EntityName != "Deal 6" || api.names.Load() != names || api.pageCalls.Load() != 1 {
		t.Fatalf("fresh snapshot %+v %v names=%d", queue, err, api.names.Load()-names)
	}
}

// A stale snapshot outlives the access check that captured it: serving it
// never looks a card up again, so a reassigned lead's new name stays hidden.
func TestStaleTaskQueueNeverResolvesCardsLive(t *testing.T) {
	pool, scope, api := taskFixture(t)
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	loader := replica(pool, scope, api)
	api.open[7] = append(api.open[7], amocrm.Task{ID: 9, EntityID: 99, EntityType: "leads", ResponsibleUserID: 7})
	if queue, err := loader.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil || len(queue.Tasks) != 3 || queue.Tasks[2].EntityName != "Deal 99" {
		t.Fatalf("queue with lead 99 %+v %v", queue, err)
	}
	api.mu.Lock()
	api.open[7] = api.open[7][:2]
	api.renamed = map[int64]string{99: "New"}
	api.mu.Unlock()
	if hidden, err := loader.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 99}); err != nil || !hidden.Restricted {
		t.Fatalf("reassigned lead %+v %v", hidden, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_task_queues SET fresh_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	api.pageErr = &amocrm.APIError{Kind: amocrm.ErrorTemporary, StatusCode: 503, Retryable: true}
	names := api.names.Load()
	for _, s := range []*Service{replica(pool, scope, api), loader} {
		queue, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth, LeadID: 3})
		if err != nil || !queue.Stale || len(queue.Tasks) != 3 {
			t.Fatalf("stale queue %+v %v", queue, err)
		}
		for _, task := range queue.Tasks {
			if task.EntityName == "New" || task.Related != "" {
				t.Fatalf("stale queue disclosed live card data: %+v", queue.Tasks)
			}
		}
		if queue.Tasks[2].EntityName != "Deal 99" {
			t.Fatalf("stored name lost: %+v", queue.Tasks[2])
		}
	}
	if api.names.Load() != names || api.participants.Load() != 0 {
		t.Fatalf("stale queue reached amoCRM: names=%d participants=%d", api.names.Load()-names, api.participants.Load())
	}
}

func TestTaskQueueIsolatesEmployeesAndRejectsOtherPrincipals(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	other, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:8"}})
	if err != nil || len(other.Tasks) != 1 || other.Tasks[0].ID != 5 {
		t.Fatalf("employee 8 queue %+v %v", other, err)
	}
	own, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}})
	if err != nil || len(own.Tasks) != 2 {
		t.Fatalf("employee 7 queue %+v %v", own, err)
	}
	if _, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "user:7"}}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("admin/user principal read employee queue: %v", err)
	}
	// The same installation under another integration never shares a row.
	foreign := New(api, employeePolicy{scope: serviceapi.Scope{InstallationID: scope.InstallationID, IntegrationID: uuid.New()}}).WithTaskQueues(NewTaskQueueStore(pool))
	foreign.taskQueues.wait = 300 * time.Millisecond
	if _, err := foreign.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}}); err == nil {
		t.Fatal("foreign integration reused the installation queue")
	}
}

func TestTaskQueueBacksOffAfterRateLimitAndServesStale(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	auth := serviceapi.Auth{Token: "employee:7"}
	if _, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE activity_task_queues SET fresh_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	api.pageErr = &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429, Retryable: true, RetryAfter: 20 * time.Second}
	stale, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth})
	if err != nil || !stale.Stale || len(stale.Tasks) != 2 || stale.ErrorCode != string(serviceapi.ResourceExhausted) || stale.RetryAfterSeconds < 20 {
		t.Fatalf("429 with snapshot %+v %v", stale, err)
	}
	calls := api.pageCalls.Load()
	for i := 0; i < 5; i++ {
		if again, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: auth}); err != nil || !again.Stale {
			t.Fatalf("backoff read %+v %v", again, err)
		}
	}
	if api.pageCalls.Load() != calls {
		t.Fatal("tabs retried amoCRM during Retry-After")
	}
	// Without a snapshot the backoff is an explicit retryable error.
	_, err = s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:8"}})
	var domain *serviceapi.Error
	if !errors.As(err, &domain) || domain.Code != serviceapi.ResourceExhausted || domain.RetryAfter < 20*time.Second {
		t.Fatalf("cold 429: %v", err)
	}
	if _, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:8"}}); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted || api.pageCalls.Load() != calls+1 {
		t.Fatalf("cold retry during backoff: %v calls=%d", err, api.pageCalls.Load()-calls)
	}
}

func TestTaskQueueExpiredLeaseIsTakenOverAndFenced(t *testing.T) {
	pool, scope, api := taskFixture(t)
	store := NewTaskQueueStore(pool)
	ctx := context.Background()
	stale, _, ok, err := store.acquire(ctx, scope, 7)
	if err != nil || !ok {
		t.Fatalf("first lease %v %v", ok, err)
	}
	if _, _, ok, _ := store.acquire(ctx, scope, 7); ok {
		t.Fatal("live lease acquired twice")
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_task_queues SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(ctx, scope, 7, func(context.Context) ([]storedTask, bool, error) {
		return []storedTask{{ID: 1, Text: "fresh"}}, true, nil
	})
	if err != nil || len(snap.Tasks) != 1 {
		t.Fatalf("takeover %+v %v", snap, err)
	}
	// The expired holder finishes late; its token no longer matches.
	tag, err := pool.Exec(ctx, `UPDATE activity_task_queues SET payload='[]' WHERE installation_id=$1 AND user_id=7 AND lease_token=$2`, scope.InstallationID, stale)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("stale holder overwrote the snapshot: %d %v", tag.RowsAffected(), err)
	}
	_ = api
}

func TestTaskQueueCompletionDuringLoadPublishesDueSnapshot(t *testing.T) {
	pool, scope, _ := taskFixture(t)
	store := NewTaskQueueStore(pool)
	ctx := context.Background()
	snap, err := store.Load(ctx, scope, 7, func(ctx context.Context) ([]storedTask, bool, error) {
		// A completion lands while the queue is being read.
		if err := store.Invalidate(ctx, scope, 7); err != nil {
			t.Fatal(err)
		}
		return []storedTask{{ID: 1}}, true, nil
	})
	if err != nil || len(snap.Tasks) != 1 {
		t.Fatal(err)
	}
	var due bool
	if err := pool.QueryRow(ctx, `SELECT fresh_until<=now() FROM activity_task_queues WHERE installation_id=$1 AND user_id=7`, scope.InstallationID).Scan(&due); err != nil || !due {
		t.Fatalf("overlapping load published as fresh: %v %v", due, err)
	}
}

func TestOwnTaskDeniesOtherEmployeesTask(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	_, err := s.OwnTask(context.Background(), serviceapi.OwnTaskRequest{Auth: serviceapi.Auth{Token: "employee:7"}, TaskID: 5})
	if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || strings.Contains(err.Error(), "Secret") {
		t.Fatalf("foreign task: %v", err)
	}
	if _, err := s.OwnTask(context.Background(), serviceapi.OwnTaskRequest{Auth: serviceapi.Auth{Token: "employee:7"}, TaskID: 404}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("missing task: %v", err)
	}
	detail, err := s.OwnTask(context.Background(), serviceapi.OwnTaskRequest{Auth: serviceapi.Auth{Token: "employee:7"}, TaskID: 1, WithContext: true})
	if err != nil || detail.TaskTypeName != "Звонок" || detail.Entity.Name != "Lead 3" || detail.Entity.PipelineName != "Sales" || detail.Entity.StatusColor != "#fff" {
		t.Fatalf("own task %+v %v", detail, err)
	}
}

func TestCompleteOwnTaskIsIdempotentAndDropsQueueRow(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	if _, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 5, Result: "x"}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || api.patches.Load() != 0 {
		t.Fatalf("completed another employee's task: %v", err)
	}
	if _, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "  "}); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("empty result: %v", err)
	}
	done, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Called"})
	if err != nil || !done.IsCompleted || done.ResultText != "Called" || api.patches.Load() != 1 {
		t.Fatalf("complete %+v %v", done, err)
	}
	// A retry after a lost response finds the completion and does not PATCH.
	again, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Called twice"})
	if err != nil || !again.IsCompleted || again.ResultText != "Called" || api.patches.Load() != 1 {
		t.Fatalf("repeat %+v %v patches=%d", again, err, api.patches.Load())
	}
	queue, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth})
	if err != nil || len(queue.Tasks) != 1 || queue.Tasks[0].ID != 2 || api.pageCalls.Load() != 1 {
		t.Fatalf("queue after completion %+v %v pages=%d", queue, err, api.pageCalls.Load())
	}
	// An unknown PATCH outcome forces a fresh read.
	api.tasks[2] = amocrm.Task{ID: 2, ResponsibleUserID: 7}
	api.patchErr = &amocrm.APIError{Kind: amocrm.ErrorTemporary, StatusCode: 502, Retryable: true}
	if _, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 2, Result: "Mailed"}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("502: %v", err)
	}
	if _, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil || api.pageCalls.Load() != 2 {
		t.Fatalf("unknown outcome kept a fresh queue: pages=%d %v", api.pageCalls.Load(), err)
	}
}

func TestOwnLeadNamesOnlyVisibleLeads(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	lead, err := s.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 3})
	if err != nil || lead.Restricted || lead.Name != "Lead 3" || lead.StatusName != "New" {
		t.Fatalf("own lead %+v %v", lead, err)
	}
	hidden, err := s.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 99})
	if err != nil || !hidden.Restricted || hidden.Name != "" || hidden.PipelineID != 0 {
		t.Fatalf("foreign lead disclosed %+v %v", hidden, err)
	}
	// A lead of the employee's own open task is visible, checked live.
	api.open[7] = append(api.open[7], amocrm.Task{ID: 9, EntityID: 99, EntityType: "leads", ResponsibleUserID: 7})
	if lead, err := s.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 99}); err != nil || lead.Restricted || lead.Name != "Lead 99" {
		t.Fatalf("lead of own task %+v %v", lead, err)
	}
}

func TestRelatedLabelsDecorateOnlyOwnTasks(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	queue, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}, LeadID: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Task 2 is on lead 6, linked to open lead 3 through contact Anna.
	if queue.Tasks[0].Related != "" || queue.Tasks[1].Related != "Anna" {
		t.Fatalf("related labels %+v", queue.Tasks)
	}
}

func TestOwnLeadReassignedTaskRevokesVisibility(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	api.open[7] = append(api.open[7], amocrm.Task{ID: 9, EntityID: 99, EntityType: "leads", ResponsibleUserID: 7})
	if queue, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil || len(queue.Tasks) != 3 {
		t.Fatalf("queue with lead 99 %+v %v", queue, err)
	}
	// The task moves to employee 8; the cached snapshot still lists lead 99.
	api.open[7] = api.open[7][:2]
	hidden, err := s.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 99})
	if err != nil || !hidden.Restricted || hidden.Name != "" || hidden.PipelineID != 0 || hidden.StatusID != 0 || hidden.StatusName != "" {
		t.Fatalf("reassigned lead disclosed %+v %v", hidden, err)
	}
	// A failed live check never grants access.
	api.open[7] = append(api.open[7], amocrm.Task{ID: 9, EntityID: 99, EntityType: "leads", ResponsibleUserID: 7})
	api.openErr = &amocrm.APIError{Kind: amocrm.ErrorTemporary, StatusCode: 502, Retryable: true}
	if lead, err := s.OwnLead(ctx, serviceapi.OwnLeadRequest{Auth: auth, LeadID: 99}); (err == nil && !lead.Restricted) || lead.Name != "" || lead.PipelineID != 0 {
		t.Fatalf("failed check disclosed %+v %v", lead, err)
	}
}

func TestRelatedLabelsSkipInaccessibleLead(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	queue, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}, LeadID: 99})
	if err != nil || len(queue.Tasks) != 2 {
		t.Fatalf("queue %+v %v", queue, err)
	}
	for _, task := range queue.Tasks {
		if task.Related != "" {
			t.Fatalf("participants of a foreign lead disclosed: %+v", queue.Tasks)
		}
	}
	if api.participants.Load() != 0 {
		t.Fatal("foreign lead participants were read")
	}
	// The verdict is cached per actor: the next poll spends no amoCRM call.
	checks := api.openChecks.Load()
	if _, err := s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}, LeadID: 99}); err != nil || api.openChecks.Load() != checks || api.participants.Load() != 0 {
		t.Fatalf("cached verdict: checks=%d participants=%d %v", api.openChecks.Load()-checks, api.participants.Load(), err)
	}
	api.openErr = errors.New("boom")
	queue, err = s.OwnTasks(context.Background(), serviceapi.OwnTasksRequest{Auth: serviceapi.Auth{Token: "employee:7"}, LeadID: 98})
	if err != nil || len(queue.Tasks) != 2 || queue.Tasks[0].Related != "" || queue.Tasks[1].Related != "" || api.participants.Load() != 0 {
		t.Fatalf("failed check decorated %+v %v", queue, err)
	}
}

func TestCompleteOwnTaskOnTwoReplicasPatchesOnce(t *testing.T) {
	pool, scope, api := taskFixture(t)
	api.patchDelay = 200 * time.Millisecond
	replicas := []*Service{replica(pool, scope, api), replica(pool, scope, api)}
	results := make([]serviceapi.TaskDetail, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = replicas[i].CompleteOwnTask(context.Background(), serviceapi.CompleteTaskRequest{Auth: serviceapi.Auth{Token: "employee:7"}, TaskID: 1, Result: "Tab " + strconv.Itoa(i)})
		}(i)
	}
	wg.Wait()
	if api.patches.Load() != 1 {
		t.Fatalf("two tabs sent %d PATCHes", api.patches.Load())
	}
	for i := range results {
		if errs[i] != nil || !results[i].IsCompleted || results[i].ResultText != api.tasks[1].ResultText {
			t.Fatalf("tab %d %+v %v", i, results[i], errs[i])
		}
	}
	var leases int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM activity_task_completions`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("leases left %d %v", leases, err)
	}
}

func TestCompleteOwnTaskReconcilesLandedTimeout(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	if _, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil {
		t.Fatal(err)
	}
	api.patchErr, api.patchLands = context.DeadlineExceeded, true
	done, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Called"})
	if err != nil || !done.IsCompleted || done.ResultText != "Called" || api.patches.Load() != 1 {
		t.Fatalf("landed timeout %+v %v patches=%d", done, err, api.patches.Load())
	}
	if again, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Again"}); err != nil || again.ResultText != "Called" || api.patches.Load() != 1 {
		t.Fatalf("repeat %+v %v patches=%d", again, err, api.patches.Load())
	}
	if queue, err := s.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil || len(queue.Tasks) != 1 || queue.Tasks[0].ID != 2 || api.pageCalls.Load() != 1 {
		t.Fatalf("queue %+v %v pages=%d", queue, err, api.pageCalls.Load())
	}
}

func TestCompleteOwnTaskWaitsForHeldLeaseUntilItExpires(t *testing.T) {
	pool, scope, api := taskFixture(t)
	s := replica(pool, scope, api)
	s.taskQueues.completeWait = 300 * time.Millisecond
	ctx := context.Background()
	auth := serviceapi.Auth{Token: "employee:7"}
	// A crashed holder left a live lease.
	if _, err := pool.Exec(ctx, `INSERT INTO activity_task_completions VALUES($1,1,$2,now()+interval '1 minute')`, scope.InstallationID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	_, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Called"})
	var domain *serviceapi.Error
	if !errors.As(err, &domain) || domain.Code != serviceapi.Unavailable || domain.RetryAfter <= 0 || api.patches.Load() != 0 {
		t.Fatalf("held lease: %v patches=%d", err, api.patches.Load())
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_task_completions SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	done, err := s.CompleteOwnTask(ctx, serviceapi.CompleteTaskRequest{Auth: auth, TaskID: 1, Result: "Called"})
	if err != nil || !done.IsCompleted || api.patches.Load() != 1 {
		t.Fatalf("expired lease %+v %v patches=%d", done, err, api.patches.Load())
	}
}

func TestTaskQueueCompletionDuringFirstLoadIsNotFresh(t *testing.T) {
	pool, scope, _ := taskFixture(t)
	store := NewTaskQueueStore(pool)
	ctx := context.Background()
	snap, err := store.Load(ctx, scope, 7, func(ctx context.Context) ([]storedTask, bool, error) {
		if err := store.RemoveTask(ctx, scope, 7, 1); err != nil {
			t.Fatal(err)
		}
		return []storedTask{{ID: 1}, {ID: 2}}, true, nil
	})
	if err != nil || !snap.Stale || snap.FreshUntil.After(snap.FetchedAt) {
		t.Fatalf("first load raced a completion and stayed fresh: %+v %v", snap, err)
	}
	loads := 0
	snap, err = store.Load(ctx, scope, 7, func(context.Context) ([]storedTask, bool, error) {
		loads++
		return []storedTask{{ID: 2}}, true, nil
	})
	if err != nil || loads != 1 || snap.Stale || len(snap.Tasks) != 1 || snap.Tasks[0].ID != 2 {
		t.Fatalf("next load %+v %v loads=%d", snap, err, loads)
	}
}
