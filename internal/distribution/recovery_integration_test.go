package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"net/http/httptest"
	"testing"
	"time"
)

type scanFake struct {
	Lead      amocrm.LeadState
	IDs       []int64
	Next      bool
	PageCalls int
	Calls     map[int64]int
	FailID    int64
	Hook      func()
}

func (c *scanFake) GetLeadSnapshot(_ context.Context, _ uuid.UUID, id int64) (amocrm.LeadState, error) {
	c.Calls[id]++
	if c.Hook != nil {
		c.Hook()
		c.Hook = nil
	}
	if id == c.FailID {
		return amocrm.LeadState{}, errors.New("temporary read")
	}
	l := c.Lead
	l.ID = id
	return l, nil
}
func (c *scanFake) DistributionLeadPage(context.Context, uuid.UUID, time.Time, time.Time, int) (amocrm.LeadScanPage, error) {
	c.PageCalls++
	return amocrm.LeadScanPage{IDs: c.IDs, HasNext: c.Next}, nil
}
func seedScan(t *testing.T, f *assignmentFixtureData, maxPages int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	scope, _ := json.Marshal(f.Assignment.Scope)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, e := f.Pool.Exec(ctx, `INSERT INTO distribution_recovery_scans(id,binding_id,scope,window_from,window_to,max_pages) VALUES($1,$2,$3,$4,$5,$6)`, id, f.Assignment.Scope.BindingID, scope, now.Add(-time.Minute), now, maxPages); e != nil {
		t.Fatal(e)
	}
	return id
}
func TestRecoveryFrozenPageCursorRetriesAndBoundedTick(t *testing.T) {
	f := assignmentFixture(t)
	id := seedScan(t, f, 1)
	ids := []int64{}
	for i := int64(1); i <= 12; i++ {
		ids = append(ids, i)
	}
	crm := &scanFake{Lead: f.CRM.Lead, IDs: ids, Calls: map[int64]int{}, FailID: 2}
	worker := RecoveryWorker{Store: f.Store, CRM: crm}
	ctx := context.Background()
	if e := worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	var cursor int
	var state string
	if e := f.Pool.QueryRow(ctx, `SELECT item_cursor,state FROM distribution_recovery_scans WHERE id=$1`, id).Scan(&cursor, &state); e != nil {
		t.Fatal(e)
	}
	if cursor != 1 || state != "pending" {
		t.Fatal("lost partial progress", cursor, state)
	}
	crm.FailID = 0
	_, _ = f.Pool.Exec(ctx, `UPDATE distribution_recovery_scans SET next_attempt_at=clock_timestamp() WHERE id=$1`, id)
	if e := worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	_ = f.Pool.QueryRow(ctx, `SELECT item_cursor,state FROM distribution_recovery_scans WHERE id=$1`, id).Scan(&cursor, &state)
	if cursor != 11 || state != "pending" || crm.PageCalls != 1 || crm.Calls[1] != 1 {
		t.Fatal("quota spent rereading persisted items", cursor, state, crm.PageCalls, crm.Calls)
	}
	if e := worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM distribution_recovery_scans WHERE id=$1`, id).Scan(&state)
	if state != "completed" {
		t.Fatal(state)
	}
	var count int
	_ = f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_event_outbox WHERE recovery_scan_id=$1`, id).Scan(&count)
	if count != 12 {
		t.Fatal("lost or repeated recovery envelope", count)
	}
	rows, e := f.Pool.Query(ctx, `SELECT payload FROM distribution_event_outbox WHERE recovery_scan_id=$1`, id)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		validateRuntimeResponse(t, "EventEnvelope", raw)
		var env EventEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.Event.Kind != "lead.snapshot_reconciled" || env.Event.EntryFingerprint != nil || env.Event.Before != nil {
			t.Fatal("invented recovery entry", env)
		}
	}
}
func TestRecoveryScopeRevocationBeforeCommitAndVisiblePageGap(t *testing.T) {
	t.Run("revocation", func(t *testing.T) {
		f := assignmentFixture(t)
		seedScan(t, f, 1)
		crm := &scanFake{Lead: f.CRM.Lead, IDs: []int64{10}, Calls: map[int64]int{}, Hook: func() {
			_, _ = f.Pool.Exec(context.Background(), `UPDATE distribution_bindings SET state='revoked',revoked_at=clock_timestamp()`)
		}}
		if e := (&RecoveryWorker{Store: f.Store, CRM: crm}).Tick(context.Background()); e == nil {
			t.Fatal("revoked scope committed")
		}
		var n int
		_ = f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM distribution_event_outbox`).Scan(&n)
		if n != 0 {
			t.Fatal("stale outbox", n)
		}
	})
	t.Run("page limit", func(t *testing.T) {
		f := assignmentFixture(t)
		id := seedScan(t, f, 1)
		crm := &scanFake{Lead: f.CRM.Lead, IDs: []int64{10}, Next: true, Calls: map[int64]int{}}
		if e := (&RecoveryWorker{Store: f.Store, CRM: crm}).Tick(context.Background()); e != nil {
			t.Fatal(e)
		}
		var state, gap string
		_ = f.Pool.QueryRow(context.Background(), `SELECT state,gap_reason FROM distribution_recovery_scans WHERE id=$1`, id).Scan(&state, &gap)
		if state != "blocked" || gap != "scan_page_limit" {
			t.Fatal(state, gap)
		}
	})
}

func TestSignedRecoveryAPIAndScopedReadback(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	_, _ = f.Pool.Exec(ctx, `UPDATE distribution_bindings SET created_at=clock_timestamp()-interval '1 hour'`)
	h := assignmentHTTP(t, f)
	base := "/internal/v1/distribution/bindings/" + f.Assignment.Scope.BindingID.String()
	scanID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	call := func(method, path string, body any, status int, schema string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		Sign(req, f.Scope, h.secret, raw)
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("%s %s %d %s", method, path, response.Code, response.Body.String())
		}
		if schema != "" {
			validateRuntimeResponse(t, schema, response.Body.Bytes())
		}
	}
	input := map[string]any{"scanId": scanID, "windowFrom": now.Add(-time.Minute), "windowTo": now, "maxPages": 1}
	raw, _ := json.Marshal(input)
	validateRuntimeResponse(t, "ScanInput", raw)
	call("POST", base+"/recovery-scans", input, 202, "ScanReceipt")
	call("POST", base+"/recovery-scans", input, 202, "ScanReceipt")
	input["maxPages"] = 2
	call("POST", base+"/recovery-scans", input, 409, "ErrorResponse")
	call("GET", base+"/recovery-scans/"+scanID.String(), nil, 200, "RecoveryScan")
	_, _ = f.Pool.Exec(ctx, `UPDATE distribution_recovery_scans SET state='blocked',item_cursor=3 WHERE id=$1`, scanID)
	retry := map[string]any{"expectedCursorPage": 1, "expectedItemCursor": 3}
	call("POST", base+"/recovery-scans/"+scanID.String()+"/retry", retry, 202, "ScanReceipt")
	call("POST", base+"/recovery-scans/"+scanID.String()+"/retry", retry, 409, "ErrorResponse")
	call("GET", base+"/delivery-status", nil, 200, "DeliveryStatus")
	call("GET", base+"/leads/10", nil, 200, "LeadObservation")
}

func TestRecoveryCreateDoesNotRevealForeignScanIdentity(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	foreignBinding, scan, company := uuid.New(), uuid.New(), uuid.New()
	_, e := f.Pool.Exec(ctx, `INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by,state,revoked_at) VALUES($1,$2,$3,$4,123,1,$5,1,'revoked',clock_timestamp())`, foreignBinding, company, f.Scope.InstallationID, f.Assignment.Scope.IntegrationID, uuid.New())
	if e != nil {
		t.Fatal(e)
	}
	scope := f.Assignment.Scope
	scope.BindingID = foreignBinding
	scope.CompanyID = company
	raw, _ := json.Marshal(scope)
	now := time.Now().UTC().Truncate(time.Second)
	_, e = f.Pool.Exec(ctx, `INSERT INTO distribution_recovery_scans(id,binding_id,scope,window_from,window_to) VALUES($1,$2,$3,$4,$5)`, scan, foreignBinding, raw, now.Add(-time.Minute), now)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Pool.Exec(ctx, `UPDATE distribution_bindings SET created_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.Assignment.Scope.BindingID)
	h := assignmentHTTP(t, f)
	input := map[string]any{"scanId": scan, "windowFrom": now.Add(-time.Minute), "windowTo": now, "maxPages": 1}
	raw, _ = json.Marshal(input)
	req := httptest.NewRequest("POST", "/internal/v1/distribution/bindings/"+f.Assignment.Scope.BindingID.String()+"/recovery-scans", bytes.NewReader(raw))
	Sign(req, f.Scope, h.secret, raw)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, req)
	if response.Code != 404 {
		t.Fatal("foreign scan existence leaked", response.Code, response.Body.String())
	}
}
