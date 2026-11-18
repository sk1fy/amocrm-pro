package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/companysource"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestCompanyRefreshKind(t *testing.T) {
	a := companyActivation{uuid.MustParse("10000000-0000-0000-0000-000000000001"), math.MaxInt64}
	k := companyRefreshKind(a)
	if len(k) > 80 || string(k) != "company.refresh.10000000-0000-0000-0000-000000000001.9223372036854775807" {
		t.Fatal(k)
	}
}
func requireCompanyProcess(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(key) == "" {
			t.Skip("run make platform-test for company projection process checks")
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		owner, e := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 1)
		if e != nil {
			t.Error(e)
			return
		}
		defer owner.Close()
		_, e = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET status='dead',lease_until=NULL WHERE kind LIKE 'company.refresh.%' AND status IN ('queued','running')`)
		if e != nil {
			t.Error(e)
		}
	})

}
func companyWait(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("company projection condition timed out")
}
func TestPlatformCompanyProjectionAcrossReplicas(t *testing.T) {
	requireCompanyProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	var mu sync.Mutex
	at := time.Now().UTC().Truncate(time.Microsecond)
	until := at.Add(5 * time.Minute)
	mode := "full"
	calls := 0
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if len(r.TLS.PeerCertificates[0].URIs) != 1 || r.TLS.PeerCertificates[0].URIs[0].String() != "spiffe://amocrm-pro/platform-company-crm" {
			w.WriteHeader(403)
			return
		}
		s := companycrm.Source{AccountID: 12, BindingRevision: "r1", State: "fresh", Complete: true, ObservedAt: &at, FreshUntil: &until}
		if mode == "partial" {
			s.Complete = false
			s.State = "error"
			s.ErrorCode = "source_unavailable"
			w.WriteHeader(503)
		}
		if mode == "rate" {
			s.Complete = false
			s.State = "error"
			s.RetryAfter = 30
			w.Header().Set("Retry-After", "20")
			w.WriteHeader(429)
		}
		if mode == "stale" {
			s.State = "stale"
			expired := at.Add(-time.Minute)
			s.FreshUntil = &expired
		}
		if r.URL.Path == "/v1/company-crm/clients" {
			data := &companycrm.Clients{Companies: []companycrm.Client{{CompanyID: 11, Name: "A", AccountIDs: []int64{1, 2}, Resolution: "multiple"}, {CompanyID: 22, Name: "B", AccountIDs: []int64{2}, Resolution: "multiple"}}, Contacts: []companycrm.Contact{{ID: 33, Name: "Contact", CompanyIDs: []int64{11, 22, 99}}}}
			if mode == "partial" || mode == "rate" {
				data = nil
			}
			if mode == "empty" {
				data = &companycrm.Clients{Companies: []companycrm.Client{}, Contacts: []companycrm.Contact{}}
			}
			_ = json.NewEncoder(w).Encode(companycrm.Response[companycrm.Clients]{Source: s, Data: data})
			return
		}
		p := amocrm.CompanyCRMPipeline{ID: 100, AccountID: 12, Name: "Sales"}
		p.Embedded.Statuses = []amocrm.CompanyCRMStage{{ID: 101, PipelineID: 100, AccountID: 12, Name: "New"}}
		data := &companycrm.WorkflowMetadata{RequiredFieldsState: "unknown", Pipelines: []amocrm.CompanyCRMPipeline{p}, LeadFields: []amocrm.CompanyCRMField{}}
		if mode == "partial" || mode == "rate" {
			data = nil
		}
		_ = json.NewEncoder(w).Encode(companycrm.Response[companycrm.WorkflowMetadata]{Source: s, Data: data})
	}))
	source.TLS, err = PrivateTLS(filepath.Join(certs, "core/tls.crt"), filepath.Join(certs, "core/tls.key"), filepath.Join(certs, "core/ca.crt"))
	mustManager(t, err)
	source.StartTLS()
	defer source.Close()
	origin := strings.Replace(source.URL, "127.0.0.1", "localhost", 1)
	sourceEnv := []string{"PLATFORM_WORKERS=2", "PLATFORM_COMPANY_CORE_ORIGIN=" + origin, "PLATFORM_COMPANY_CORE_CA_FILE=" + filepath.Join(certs, "platform-company-crm/ca.crt"), "PLATFORM_COMPANY_CORE_CERT_FILE=" + filepath.Join(certs, "platform-company-crm/tls.crt"), "PLATFORM_COMPANY_CORE_KEY_FILE=" + filepath.Join(certs, "platform-company-crm/tls.key"), "PLATFORM_COMPANY_SOURCE_ACCOUNT_ID=12", "PLATFORM_COMPANY_BINDING_REVISION=r1"}
	first, second := startManagerProcess(t, sourceEnv...), startManagerProcess(t, sourceEnv...)
	client := tlsClient(t, certs, "manager-bff")
	core := tlsClient(t, certs, "core")
	pilot.deliver(t, core, first.private)
	editor := managerTestActor{uuid.New(), 1, []string{"catalog.manage"}}
	reader := managerTestActor{uuid.New(), 1, []string{"clients.read"}}
	// Adopt a real product through the existing command boundary.
	productBody := fmt.Sprintf(`{"code":"pilot-review","display_name":"Reviewed","line":"PRO","availability":"pilot","integration_refs":[%q],"tariff_version":null,"trial_policy":{"kind":"once","duration_days":7},"links":[]}`, pilot.integration)
	managerRequest(t, client, "POST", first.private+managerPath+"products", editor, newManagerTestCommand(uuid.Nil, 0), productBody, 201)
	workflowPath := managerPath + "products/" + pilot.product.String() + "/company-workflow"
	absent := managerRequest(t, client, "GET", second.private+workflowPath, editor, managerTestCommand{}, "", 200)
	if absent.etag != `"1"` || !bytes.Contains(absent.body, []byte(`"configuration":null`)) {
		t.Fatalf("virtual workflow: %s", absent.body)
	}
	var count int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.product_workflow_heads`).Scan(&count))
	if count != 0 {
		t.Fatal("GET inserted head")
	}
	var metadata companyWorkflowMetadata
	companyWait(t, func() bool {
		resp := managerCall(client, "GET", first.private+managerPath+"company-workflow-metadata", editor, managerTestCommand{}, "")
		if resp.status != 200 {
			return false
		}
		return json.Unmarshal(resp.body, &metadata) == nil && metadata.Generation != nil
	})
	var page companyPage
	companyWait(t, func() bool {
		resp := managerCall(client, "GET", first.private+managerPath+"companies?limit=1", reader, managerTestCommand{}, "")
		return resp.status == 200 && json.Unmarshal(resp.body, &page) == nil && len(page.Items) == 1
	})
	if page.Next == nil || page.Items[0].CRMID != "11" || page.Items[0].RuntimeResolution != "ambiguous" || len(page.Items[0].CustomerAccounts) != 2 || len(page.Items[0].RuntimeAccounts) != 0 {
		t.Fatalf("company mapping: %+v", page)
	}
	id := uuid.UUID(page.Items[0].ID)
	detail := managerRequest(t, client, "GET", first.private+managerPath+"companies/"+id.String(), reader, managerTestCommand{}, "", 200)
	if !bytes.Contains(detail.body, []byte(`"crm_company_id":"99","company_id":null`)) {
		t.Fatalf("dangling contact lost: %s", detail.body)
	}
	cursorPath := managerPath + "companies?limit=1&cursor=" + url.QueryEscape(*page.Next)
	next := managerRequest(t, client, "GET", second.private+cursorPath, reader, managerTestCommand{}, "", 200)
	if !bytes.Contains(next.body, []byte(`"crm_company_id":"22"`)) {
		t.Fatal(string(next.body))
	}
	revoked := reader
	revoked.version++
	managerRequest(t, client, "GET", second.private+cursorPath, revoked, managerTestCommand{}, "", 400)
	managerRequest(t, client, "GET", second.private+managerPath+"company-workflow-metadata", reader, managerTestCommand{}, "", 403)
	body := fmt.Sprintf(`{"source_account_id":"12","binding_revision":"r1","metadata_generation_id":%q,"pipeline_id":"100","initial_stage_id":"101"}`, *metadata.Generation)
	bad := strings.Replace(body, `"101"`, `"999"`, 1)
	badCmd := newManagerTestCommand(pilot.product, 1)
	managerRequest(t, client, "PUT", first.private+workflowPath, editor, badCmd, bad, 422).requireCode(t, "company_stage_mismatch")
	managerRequest(t, client, "GET", second.private+badCmd.receiptPath(), editor, managerTestCommand{}, "", 200)
	a, b := newManagerTestCommand(pilot.product, 1), newManagerTestCommand(pilot.product, 1)
	results := make(chan struct {
		r observedManagerResponse
		c managerTestCommand
	}, 2)
	go func() {
		results <- struct {
			r observedManagerResponse
			c managerTestCommand
		}{managerCall(client, "PUT", first.private+workflowPath, editor, a, body), a}
	}()
	go func() {
		results <- struct {
			r observedManagerResponse
			c managerTestCommand
		}{managerCall(client, "PUT", second.private+workflowPath, editor, b, body), b}
	}()
	var winner managerTestCommand
	var saved observedManagerResponse
	statuses := map[int]int{}
	for range 2 {
		result := <-results
		if result.r.err != nil {
			t.Fatal(result.r.err)
		}
		statuses[result.r.status]++
		if result.r.status == 200 {
			winner, saved = result.c, result.r
		}
	}
	if statuses[200] != 1 || statuses[409] != 1 || saved.etag != `"2"` {
		t.Fatalf("first-save CAS statuses=%v body=%s", statuses, saved.body)
	}
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.audit WHERE kind='product.company_workflow.set'`).Scan(&count))
	if count != 1 {
		t.Fatal("audit not atomic")
	}
	var productVersion int64
	mustManager(t, owner.QueryRow(ctx, `SELECT catalog_version FROM platform_lifecycle.products WHERE id=$1`, pilot.product).Scan(&productVersion))
	if productVersion != 1 {
		t.Fatal("workflow advanced product card")
	}
	// A transient observation failure rolls back the command reservation.
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET attempt_state='stale',error_code=NULL WHERE component='metadata'`)
	mustManager(t, err)
	staleMetadata := managerRequest(t, client, "GET", first.private+managerPath+"company-workflow-metadata", editor, managerTestCommand{}, "", 200)
	if !bytes.Contains(staleMetadata.body, []byte(`"state":"stale"`)) {
		t.Fatal("stale source promoted")
	}
	staleReplay := managerRequest(t, client, "PUT", second.private+workflowPath, editor, winner, body, 200)
	if !bytes.Equal(staleReplay.body, saved.body) {
		t.Fatal("stale metadata blocked committed replay")
	}
	retryCommand := newManagerTestCommand(pilot.product, 2)
	transient := managerRequest(t, client, "PUT", first.private+workflowPath, editor, retryCommand, body, 503)
	if !bytes.Contains(transient.body, []byte(`"retryable":true`)) {
		t.Fatal("transient metadata failure marked definitive")
	}
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_manager.commands WHERE id=$1`, retryCommand.id).Scan(&count))
	if count != 0 {
		t.Fatal("transient command reserved")
	}
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET attempt_state='fresh',error_code=NULL WHERE component='metadata'`)
	mustManager(t, err)
	managerRequest(t, client, "PUT", second.private+workflowPath, editor, retryCommand, body, 200)
	// Catalog and workflow commands serialize their own versions without sharing ETags.
	for i := int64(0); i < 3; i++ {
		done := make(chan observedManagerResponse, 2)
		go func() {
			done <- managerCall(client, "PATCH", first.private+managerPath+"products/"+pilot.product.String(), editor, newManagerTestCommand(pilot.product, 1+i), fmt.Sprintf(`{"display_name":"Updated catalog name %d"}`, i))
		}()
		go func() {
			done <- managerCall(client, "PUT", second.private+workflowPath, editor, newManagerTestCommand(pilot.product, 3+i), body)
		}()
		for range 2 {
			response := <-done
			if response.err != nil || response.status != 200 {
				t.Fatalf("catalog/workflow concurrent command: status=%d err=%v body=%s", response.status, response.err, response.body)
			}
		}
	}
	// Both replicas share one activation and cannot rewrite evidence or retarget it.
	var activation companyActivation
	mustManager(t, owner.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.source_control`).Scan(&activation.Scope, &activation.Epoch))
	for _, sql := range []string{`UPDATE platform_company.generations SET source_state='stale'`, `DELETE FROM platform_company.company_rows`, `UPDATE platform_company.source_control SET epoch=epoch+1`, `SELECT * FROM platform_company.activate_source(NULL,1,13,'r2',gen_random_uuid())`} {
		if _, err = jobs.Exec(ctx, sql); err == nil {
			t.Fatalf("runtime allowed %s", sql)
		}
	}
	var ignored uuid.UUID
	err = jobs.QueryRow(ctx, `SELECT scope_id FROM platform_company.register_source(13,'r2')`).Scan(&ignored)
	if err != pgx.ErrNoRows {
		t.Fatalf("runtime retarget: %v", err)
	}
	// Stop workers to make queue routing and old publication checks deterministic.
	first.stop(t)
	second.stop(t)
	cfg, err := companysource.ParseConfig(func(k string) string {
		for _, v := range sourceEnv {
			key, value, _ := strings.Cut(v, "=")
			if key == k {
				return value
			}
		}
		return ""
	})
	mustManager(t, err)
	sourceClient, err := companysource.New(cfg)
	mustManager(t, err)
	defer sourceClient.Close()
	old, err := NewCompanyObserver(ctx, jobs, sourceClient)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET next_refresh=NULL,current_job_id=NULL`)
	mustManager(t, err)
	mustManager(t, old.enrollCompany(ctx))
	lease, found, err := queue.NewStore(jobs).Claim(ctx, []queue.Kind{old.Kind()}, time.Minute)
	mustManager(t, err)
	if !found {
		t.Fatal("old job not claimed")
	}
	completion := old.RefreshJob(ctx, lease.Job)
	var rebound companyActivation
	mustManager(t, owner.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.activate_source($1,$2,13,'r2',$3)`, activation.Scope, activation.Epoch, editor.subject).Scan(&rebound.Scope, &rebound.Epoch))
	tx, err := jobs.Begin(ctx)
	mustManager(t, err)
	outcome, err := completion.Publish(ctx, tx)
	mustManager(t, err)
	mustManager(t, tx.Commit(ctx))
	if outcome.FailureCode != "source_scope_changed" {
		t.Fatal("old publication not fenced")
	}
	cfg.AccountID = 13
	cfg.Revision = "r2"
	newClient, err := companysource.New(cfg)
	mustManager(t, err)
	defer newClient.Close()
	newObserver, err := NewCompanyObserver(ctx, jobs, newClient)
	mustManager(t, err)
	mustManager(t, newObserver.enrollCompany(ctx))
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	cfgWorker := queue.DefaultConfig(1)
	cfgWorker.PollInterval = 20 * time.Millisecond
	cfgWorker.SweepInterval = time.Hour
	worker := queue.NewWorker(queue.NewStore(jobs), map[queue.Kind]queue.Handler{old.Kind(): old.RefreshJob}, cfgWorker, prometheus.NewRegistry())
	go func() { defer close(workerDone); worker.Run(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()
	time.Sleep(150 * time.Millisecond)
	// Old kind can claim only old queued work, never the new activation's jobs.
	for range 3 {
		l, ok, e := queue.NewStore(jobs).Claim(ctx, []queue.Kind{old.Kind()}, time.Minute)
		mustManager(t, e)
		if ok && l.Kind != old.Kind() {
			t.Fatal("old observer claimed new kind")
		}
	}
	mustManager(t, owner.QueryRow(ctx, `SELECT coalesce(sum(attempt),0) FROM platform_jobs.jobs WHERE kind=$1`, newObserver.Kind()).Scan(&count))
	if count != 0 {
		t.Fatal("new activation jobs consumed by old worker")
	}
	var aba companyActivation
	mustManager(t, owner.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.activate_source($1,$2,12,'r1',$3)`, rebound.Scope, rebound.Epoch, editor.subject).Scan(&aba.Scope, &aba.Epoch))
	if aba.Scope != activation.Scope || aba.Epoch != 3 {
		t.Fatal("ABA epoch")
	}
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.source_components WHERE scope_id=$1 AND (current_generation IS NOT NULL OR current_job_id IS NOT NULL)`, aba.Scope).Scan(&count))
	if count != 0 {
		t.Fatal("ABA retained current pointer")
	}
	mustManager(t, old.enrollCompany(ctx))
	fresh, err := NewCompanyObserver(ctx, jobs, sourceClient)
	mustManager(t, err)
	mustManager(t, fresh.enrollCompany(ctx))
	_, _, err = queue.NewStore(jobs).Claim(ctx, []queue.Kind{old.Kind()}, time.Minute)
	mustManager(t, err)
	mustManager(t, owner.QueryRow(ctx, `SELECT coalesce(sum(attempt),0) FROM platform_jobs.jobs WHERE kind=$1`, fresh.Kind()).Scan(&count))
	if count != 0 {
		t.Fatal("old worker claimed ABA job")
	}
	time.Sleep(150 * time.Millisecond)
	mustManager(t, owner.QueryRow(ctx, `SELECT coalesce(sum(attempt),0) FROM platform_jobs.jobs WHERE kind=$1`, fresh.Kind()).Scan(&count))
	if count != 0 {
		t.Fatal("constructed old worker claimed ABA activation")
	}
	staleEnv := append(append([]string{}, sourceEnv...), "PLATFORM_COMPANY_SOURCE_ACCOUNT_ID=13", "PLATFORM_COMPANY_BINDING_REVISION=r2")
	staleReplica := startManagerProcess(t, staleEnv...)
	replay := managerRequest(t, client, "PUT", staleReplica.private+workflowPath, editor, winner, body, 200)
	if !bytes.Equal(replay.body, saved.body) || replay.etag != saved.etag {
		t.Fatal("rebind replay changed")
	}
	managerRequest(t, client, "GET", staleReplica.private+winner.receiptPath(), editor, managerTestCommand{}, "", 200)
	managerRequest(t, client, "PUT", staleReplica.private+workflowPath, editor, winner, strings.Replace(body, `"101"`, `"102"`, 1), 409)
	noGrant := editor
	noGrant.grants = []string{"clients.read"}
	managerRequest(t, client, "GET", staleReplica.private+winner.receiptPath(), noGrant, managerTestCommand{}, "", 403)
	managerRequest(t, client, "GET", staleReplica.private+cursorPath, reader, managerTestCommand{}, "", 400)
	currentPage := managerRequest(t, client, "GET", staleReplica.private+managerPath+"companies", reader, managerTestCommand{}, "", 200)
	if !bytes.Contains(currentPage.body, []byte(`"items":null`)) {
		t.Fatalf("ABA exposed old data: %s", currentPage.body)
	}
	restarted := startManagerProcess(t, sourceEnv...)
	startManagerProcess(t, sourceEnv...)
	companyWait(t, func() bool {
		r := managerCall(client, "GET", restarted.private+managerPath+"companies", reader, managerTestCommand{}, "")
		return r.status == 200 && bytes.Contains(r.body, []byte(id.String()))
	})
	// A failed full read retains the last complete generation and a shared cooldown.
	mu.Lock()
	mode = "rate"
	mu.Unlock()
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET next_refresh=NULL`)
	mustManager(t, err)
	companyWait(t, func() bool {
		var retry *time.Time
		e := owner.QueryRow(ctx, `SELECT retry_not_before FROM platform_company.source_control`).Scan(&retry)
		return e == nil && retry != nil && retry.After(time.Now())
	})
	mu.Lock()
	before := calls
	mu.Unlock()
	time.Sleep(3500 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("shared cooldown bypassed: %d -> %d", before, after)
	}
	retained := managerRequest(t, client, "GET", restarted.private+managerPath+"companies", reader, managerTestCommand{}, "", 200)
	if !bytes.Contains(retained.body, []byte(id.String())) {
		t.Fatal("failure replaced full snapshot")
	}
}

