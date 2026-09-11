package servicerpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Stage 6 MOD-02: the same Activity/Events implementation is compared locally
// and over mTLS. Placement must not change filters, cards, errors or command identity.

type stage6API struct {
	fakeAPI
	directory amocrm.AccountDirectory
}

func (a *stage6API) GetDirectory(context.Context, uuid.UUID) (amocrm.AccountDirectory, error) {
	return a.directory, nil
}

type stage6Repository struct {
	inner  *parityRepository
	writes atomic.Int32
}

func (r *stage6Repository) Settings(ctx context.Context, scope serviceapi.Scope) (serviceapi.Settings, error) {
	return r.inner.Settings(ctx, scope)
}
func (r *stage6Repository) Configure(ctx context.Context, p serviceapi.Principal, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	r.inner.mu.Lock()
	_, existed := r.inner.commands[c.CommandID]
	r.inner.mu.Unlock()
	op, err := r.inner.Configure(ctx, p, c)
	if err == nil && !existed {
		r.writes.Add(1)
	}
	return op, err
}
func (r *stage6Repository) Operation(ctx context.Context, p serviceapi.Principal, id string) (serviceapi.Operation, error) {
	return r.inner.Operation(ctx, p, id)
}
func (r *stage6Repository) ResolveShare(ctx context.Context, hash []byte) (serviceapi.ShareLookup, error) {
	return r.inner.ResolveShare(ctx, hash)
}
func (r *stage6Repository) CreatePanel(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, panel serviceapi.ManagedPanel, hash []byte) (serviceapi.ManagedPanel, error) {
	return r.inner.CreatePanel(ctx, p, c, panel, hash)
}
func (r *stage6Repository) ListPanels(ctx context.Context, scope serviceapi.Scope) ([]serviceapi.ManagedPanel, error) {
	return r.inner.ListPanels(ctx, scope)
}
func (r *stage6Repository) GetPanel(ctx context.Context, scope serviceapi.Scope, id uuid.UUID) (serviceapi.ManagedPanel, error) {
	return r.inner.GetPanel(ctx, scope, id)
}
func (r *stage6Repository) PatchPanel(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	return r.inner.PatchPanel(ctx, p, c)
}
func (r *stage6Repository) RotateShareLink(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, hash []byte, viewKey string) (serviceapi.ManagedPanel, error) {
	return r.inner.RotateShareLink(ctx, p, c, hash, viewKey)
}

type stage6Events struct {
	mu        sync.Mutex
	policy    serviceapi.Policy
	events    []serviceapi.Event
	status    serviceapi.SyncStatus
	commands  map[string]serviceapi.Command
	ops       map[string]serviceapi.Operation
	queries   atomic.Int32
	gets      atomic.Int32
	writes    atomic.Int32
	readError error
}

func (e *stage6Events) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	e.queries.Add(1)
	if _, err := e.policy.Validate(ctx, q.Auth, serviceapi.EventsService, serviceapi.ActionRead); err != nil {
		return serviceapi.QueryResult{}, err
	}
	if err := serviceapi.ValidateQuery(q); err != nil {
		return serviceapi.QueryResult{}, err
	}
	if e.readError != nil {
		return serviceapi.QueryResult{}, e.readError
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result := serviceapi.QueryResult{Events: []serviceapi.Event{}, Summaries: []serviceapi.UserSummary{}, ReadVersion: serviceapi.PresentationReadVersion, PayloadsOmitted: q.Compact, Status: e.status}
	matched := e.matchLocked(q)
	if q.Order == "desc" {
		sort.Slice(matched, func(i, j int) bool {
			return matched[i].CreatedAt > matched[j].CreatedAt || (matched[i].CreatedAt == matched[j].CreatedAt && matched[i].ID > matched[j].ID)
		})
	} else {
		sort.Slice(matched, func(i, j int) bool {
			return matched[i].CreatedAt < matched[j].CreatedAt || (matched[i].CreatedAt == matched[j].CreatedAt && matched[i].ID < matched[j].ID)
		})
	}
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	for _, event := range matched {
		if q.Compact {
			event.ValueBefore, event.ValueAfter = nil, nil
			event.Enrichment, event.Names = nil, nil
		}
		result.Events = append(result.Events, event)
	}
	byUser := map[int64]*serviceapi.UserSummary{}
	cats := map[string]int64{}
	for _, event := range e.matchLocked(q) {
		summary := byUser[event.CreatedBy]
		if summary == nil {
			summary = &serviceapi.UserSummary{UserID: event.CreatedBy, FirstEventAt: event.CreatedAt, LastEventAt: event.CreatedAt}
			byUser[event.CreatedBy] = summary
		}
		summary.UniqueEvents++
		if event.CreatedAt < summary.FirstEventAt {
			summary.FirstEventAt = event.CreatedAt
		}
		if event.CreatedAt > summary.LastEventAt {
			summary.LastEventAt = event.CreatedAt
		}
		category := serviceapi.EventCategory(event.Type)
		cats[category]++
		found := false
		for i := range summary.CategoryCounts {
			if summary.CategoryCounts[i].Category == category {
				summary.CategoryCounts[i].Count++
				found = true
				break
			}
		}
		if !found {
			summary.CategoryCounts = append(summary.CategoryCounts, serviceapi.CategoryCount{Category: category, Count: 1})
		}
		result.Totals.UniqueEvents++
		if event.Type == "task_completed" {
			result.Totals.TaskCompletedEvents++
		}
	}
	for _, id := range []int64{0, 7, 9, 71999} {
		if summary := byUser[id]; summary != nil {
			result.Summaries = append(result.Summaries, *summary)
		}
	}
	for _, category := range serviceapi.KnownCategories() {
		if n := cats[category]; n > 0 {
			result.Totals.CategoryCounts = append(result.Totals.CategoryCounts, serviceapi.CategoryCount{Category: category, Count: n})
		}
	}
	if result.Totals.UniqueEvents > 0 {
		result.Totals.FirstEventAt = result.Events[0].CreatedAt
		result.Totals.LastEventAt = result.Events[len(result.Events)-1].CreatedAt
	}
	buckets, err := serviceapi.QueryTimeBuckets(q)
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	if len(buckets) > 0 {
		result.Timeline = buckets
		for i, bucket := range result.Timeline {
			for _, event := range e.matchLocked(q) {
				if event.CreatedAt >= bucket.StartAt && event.CreatedAt <= bucket.EndAt {
					result.Timeline[i].Count++
				}
			}
		}
	}
	return result, nil
}
func (e *stage6Events) matchLocked(q serviceapi.Query) []serviceapi.Event {
	directory := map[int64]bool{}
	for _, id := range q.DirectoryUserIDs {
		directory[id] = true
	}
	users := map[int64]bool{}
	for _, id := range q.UserIDs {
		users[id] = true
	}
	var matched []serviceapi.Event
	for _, event := range e.events {
		if event.CreatedAt < q.From || event.CreatedAt > q.To {
			continue
		}
		known := len(q.UserIDs) == 0 || users[event.CreatedBy]
		unknown := q.IncludeUnknownAuthors && (event.CreatedBy == 0 || (len(q.DirectoryUserIDs) > 0 && !directory[event.CreatedBy]))
		if !known && !unknown {
			continue
		}
		if len(q.Types) > 0 {
			ok := false
			for _, kind := range q.Types {
				if event.Type == kind {
					ok = true
					break
				}
			}
			if !ok && (q.TypePrefix == "" || !strings.HasPrefix(event.Type, q.TypePrefix)) {
				continue
			}
		} else if q.TypePrefix != "" && !strings.HasPrefix(event.Type, q.TypePrefix) {
			continue
		}
		if q.EntityType != "" && event.EntityType != q.EntityType {
			continue
		}
		if len(q.EntityIDs) > 0 {
			ok := false
			for _, id := range q.EntityIDs {
				if event.EntityID == id {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if len(q.Categories) > 0 {
			ok := false
			category := serviceapi.EventCategory(event.Type)
			for _, want := range q.Categories {
				if category == want {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		matched = append(matched, event)
	}
	return matched
}
func (e *stage6Events) GetEvent(ctx context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	e.gets.Add(1)
	if _, err := e.policy.Validate(ctx, req.Auth, serviceapi.EventsService, serviceapi.ActionRead); err != nil {
		return serviceapi.Event{}, err
	}
	if err := serviceapi.ValidateEventRequest(req); err != nil {
		return serviceapi.Event{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, event := range e.events {
		if event.ID == req.EventID {
			return event, nil
		}
	}
	return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "event not found")
}
func (e *stage6Events) Apply(ctx context.Context, cmd serviceapi.Command) (serviceapi.Operation, error) {
	if _, err := e.policy.Validate(ctx, cmd.Auth, serviceapi.EventsService, serviceapi.ActionSync); err != nil {
		return serviceapi.Operation{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := e.commands[cmd.CommandID]; ok {
		if old.Kind != cmd.Kind || old.InitialDays != cmd.InitialDays || old.RetentionDays != cmd.RetentionDays || old.From != cmd.From || old.To != cmd.To {
			return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "different command")
		}
		return e.ops[cmd.CommandID], nil
	}
	e.writes.Add(1)
	op := serviceapi.Operation{ID: cmd.CommandID, CommandID: cmd.CommandID, State: "succeeded"}
	e.commands[cmd.CommandID] = cmd
	e.ops[cmd.CommandID] = op
	return op, nil
}
func (e *stage6Events) Status(ctx context.Context, auth serviceapi.Auth) (serviceapi.SyncStatus, error) {
	if _, err := e.policy.Validate(ctx, auth, serviceapi.EventsService, serviceapi.ActionStatus); err != nil {
		return serviceapi.SyncStatus{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, nil
}
func (e *stage6Events) Operation(ctx context.Context, req serviceapi.OperationRequest) (serviceapi.Operation, error) {
	if _, err := e.policy.Validate(ctx, req.Auth, serviceapi.EventsService, serviceapi.ActionOperation); err != nil {
		return serviceapi.Operation{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	op, ok := e.ops[req.OperationID]
	if !ok {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return op, nil
}

type stage6DropConfigure struct {
	*product.Service
	dropID    string
	dropped   atomic.Bool
	committed chan serviceapi.Operation
}

func (s *stage6DropConfigure) Configure(ctx context.Context, command serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	op, err := s.Service.Configure(ctx, command)
	if err != nil {
		return op, err
	}
	if command.CommandID == s.dropID && s.dropped.CompareAndSwap(false, true) {
		s.committed <- op
		<-ctx.Done()
		return serviceapi.Operation{}, ctx.Err()
	}
	return op, nil
}

type stage6LegacyActivity struct{ serviceapi.Activity }

type stage6Fixture struct {
	ctx     context.Context
	check   *checker
	policy  *corepolicy.Service
	repo    *stage6Repository
	events  *stage6Events
	local   *product.Service
	remote  serviceapi.Activity
	owner   serviceapi.CRMEvents
	auth    serviceapi.Auth
	query   serviceapi.Query
	clients *Clients
}

func stage6SeedEvents() []serviceapi.Event {
	return []serviceapi.Event{
		{ID: "task-completed-7", CreatedAt: 150, CreatedBy: 7, Type: "task_completed", EntityID: 41, EntityType: "task", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"task":{"id":41}}]`)},
		{ID: "note-added-7", CreatedAt: 160, CreatedBy: 7, Type: "common_note_added", EntityID: 31, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"note":{"id":51}}]`), Enrichment: []serviceapi.EnrichmentObject{{ObjectKind: "note", ObjectKey: "51", State: serviceapi.EnrichmentPending, Source: serviceapi.SourceNotesAPI, Payload: json.RawMessage(`{"text":"synthetic","n":2}`)}}, Names: []serviceapi.CatalogName{{Kind: serviceapi.ObjectEntity, ID: 31, Name: "Lead 31", State: serviceapi.EnrichmentReady}}},
		{ID: "outgoing-call-9", CreatedAt: 170, CreatedBy: 9, Type: "outgoing_call", EntityID: 31, EntityType: "lead", LinkedTalkContactID: 88, ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"call":{"duration":12}}]`)},
		{ID: "lead-added-unknown", CreatedAt: 180, CreatedBy: 71999, Type: "lead_added", EntityID: 99, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)},
		{ID: "chat-system", CreatedAt: 190, CreatedBy: 0, Type: "incoming_chat_message", EntityID: 31, EntityType: "lead", ValueBefore: json.RawMessage(`null`), ValueAfter: json.RawMessage(`[{"message":{"id":3}}]`)},
	}
}

func stage6New(t *testing.T) stage6Fixture {
	t.Helper()
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &stage6Repository{inner: &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}}
	events := &stage6Events{
		policy:   corepolicy.ForCaller(policy, serviceapi.EventsService),
		events:   stage6SeedEvents(),
		commands: map[string]serviceapi.Command{},
		ops:      map[string]serviceapi.Operation{},
		status:   serviceapi.SyncStatus{Enabled: true, State: "idle", HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: 1000, Verification: "stabilized_api_scan"},
	}
	api := &stage6API{directory: amocrm.AccountDirectory{Timezone: "Europe/Moscow", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Alice", GroupID: 3, GroupName: "Sales"}, {ID: 9, Name: "Bob", GroupID: 4, GroupName: "Support"}}}}
	gw := gateway.New(api, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	local := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), events, gw)
	clients := dialTest(t, ca, start(t, ca, &Endpoints{Activity: local, CRMEvents: events, Policy: policy, Ready: func(context.Context) error { return nil }}), serviceapi.CoreService)
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: uuid.NewString(), Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	return stage6Fixture{ctx: ctx, check: check, policy: policy, repo: repo, events: events, local: local, remote: clients.Activity, owner: clients.CRMEvents, auth: auth, query: serviceapi.Query{Auth: auth, From: 90, To: 200, Limit: 100}, clients: clients}
}

func stage6Presenter(t *testing.T, client serviceapi.Activity) serviceapi.EventPresenter {
	t.Helper()
	p, ok := client.(serviceapi.EventPresenter)
	if !ok {
		t.Fatal("Activity omitted event presenter")
	}
	return p
}
func stage6Reader(t *testing.T, client serviceapi.CRMEvents) serviceapi.EventReader {
	t.Helper()
	r, ok := client.(serviceapi.EventReader)
	if !ok {
		t.Fatal("CRM Events omitted event reader")
	}
	return r
}
func stage6Meaning(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out any
	if err = decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
func stage6Equal(t *testing.T, name string, local, remote any) {
	t.Helper()
	if !stage6JSONEq(stage6Meaning(t, local), stage6Meaning(t, remote)) {
		t.Fatalf("%s local=%s remote=%s", name, stage6MustJSON(t, local), stage6MustJSON(t, remote))
	}
}
func stage6JSONEq(a, b any) bool {
	if am, ok := a.(map[string]any); ok {
		bm, ok := b.(map[string]any)
		if !ok {
			return false
		}
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			if !stage6JSONEq(am[k], bm[k]) {
				return false
			}
		}
		return true
	}
	as, aSlice := a.([]any)
	bs, bSlice := b.([]any)
	if a == nil || b == nil {
		empty := func(v any, slice bool, items []any) bool {
			return v == nil || slice && len(items) == 0
		}
		return empty(a, aSlice, as) && empty(b, bSlice, bs)
	}
	if aSlice && bSlice {
		if len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !stage6JSONEq(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}
func stage6MustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func stage6Code(t *testing.T, name string, local, remote error, want serviceapi.Code) {
	t.Helper()
	if serviceapi.ErrorCode(local) != want || serviceapi.ErrorCode(remote) != want {
		t.Fatalf("%s local=%v remote=%v want=%s", name, local, remote, want)
	}
}
func stage6Serve(t *testing.T, ca certAuthority, endpoints *Endpoints) (*grpc.Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(ca.config(t, serviceapi.ActivityService, true), endpoints)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return server, listener.Addr().String()
}

func TestStage6ActivityPresentationLocalAndMTLSGRPCParity(t *testing.T) {
	f := stage6New(t)
	localCard, remoteCard := stage6Presenter(t, f.local), stage6Presenter(t, f.remote)
	localOwner, remoteOwner := f.events, stage6Reader(t, f.owner)
	clients := []serviceapi.Activity{f.local, f.remote}

	fullLocal, err := f.local.Panel(f.ctx, f.query)
	if err != nil {
		t.Fatal(err)
	}
	fullRemote, err := f.remote.Panel(f.ctx, f.query)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "full panel", fullLocal, fullRemote)
	if fullRemote.Data.ReadVersion != serviceapi.PresentationReadVersion || fullRemote.Data.PayloadsOmitted || len(fullRemote.Data.Events) != 5 {
		t.Fatalf("full panel metadata %+v", fullRemote.Data)
	}
	if fullRemote.Data.Events[0].View == nil || fullRemote.Data.Events[0].View.Details != nil || fullRemote.Data.Events[1].View.Category != serviceapi.CategoryNotes {
		t.Fatalf("list view leaked card details %+v", fullRemote.Data.Events[0].View)
	}

	compact := f.query
	compact.Compact = true
	compact.Order = "desc"
	cLocal, err := f.local.Panel(f.ctx, compact)
	if err != nil {
		t.Fatal(err)
	}
	cRemote, err := f.remote.Panel(f.ctx, compact)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "compact panel", cLocal, cRemote)
	if !cRemote.Data.PayloadsOmitted || cRemote.Data.Events[0].ID != "chat-system" || len(cRemote.Data.Events[0].ValueBefore)+len(cRemote.Data.Events[0].ValueAfter) != 0 {
		t.Fatalf("compact omitted payloads incorrectly %+v", cRemote.Data.Events[0])
	}
	if cRemote.Data.Events[0].View == nil || cRemote.Data.Events[0].View.DetailState != serviceapi.DetailOmitted {
		t.Fatalf("compact detail state %+v", cRemote.Data.Events[0].View)
	}

	for _, id := range []string{"note-added-7", "outgoing-call-9"} {
		want, err := localCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		got, err := remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		stage6Equal(t, "event card "+id, want, got)
		ownerLocal, err := localOwner.GetEvent(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		ownerRemote, err := remoteOwner.GetEvent(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		stage6Equal(t, "owner detail "+id, ownerLocal, ownerRemote)
		if got.View == nil || len(got.View.Details) == 0 || ownerRemote.View != nil {
			t.Fatalf("card/owner presentation mixed card=%+v owner=%+v", got.View, ownerRemote.View)
		}
	}
	card, err := remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "note-added-7"})
	if err != nil || card.View == nil || card.View.EnrichmentState != serviceapi.DetailPending || card.View.Category != serviceapi.CategoryNotes {
		t.Fatalf("note card %+v %v", card, err)
	}
	if len(card.Enrichment) != 1 || !reflect.DeepEqual(stage6Meaning(t, card.Enrichment[0].Payload), stage6Meaning(t, json.RawMessage(`{"text":"synthetic","n":2}`))) {
		t.Fatalf("enrichment JSON meaning lost %+v", card.Enrichment)
	}
	call, err := remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "outgoing-call-9"})
	if err != nil || call.LinkedTalkContactID != 88 || call.View.Category != serviceapi.CategoryCalls {
		t.Fatalf("call card %+v %v", call, err)
	}

	tasks := f.query
	tasks.Categories = []string{serviceapi.CategoryTasks}
	tLocal, err := f.local.Panel(f.ctx, tasks)
	if err != nil {
		t.Fatal(err)
	}
	tRemote, err := f.remote.Panel(f.ctx, tasks)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "category filter", tLocal, tRemote)
	if len(tRemote.Data.Events) != 1 || tRemote.Data.Events[0].ID != "task-completed-7" {
		t.Fatalf("categories leaked %+v", tRemote.Data.Events)
	}

	group := f.query
	group.GroupID = 3
	gLocal, err := f.local.Panel(f.ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	gRemote, err := f.remote.Panel(f.ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "group_id", gLocal, gRemote)
	for _, event := range gRemote.Data.Events {
		if event.CreatedBy != 7 {
			t.Fatalf("group_id included other authors %+v", event)
		}
	}
	missingGroup := f.query
	missingGroup.GroupID = 99
	emptyLocal, err := f.local.Panel(f.ctx, missingGroup)
	if err != nil {
		t.Fatal(err)
	}
	emptyRemote, err := f.remote.Panel(f.ctx, missingGroup)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "empty group", emptyLocal, emptyRemote)
	if len(emptyRemote.Data.Events) != 0 || emptyRemote.EmptyReason == "" {
		t.Fatalf("empty group became an error or history %+v", emptyRemote)
	}

	unknown := f.query
	unknown.UserIDs = []int64{7}
	unknown.IncludeUnknownAuthors = true
	uLocal, err := f.local.Panel(f.ctx, unknown)
	if err != nil {
		t.Fatal(err)
	}
	uRemote, err := f.remote.Panel(f.ctx, unknown)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "unknown authors", uLocal, uRemote)
	var authors []int64
	for _, event := range uRemote.Data.Events {
		authors = append(authors, event.CreatedBy)
	}
	if !reflect.DeepEqual(authors, []int64{7, 7, 71999, 0}) {
		t.Fatalf("unknown-author set %+v", authors)
	}

	buckets := f.query
	buckets.Buckets = serviceapi.BucketHour
	bLocal, err := f.local.Panel(f.ctx, buckets)
	if err != nil {
		t.Fatal(err)
	}
	bRemote, err := f.remote.Panel(f.ctx, buckets)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "hour buckets", bLocal, bRemote)
	if len(bRemote.Data.Timeline) == 0 {
		t.Fatal("buckets dropped on the wire")
	}

	ownerQuery, err := f.events.Query(f.ctx, compact)
	if err != nil {
		t.Fatal(err)
	}
	ownerRemoteQuery, err := f.owner.Query(f.ctx, compact)
	if err != nil {
		t.Fatal(err)
	}
	stage6Equal(t, "owner compact query", ownerQuery, ownerRemoteQuery)

	writes, gets := f.repo.writes.Load(), f.events.gets.Load()
	if _, err := f.remote.Panel(f.ctx, f.query); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "note-added-7"}); err != nil {
		t.Fatal(err)
	}
	if f.repo.writes.Load() != writes || f.events.writes.Load() != 0 {
		t.Fatalf("reads persisted commands repo=%d events=%d", f.repo.writes.Load(), f.events.writes.Load())
	}
	if f.events.gets.Load() <= gets {
		t.Fatal("event card skipped the owner reader")
	}

	for _, client := range clients {
		invalid := f.query
		invalid.Categories = []string{"future"}
		if _, err := client.Panel(f.ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("unknown category %v", err)
		}
		invalid = f.query
		invalid.Order = "random"
		if _, err := client.Panel(f.ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("invalid order %v", err)
		}
		invalid = f.query
		invalid.Buckets = "shift"
		if _, err := client.Panel(f.ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("invalid buckets %v", err)
		}
		invalid = f.query
		invalid.GroupID = -1
		if _, err := client.Panel(f.ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("negative group %v", err)
		}
		invalid = f.query
		invalid.To = invalid.From + 32*86400
		if _, err := client.Panel(f.ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("period bound %v", err)
		}
	}
	missingLocal, missingLocalErr := localCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "does-not-exist"})
	missingRemote, missingRemoteErr := remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "does-not-exist"})
	stage6Code(t, "missing card", missingLocalErr, missingRemoteErr, serviceapi.NotFound)
	if missingLocal.ID != "" || missingRemote.ID != "" {
		t.Fatalf("not found returned an envelope %+v %+v", missingLocal, missingRemote)
	}
	badID := serviceapi.EventRequest{Auth: f.auth, EventID: "bad id"}
	stage6Code(t, "invalid event id", errorOf(localCard.EventCard(f.ctx, badID)), errorOf(remoteCard.EventCard(f.ctx, badID)), serviceapi.InvalidArgument)

	forged := f.query
	forged.Auth.Token = "forged"
	stage6Code(t, "panel forged", errorOf(f.local.Panel(f.ctx, forged)), errorOf(f.remote.Panel(f.ctx, forged)), serviceapi.Unauthenticated)
	stage6Code(t, "card forged", errorOf(localCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: forged.Auth, EventID: "note-added-7"})), errorOf(remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: forged.Auth, EventID: "note-added-7"})), serviceapi.Unauthenticated)

	f.check.disabled.Store(true)
	stage6Code(t, "panel denied", errorOf(f.local.Panel(f.ctx, f.query)), errorOf(f.remote.Panel(f.ctx, f.query)), serviceapi.PermissionDenied)
	stage6Code(t, "card denied", errorOf(localCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "note-added-7"})), errorOf(remoteCard.EventCard(f.ctx, serviceapi.EventRequest{Auth: f.auth, EventID: "note-added-7"})), serviceapi.PermissionDenied)
	f.check.disabled.Store(false)
}

func errorOf(_ any, err error) error { return err }

func TestStage6DurableConfigureReplayAfterLostRPCAndRestart(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &stage6Repository{inner: &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}}
	events := &stage6Events{policy: corepolicy.ForCaller(policy, serviceapi.EventsService), events: stage6SeedEvents(), commands: map[string]serviceapi.Command{}, ops: map[string]serviceapi.Operation{}, status: serviceapi.SyncStatus{Enabled: true, State: "idle", HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: 1000}}
	api := &stage6API{directory: amocrm.AccountDirectory{Timezone: "UTC", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Alice"}}}}
	local := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), events, gateway.New(api, corepolicy.ForCaller(policy, serviceapi.GatewayService)))
	commandID := uuid.NewString()
	gate := &stage6DropConfigure{Service: local, dropID: commandID, committed: make(chan serviceapi.Operation, 1)}
	_, address := stage6Serve(t, ca, &Endpoints{Activity: gate, CRMEvents: events})
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "stage6-configure", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	command := serviceapi.SettingsCommand{Auth: auth, CommandID: commandID, Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 10}}
	connection := dialTest(t, ca, address, serviceapi.CoreService)
	lost := make(chan error, 1)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _, err := connection.Activity.Configure(callCtx, command); lost <- err }()
	var committed serviceapi.Operation
	select {
	case committed = <-gate.committed:
	case err := <-lost:
		t.Fatalf("request ended before commit: %v", err)
	case <-callCtx.Done():
		t.Fatal("receiver did not commit")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-lost:
		if err == nil {
			t.Fatal("lost transport reported success")
		}
	case <-callCtx.Done():
		t.Fatal("client did not observe transport loss")
	}
	if repo.writes.Load() != 1 {
		t.Fatalf("commit writes=%d", repo.writes.Load())
	}
	reconnect := dialTest(t, ca, address, serviceapi.CoreService)
	replay, err := reconnect.Activity.Configure(ctx, command)
	if err != nil || !reflect.DeepEqual(replay, committed) {
		t.Fatalf("replay=%+v committed=%+v err=%v", replay, committed, err)
	}
	if repo.writes.Load() != 1 {
		t.Fatalf("replay duplicated inbox writes=%d", repo.writes.Load())
	}
	conflict := command
	conflict.Settings.RetentionDays++
	if _, err := reconnect.Activity.Configure(ctx, conflict); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed payload %v", err)
	}

	syncCmd := serviceapi.Command{Auth: auth, CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 7}
	accepted, err := events.Apply(ctx, syncCmd)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := reconnect.CRMEvents.Apply(ctx, syncCmd)
	if err != nil || !reflect.DeepEqual(accepted, repeated) || events.writes.Load() != 1 {
		t.Fatalf("sync inbox local=%+v grpc=%+v writes=%d err=%v", accepted, repeated, events.writes.Load(), err)
	}

	reads := repo.writes.Load()
	query := serviceapi.Query{Auth: auth, From: 90, To: 200, Limit: 100}
	if _, err := reconnect.Activity.Panel(ctx, query); err != nil {
		t.Fatal(err)
	}
	if _, err := stage6Presenter(t, reconnect.Activity).EventCard(ctx, serviceapi.EventRequest{Auth: auth, EventID: "note-added-7"}); err != nil {
		t.Fatal(err)
	}
	if repo.writes.Load() != reads || events.writes.Load() != 1 {
		t.Fatalf("panel/card created writes repo=%d events=%d", repo.writes.Load(), events.writes.Load())
	}

	restart, restartAddr := stage6Serve(t, ca, &Endpoints{Activity: local, CRMEvents: events})
	afterRestart := dialTest(t, ca, restartAddr, serviceapi.CoreService)
	again, err := afterRestart.Activity.Configure(ctx, command)
	if err != nil || again.CommandID != commandID || repo.writes.Load() != 1 {
		t.Fatalf("restart replay %+v writes=%d err=%v", again, repo.writes.Load(), err)
	}
	restart.Stop()
	if _, err := afterRestart.Activity.Configure(ctx, serviceapi.SettingsCommand{Auth: auth, CommandID: uuid.NewString(), Settings: command.Settings}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("receiver down %v", err)
	}
	if repo.writes.Load() != 1 {
		t.Fatalf("unavailable receiver stored a command writes=%d", repo.writes.Load())
	}
}

func TestStage6ActivityUnavailableIsExplicitNotEmptyHistory(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	coreAddr := start(t, ca, &Endpoints{Policy: policy, Ready: func(context.Context) error { return nil }})
	core := dialTest(t, ca, coreAddr, serviceapi.CoreService)
	if err := core.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Policy.Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "core-still-useful", Grants: serviceapi.UserGrants()}); err != nil {
		t.Fatalf("core issue while activity unregistered: %v", err)
	}
	panel, panelErr := core.Activity.Panel(ctx, serviceapi.Query{From: 90, To: 200, Limit: 1})
	card, cardErr := stage6Presenter(t, core.Activity).EventCard(ctx, serviceapi.EventRequest{EventID: "note-added-7"})
	if serviceapi.ErrorCode(panelErr) != serviceapi.Unavailable || serviceapi.ErrorCode(cardErr) != serviceapi.Unavailable {
		t.Fatalf("unregistered activity panel=%v card=%v", panelErr, cardErr)
	}
	if len(panel.Data.Events) != 0 || card.ID != "" {
		t.Fatalf("unavailable returned history panel=%+v card=%+v", panel, card)
	}

	repo := &stage6Repository{inner: &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}}
	events := &stage6Events{policy: corepolicy.ForCaller(policy, serviceapi.EventsService), events: stage6SeedEvents(), commands: map[string]serviceapi.Command{}, ops: map[string]serviceapi.Operation{}, status: serviceapi.SyncStatus{Enabled: true, State: "idle", HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: 1000}}
	api := &stage6API{directory: amocrm.AccountDirectory{Timezone: "UTC", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Alice"}}}}
	local := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), events, gateway.New(api, corepolicy.ForCaller(policy, serviceapi.GatewayService)))
	var activityDown atomic.Bool
	activityServer, activityAddr := stage6Serve(t, ca, &Endpoints{Activity: local, CRMEvents: events, Ready: func(context.Context) error {
		if activityDown.Load() {
			return serviceapi.Fail(serviceapi.Unavailable, "activity database unavailable")
		}
		return nil
	}})
	activity := dialTest(t, ca, activityAddr, serviceapi.CoreService)
	if err := activity.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	activityDown.Store(true)
	if err := activity.Ready(ctx); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("activity ready %v", err)
	}
	if err := core.Ready(ctx); err != nil {
		t.Fatalf("core ready followed activity: %v", err)
	}
	activityDown.Store(false)

	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "stage6-http", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	principal := widgetauth.Principal{IntegrationID: scope.IntegrationID, InstallationID: scope.InstallationID, UserID: 7}
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(widgetauth.ContextWithPrincipal(r.Context(), principal)))
		})
	}
	bridge := activitybridge.New(nil, corepolicy.ForCaller(policy, serviceapi.CoreService), activity.Activity, events)
	router := chi.NewRouter()
	bridge.RegisterHTTP(router, inject, inject)
	okPanel := httptest.NewRecorder()
	router.ServeHTTP(okPanel, httptest.NewRequest(http.MethodGet, "/api/v1/widget/activity/panel?from=90&to=200", nil))
	if okPanel.Code != http.StatusOK || !strings.Contains(okPanel.Body.String(), `"id":"task-completed-7"`) {
		t.Fatalf("live panel %d %s", okPanel.Code, okPanel.Body.String())
	}
	activityServer.Stop()
	downPanel := httptest.NewRecorder()
	router.ServeHTTP(downPanel, httptest.NewRequest(http.MethodGet, "/api/v1/widget/activity/panel?from=90&to=200", nil))
	if downPanel.Code != http.StatusServiceUnavailable || !strings.Contains(downPanel.Body.String(), `"code":"unavailable"`) || strings.Contains(downPanel.Body.String(), `"events"`) {
		t.Fatalf("down panel must be explicit unavailability: %d %s", downPanel.Code, downPanel.Body.String())
	}
	if _, err := activity.Activity.Panel(ctx, serviceapi.Query{Auth: auth, From: 90, To: 200, Limit: 100}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("down rpc panel %v", err)
	}
	if _, err := stage6Presenter(t, activity.Activity).EventCard(ctx, serviceapi.EventRequest{Auth: auth, EventID: "note-added-7"}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("down rpc card %v", err)
	}
	// Public detail falls back to the Events owner envelope when Activity is unavailable.
	fallback := httptest.NewRecorder()
	router.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/api/v1/widget/activity/events/note-added-7", nil))
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), `"id":"note-added-7"`) || strings.Contains(fallback.Body.String(), `"view"`) {
		t.Fatalf("detail fallback %d %s", fallback.Code, fallback.Body.String())
	}
	bothDown := activitybridge.New(nil, corepolicy.ForCaller(policy, serviceapi.CoreService), activity.Activity, nil)
	bothRouter := chi.NewRouter()
	bothDown.RegisterHTTP(bothRouter, inject, inject)
	missing := httptest.NewRecorder()
	bothRouter.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/widget/activity/events/note-added-7", nil))
	if missing.Code != http.StatusServiceUnavailable || !strings.Contains(missing.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("detail without owners %d %s", missing.Code, missing.Body.String())
	}
}

func TestStage6OldPeerPresentationAndGetEventFailClosed(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "stage6-old", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	events := &stage6Events{policy: corepolicy.ForCaller(policy, serviceapi.EventsService), events: stage6SeedEvents(), commands: map[string]serviceapi.Command{}, ops: map[string]serviceapi.Operation{}, status: serviceapi.SyncStatus{Enabled: true, State: "idle", HistoryFrom: 1, VerifiedFrom: 1, VerifiedThrough: 1000}}
	api := &stage6API{directory: amocrm.AccountDirectory{Timezone: "UTC", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Alice"}}}}
	gw := gateway.New(api, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	current := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), events, gw)
	legacy := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), stage6LegacyActivityReads{CRMEvents: events}, gw)
	remoteLegacy := dialTest(t, ca, start(t, ca, &Endpoints{Activity: stage6LegacyActivity{legacy}}), serviceapi.CoreService).Activity
	query := serviceapi.Query{Auth: auth, From: 90, To: 200, Limit: 10, Categories: []string{serviceapi.CategoryTasks}}
	if _, err := legacy.Panel(ctx, query); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("old read version accepted categories locally: %v", err)
	}
	if _, err := remoteLegacy.Panel(ctx, query); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("old read version accepted categories over RPC: %v", err)
	}
	if _, err := stage6Presenter(t, remoteLegacy).EventCard(ctx, serviceapi.EventRequest{Auth: auth, EventID: "note-added-7"}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("legacy activity without presenter %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(ca.config(t, serviceapi.ActivityService, true))))
	desc := pb.Activity_ServiceDesc
	desc.Methods = nil
	for _, method := range pb.Activity_ServiceDesc.Methods {
		if method.MethodName != "GetEvent" {
			desc.Methods = append(desc.Methods, method)
		}
	}
	server.RegisterService(&desc, &activityServer{impl: current})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	oldPeer := dialTest(t, ca, listener.Addr().String(), serviceapi.CoreService)
	if _, err := oldPeer.Activity.Panel(ctx, serviceapi.Query{Auth: auth, From: 90, To: 200, Limit: 10}); err != nil {
		t.Fatalf("legacy panel rejected: %v", err)
	}
	if _, err := stage6Presenter(t, oldPeer.Activity).EventCard(ctx, serviceapi.EventRequest{Auth: auth, EventID: "note-added-7"}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("old Activity silently ignored GetEvent: %v", err)
	}
}

type stage6LegacyActivityReads struct{ serviceapi.CRMEvents }

func (s stage6LegacyActivityReads) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	result, err := s.CRMEvents.Query(ctx, q)
	if err != nil {
		return result, err
	}
	result.ReadVersion = serviceapi.EventReadVersion
	return result, nil
}
func (s stage6LegacyActivityReads) GetEvent(ctx context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	reader, ok := s.CRMEvents.(serviceapi.EventReader)
	if !ok {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.Unavailable, "event detail reader unavailable")
	}
	return reader.GetEvent(ctx, req)
}