func TestPlatformCompanyPublicationBoundaries(t *testing.T) {
	requireCompanyProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	var activation companyActivation
	mustManager(t, jobs.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source(12,'r1')`).Scan(&activation.Scope, &activation.Epoch))
	at := time.Now().UTC().Truncate(time.Microsecond)
	until := at.Add(5 * time.Minute)
	pipeline := amocrm.CompanyCRMPipeline{ID: 100, AccountID: 12, Name: "Sales"}
	pipeline.Embedded.Statuses = []amocrm.CompanyCRMStage{{ID: 101, AccountID: 12, PipelineID: 100, Name: "New"}}
	obs := companyObservation{source: companycrm.Source{AccountID: 12, BindingRevision: "r1", State: "fresh", Complete: true, ObservedAt: &at, FreshUntil: &until}, metadata: &companycrm.WorkflowMetadata{RequiredFieldsState: "unknown", Pipelines: []amocrm.CompanyCRMPipeline{pipeline}}}
	publish := func(p companyJobPayload, o companyObservation) queue.Outcome {
		t.Helper()
		id := uuid.New()
		_, e := owner.Exec(ctx, `INSERT INTO platform_jobs.jobs(id,kind,payload) VALUES($1,$2,'{}');`, id, companyRefreshKind(activation))
		mustManager(t, e)
		_, e = owner.Exec(ctx, `UPDATE platform_company.source_components SET current_job_id=$3 WHERE scope_id=$1 AND component=$2`, p.Scope, p.Component, id)
		mustManager(t, e)
		tx, e := jobs.Begin(ctx)
		mustManager(t, e)
		defer func() { _ = tx.Rollback(ctx) }()
		outcome, e := publishCompanyObservation(ctx, tx, p, id, o)
		mustManager(t, e)
		mustManager(t, tx.Commit(ctx))
		return outcome
	}
	p := companyJobPayload{activation, companyMetadata}
	publish(p, obs)
	initial, err := (managerStore{jobs}).companyWorkflowMetadata(ctx)
	mustManager(t, err)
	if initial.Source.State != "fresh" {
		t.Fatal(initial.Source)
	}
	// Same timestamp and hash with stale source state must downgrade fresh evidence before expiry.
	obs.source.State = "stale"
	obs.source.ErrorCode = "source_unavailable"
	publish(p, obs)
	stale, err := (managerStore{jobs}).companyWorkflowMetadata(ctx)
	mustManager(t, err)
	if stale.Source.State != "stale" || stale.Source.ErrorCode == nil || *stale.Source.ErrorCode != "source_unavailable" || stale.Generation == nil || *stale.Generation != *initial.Generation {
		t.Fatalf("stale duplicate promoted: %+v", stale)
	}
	var count int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.generations`).Scan(&count))
	if count != 1 {
		t.Fatal("duplicate generation")
	}
	// Equal-time changed content is an inconsistency; an older response never becomes current.
	obs.metadata.Pipelines[0].Name = "Changed"
	publish(p, obs)
	inconsistent, err := (managerStore{jobs}).companyWorkflowMetadata(ctx)
	mustManager(t, err)
	if inconsistent.Source.ErrorCode == nil || *inconsistent.Source.ErrorCode != "source_inconsistent" || inconsistent.Pipelines[0].Name != "Sales" {
		t.Fatalf("equal timestamp overwrite: %+v", inconsistent)
	}
	old := at.Add(-time.Second)
	obs.source.ObservedAt = &old
	obs.source.State = "fresh"
	publish(p, obs)
	unchanged, err := (managerStore{jobs}).companyWorkflowMetadata(ctx)
	mustManager(t, err)
	if *unchanged.Generation != *initial.Generation {
		t.Fatal("older observation became current")
	}
	// A bare 429 still establishes the shared minimum cooldown.
	publish(p, companyObservation{code: "source_rate_limited"})
	var retry *time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT retry_not_before FROM platform_company.source_control`).Scan(&retry))
	if retry == nil || time.Until(*retry) < 4*time.Second {
		t.Fatal("bare 429 omitted shared cooldown")
	}
	// Full empty data and a failed attempt remain different states.
	clientsObservation := companyObservation{source: companycrm.Source{AccountID: 12, BindingRevision: "r1", State: "fresh", Complete: true, ObservedAt: &at, FreshUntil: &until}, clients: &companycrm.Clients{Companies: []companycrm.Client{}, Contacts: []companycrm.Contact{}}}
	clientPayload := companyJobPayload{activation, companyClients}
	publish(clientPayload, clientsObservation)
	page, _, e := (managerStore{jobs}).companies(ctx, companyFilters{Limit: 25}, companySeek{})
	mustManager(t, e)
	if page.Items == nil || len(page.Items) != 0 || !page.Source.Complete {
		t.Fatal("complete empty was null")
	}
	publish(clientPayload, companyObservation{source: companycrm.Source{State: "disabled"}, code: "source_incomplete"})
	page, _, e = (managerStore{jobs}).companies(ctx, companyFilters{Limit: 25}, companySeek{})
	mustManager(t, e)
	if page.Items == nil || page.Source.State != "disabled" || !page.Source.Complete {
		t.Fatal("disabled attempt erased retained empty data")
	}
	var tables int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='platform_company' AND table_type='BASE TABLE'`).Scan(&tables))
	if tables != 14 {
		t.Fatalf("tables=%d", tables)
	}
	access, e := storage.Open(ctx, os.Getenv("PLATFORM_TEST_ACCESS_DSN"), storage.Access, 1)
	mustManager(t, e)
	defer access.Close()
	if _, e = access.Exec(ctx, `SELECT * FROM platform_company.company_rows`); e == nil {
		t.Fatal("access role read private company data")
	}
	// Relational evidence and audit cannot be forged by runtime.
	for _, sql := range []string{
		`INSERT INTO platform_company.audit(id,kind,scope_id,epoch,previous_scope,previous_epoch,actor_id) SELECT gen_random_uuid(),'source.activated',scope_id,2,scope_id,1,gen_random_uuid() FROM platform_company.source_control`,
		`INSERT INTO platform_company.workflow_stages VALUES('00000000-0000-0000-0000-000000000001',100,999,'bad')`,
	} {
		if _, err = jobs.Exec(ctx, sql); err == nil {
			t.Fatalf("runtime accepted %s", sql)
		}
	}
	tx, err := owner.Begin(ctx)
	mustManager(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO platform_company.audit(id,kind,scope_id,actor_id) SELECT gen_random_uuid(),'source.activated',scope_id,gen_random_uuid() FROM platform_company.source_control`)
	if err == nil {
		t.Fatal("NULL activation epoch bypassed CHECK")
	}
	_ = tx.Rollback(ctx)
}

type coreCompanyFixture struct {
	binding             companycrm.Binding
	companies, contacts int
}

func (f coreCompanyFixture) Resolve(context.Context) (companycrm.Binding, error) {
	return f.binding, nil
}
func (coreCompanyFixture) VerifyCompanyCRMAccount(context.Context, uuid.UUID, int64) error {
	return nil
}
func (coreCompanyFixture) CompanyCRMStaff(context.Context, uuid.UUID) ([]amocrm.CompanyCRMUser, error) {
	return []amocrm.CompanyCRMUser{}, nil
}
func (f coreCompanyFixture) CompanyCRMCompanies(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMCompany, error) {
	result := make([]amocrm.CompanyCRMCompany, 0, max(1, f.companies))
	for i := 0; i < max(1, f.companies); i++ {
		var row amocrm.CompanyCRMCompany
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"id":%d,"account_id":12,"name":"Actual Core boundary","custom_fields_values":[{"field_id":9,"field_type":"numeric","values":[{"value":"%d"}]}]}`, 71+i, 1000+i)), &row)
		result = append(result, row)
	}
	return result, nil
}
func (f coreCompanyFixture) CompanyCRMContacts(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMContact, error) {
	result := make([]amocrm.CompanyCRMContact, 0, f.contacts)
	for i := 0; i < f.contacts; i++ {
		var row amocrm.CompanyCRMContact
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"id":%d,"account_id":12,"name":"Contact","_embedded":{"companies":[{"id":%d},{"id":999999}]}}`, 10000+i, 71+i%max(1, f.companies))), &row)
		result = append(result, row)
	}
	return result, nil
}
func (coreCompanyFixture) CompanyCRMFields(context.Context, uuid.UUID, string) ([]amocrm.CompanyCRMField, error) {
	return []amocrm.CompanyCRMField{{ID: 9, Type: "numeric", Name: "Customer accounts"}}, nil
}
func (coreCompanyFixture) CompanyCRMPipelines(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMPipeline, error) {
	p := amocrm.CompanyCRMPipeline{ID: 80, AccountID: 12, Name: "Core pipeline"}
	p.Embedded.Statuses = []amocrm.CompanyCRMStage{{ID: 81, PipelineID: 80, AccountID: 12, Name: "Core stage"}}
	return []amocrm.CompanyCRMPipeline{p}, nil
}
func TestPlatformCompanyCoreTLSChain(t *testing.T)          { testCompanyCoreTLSChain(t, 1, 0) }
func TestPlatformCompanyRepresentativeCatalog(t *testing.T) { testCompanyCoreTLSChain(t, 500, 1000) }
func testCompanyCoreTLSChain(t *testing.T, companies, contacts int) {
	requireCompanyProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	fixture := coreCompanyFixture{binding: companycrm.Binding{AccountID: 12, Revision: "core-boundary", InstallationID: uuid.New(), IntegrationID: uuid.New()}}
	fixture.companies, fixture.contacts = companies, contacts
	fixture.binding.AccountFieldID = 9
	source := httptest.NewUnstartedServer(companycrm.Router(&companycrm.Service{Resolver: fixture, CRM: fixture}))
	source.TLS, err = PrivateTLS(filepath.Join(certs, "core/tls.crt"), filepath.Join(certs, "core/tls.key"), filepath.Join(certs, "core/ca.crt"))
	mustManager(t, err)
	source.StartTLS()
	defer source.Close()
	origin := strings.Replace(source.URL, "127.0.0.1", "localhost", 1)
	process := startManagerProcess(t, "PLATFORM_WORKERS=2", "PLATFORM_COMPANY_CORE_ORIGIN="+origin, "PLATFORM_COMPANY_CORE_CA_FILE="+filepath.Join(certs, "platform-company-crm/ca.crt"), "PLATFORM_COMPANY_CORE_CERT_FILE="+filepath.Join(certs, "platform-company-crm/tls.crt"), "PLATFORM_COMPANY_CORE_KEY_FILE="+filepath.Join(certs, "platform-company-crm/tls.key"), "PLATFORM_COMPANY_SOURCE_ACCOUNT_ID=12", "PLATFORM_COMPANY_BINDING_REVISION=core-boundary")
	client := tlsClient(t, certs, "manager-bff")
	reader := managerTestActor{uuid.New(), 1, []string{"clients.read", "catalog.manage"}}
	companyWait(t, func() bool {
		r := managerCall(client, "GET", process.private+managerPath+"companies", reader, managerTestCommand{}, "")
		return r.status == 200 && bytes.Contains(r.body, []byte("Actual Core boundary"))
	})
	companyWait(t, func() bool {
		r := managerCall(client, "GET", process.private+managerPath+"company-workflow-metadata", reader, managerTestCommand{}, "")
		return r.status == 200 && bytes.Contains(r.body, []byte("Core stage"))
	})
	var companyCount, contactCount int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.company_rows`).Scan(&companyCount))
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_company.contact_rows`).Scan(&contactCount))
	if companyCount != companies || contactCount != contacts {
		t.Fatalf("published catalog companies=%d contacts=%d", companyCount, contactCount)
	}
	// The same server rejects the manager certificate on the private source route.
	request, err := http.NewRequestWithContext(ctx, "GET", origin+"/v1/company-crm/clients", nil)
	mustManager(t, err)
	response, err := client.Do(request)
	mustManager(t, err)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("Core role boundary bypassed")
	}
}

func TestPlatformCompanyExpiredLease(t *testing.T) {
	requireCompanyProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	var activation companyActivation
	mustManager(t, jobs.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source(12,'r1')`).Scan(&activation.Scope, &activation.Epoch))
	kind := companyRefreshKind(activation)
	id := uuid.New()
	payload, _ := json.Marshal(companyJobPayload{activation, companyClients})
	_, err = owner.Exec(ctx, `INSERT INTO platform_jobs.jobs(id,kind,payload) VALUES($1,$2,$3)`, id, kind, payload)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET current_job_id=$2 WHERE scope_id=$1 AND component='clients'`, activation.Scope, id)
	mustManager(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	registry := prometheus.NewRegistry()
	cfg := queue.DefaultConfig(1)
	cfg.PollInterval = 20 * time.Millisecond
	cfg.SweepInterval = time.Hour
	handler := func(context.Context, queue.Job) queue.Completion {
		close(entered)
		<-release
		return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
			return publishCompanyObservation(ctx, tx, companyJobPayload{activation, companyClients}, id, companyObservation{code: "source_rate_limited"})
		}}
	}
	worker := queue.NewWorker(queue.NewStore(jobs), map[queue.Kind]queue.Handler{kind: handler}, cfg, registry)
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(workerCtx) }()
	defer func() { stop(); <-done }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	companyWait(t, func() bool {
		families, e := registry.Gather()
		if e != nil {
			return false
		}
		for _, family := range families {
			if family.GetName() == "platform_queue_snapshot_ok" && len(family.Metric) == 1 && family.Metric[0].GetGauge().GetValue() == 1 {
				return true
			}
		}
		return false
	})
	_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	mustManager(t, err)
	close(release)
	companyWait(t, func() bool {
		families, e := registry.Gather()
		if e != nil {
			return false
		}
		for _, family := range families {
			if family.GetName() != "platform_queue_outcomes_total" {
				continue
			}
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetValue() == "lease_lost" && metric.GetCounter().GetValue() > 0 {
						return true
					}
				}
			}
		}
		return false
	})
	var retries *time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT retry_not_before FROM platform_company.source_control`).Scan(&retries))
	if retries != nil {
		t.Fatal("expired publisher wrote cooldown")
	}
	var attempted *time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT attempt_at FROM platform_company.source_components WHERE scope_id=$1 AND component='clients'`, activation.Scope).Scan(&attempted))
	if attempted != nil {
		t.Fatal("expired publisher wrote failure")
	}
}
