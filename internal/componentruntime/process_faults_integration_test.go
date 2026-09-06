package componentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
)

// Fault gates exist only in this test process. Production executables and their
// owner stores are unchanged; faults interrupt real OS processes and sockets.
func TestComponentOSProcessFaults(t *testing.T) {
	adminURL := os.Getenv("COMPONENT_PROCESS_TEST_ADMIN_URL")
	if adminURL == "" || os.Getenv("COMPONENT_PROCESS_TEST_ALLOWED") != "true" {
		t.Skip("requires explicitly authorized disposable process cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	f := newFaultTopology(t, ctx, adminURL)

	// Process A owns a blocked first page. Process B takes its expired lease and
	// runs its own scheduler. A's late reply contains a sentinel never seen by B.
	f.mode("stale", 300, 1)
	f.seed(f.a, true)
	gatewayProcess := f.gateway()
	defer func() { gatewayProcess.stop() }()
	a := f.events("a", processAddress(t))
	defer func() { a.stop() }()
	f.reached("stale")
	var oldToken int64
	mustProcess(t, f.eventsOwner.QueryRow(ctx, `SELECT lease_token FROM event_sources WHERE installation_id=$1`, f.a.InstallationID).Scan(&oldToken))
	b := f.events("b", processAddress(t))
	defer func() { b.stop() }()
	if a.cmd.Process.Pid == b.cmd.Process.Pid {
		t.Fatal("expected different OS worker processes")
	}
	_, err := f.eventsOwner.Exec(ctx, `UPDATE event_sources SET lease_token=lease_token+1,lease_until=now()-interval '1 second' WHERE installation_id=$1`, f.a.InstallationID)
	mustProcess(t, err)
	f.seed(f.b, false) // both live OS schedulers see this separately due source
	waitProcess(t, ctx, 8*time.Second, func() bool { snapshot := f.snapshot(f.a); return snapshot.Count >= 200 && snapshot.Token > oldToken })

	// Allow both OS scheduler ticks before freezing the newer executor. With B
	// stopped, a subsequent HTTP request can only come from A's single worker,
	// proving it received/processed its stale reply rather than being canceled.
	time.Sleep(1100 * time.Millisecond)
	var stale, jobsForB int
	mustProcess(t, f.eventsOwner.QueryRow(ctx, `SELECT count(*) FROM event_jobs WHERE installation_id=$1`, f.b.InstallationID).Scan(&jobsForB))
	if jobsForB != 1 {
		t.Fatalf("two OS schedulers produced%d jobs", jobsForB)
	}
	b.stop()
	_, err = f.eventsOwner.Exec(ctx, `UPDATE event_sources SET next_poll_at=now() WHERE installation_id=$1`, f.b.InstallationID)
	mustProcess(t, err)
	f.release("stale")
	waitProcess(t, ctx, 8*time.Second, func() bool {
		var replied int64
		for _, r := range f.requests() {
			if r.Stale {
				replied = r.At
			}
			if replied > 0 && r.Mode == "stale" && r.Stage == "start" && r.At > replied {
				return true
			}
		}
		return false
	})
	f.complete(f.a, 300)
	waitProcess(t, ctx, 10*time.Second, func() bool { return f.snapshot(f.b).Count == 300 })
	mustProcess(t, f.eventsOwner.QueryRow(ctx, `SELECT count(*) FROM crm_events WHERE event_id='stale-forbidden'`).Scan(&stale))
	if stale != 0 {
		t.Fatalf("late fenced executor wrote%d stale rows", stale)
	}
	t.Logf("FAULT1 PASS: distinct Events PIDs %d/%d, stolen lease token>%d; second process advanced while first blocked; stale sentinel absent; two schedulers produced one job for due source", a.cmd.Process.Pid, b.cmd.Process.Pid, oldToken)
	a.stop()
	b.stop()
	f.reset()

	// Kill the actual executable after page1 has committed, with page2 held on
	// the wire. Restart waits for the real30s lease, not a test-shortened lease.
	f.mode("crash", 500, 2)
	f.seed(f.a, true)
	a = f.events("crash-before", processAddress(t))
	f.reached("crash")
	before := f.snapshot(f.a)
	if before.Count != 100 || before.Page != 2 || before.Pass != 1 {
		t.Fatalf("wrong durable crash checkpoint=%+v", before)
	}
	killedAt := time.Now()
	killFaultProcess(t, a)
	f.release("crash")
	after := f.snapshot(f.a)
	if after.Count != 100 || after.Page != 2 || after.From != before.From || after.To != before.To {
		t.Fatalf("SIGKILL altered committed checkpoint: %+v", after)
	}
	restartedAt := time.Now().UnixNano()
	a = f.events("crash-restarted", processAddress(t))
	f.complete(f.a, 500)
	requests := f.requests()
	var resumed *faultRequest
	for i := range requests {
		r := &requests[i]
		if r.Mode == "crash" && r.Stage == "start" && r.At >= restartedAt {
			resumed = r
			break
		}
	}
	if resumed == nil || resumed.Page != 2 || resumed.From != before.From || resumed.To != before.To {
		t.Fatalf("restart did not resume fixed page2/window: %+v", resumed)
	}
	recovered := f.snapshot(f.a)
	if recovered.Processed != 1000 || recovered.Through != before.To {
		t.Fatalf("incorrect complete recovery=%+v", recovered)
	}
	if time.Since(killedAt) < 20*time.Second {
		t.Fatal("restart did not exercise durable lease expiry")
	}
	t.Logf("FAULT2 PASS: SIGKILL after100committed rows/page2; restarted PID%d resumed exact [%d,%d] page2 after actual lease expiry;500unique/1000processed, elapsed=%s", a.cmd.Process.Pid, before.From, before.To, time.Since(killedAt))
	a.stop()
	f.reset()

	// The fixture wraps the REAL receiver: commit, signal parent, then hold the
	// reply. SIGKILL tears down the real gRPC socket before any response is sent.
	f.mode("reply", 300, 0)
	receiverAddr := processAddress(t)
	commandID := uuid.NewString()
	signalFile := filepath.Join(f.dir, "receiver-committed")
	receiver := f.receiver(receiverAddr, commandID, signalFile)
	core := f.client(receiverAddr, "core")
	auth := f.auth(f.a)
	command := serviceapi.Command{Auth: auth, CommandID: commandID, Kind: "enable", InitialDays: 1, RetentionDays: 7}
	reply := make(chan error, 1)
	go func() { _, err := core.CRMEvents.Apply(ctx, command); reply <- err }()
	waitProcess(t, ctx, 8*time.Second, func() bool { _, err := os.Stat(signalFile); return err == nil })
	var committed serviceapi.Operation
	raw, err := os.ReadFile(signalFile)
	mustProcess(t, err)
	mustProcess(t, json.Unmarshal(raw, &committed))
	f.assertCommandRows(commandID, 1, 1, 1)
	killFaultProcess(t, receiver)
	select {
	case err := <-reply:
		if err == nil {
			t.Fatal("client received success before interrupted reply")
		}
		t.Logf("actual interrupted receiver RPC returned %s", serviceapi.ErrorCode(err))
	case <-time.After(3 * time.Second):
		t.Fatal("broken socket did not fail caller")
	}
	_ = core.Close()
	a = f.events("receiver-restarted", receiverAddr)
	core = f.client(receiverAddr, "core")
	command.Auth = f.auth(f.a)
	replay, err := core.CRMEvents.Apply(ctx, command)
	mustProcess(t, err)
	if replay.ID != committed.ID || replay.CommandID != commandID {
		t.Fatalf("replay changed committed identity: first=%+v repeat=%+v", committed, replay)
	}
	f.assertCommandRows(commandID, 1, 1, 1)
	command.InitialDays = 2
	if _, err := core.CRMEvents.Apply(ctx, command); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed replay=%v", err)
	}
	t.Logf("FAULT3 PASS: receiver committed then was SIGKILLed before RPC reply; actual executable restart returned same operation%s; one inbox/operation/job, changed payload conflicts", committed.ID)
	_ = core.Close()
	a.stop()
	f.reset()

	// Stop the actual Gateway/policy process during page2. A bounded retry may
	// change error state, but no page/progress can commit while policy is absent.
	f.mode("outage", 1000, 2)
	f.seed(f.a, true)
	a = f.events("gateway-outage", processAddress(t))
	f.reached("outage")
	before = f.snapshot(f.a)
	killFaultProcess(t, gatewayProcess)
	waitProcess(t, ctx, 5*time.Second, func() bool { return f.snapshot(f.a).State == "retry" })
	time.Sleep(3 * time.Second)
	after = f.snapshot(f.a)
	if after.Count != before.Count || after.Page != before.Page || after.Through != before.Through {
		t.Fatalf("Gateway absence advanced data: before=%+v after=%+v", before, after)
	}
	gatewayProcess = f.gateway()
	waitProcess(t, ctx, 10*time.Second, func() bool { return f.snapshot(f.a).Count >= 300 })
	disabledAt := time.Now()
	_, err = f.coreOwner.Exec(ctx, `UPDATE activity_pilots SET enabled=false WHERE installation_id=$1`, f.a.InstallationID)
	mustProcess(t, err)
	waitProcess(t, ctx, 15*time.Second, func() bool { return f.snapshot(f.a).State == "paused" })
	pausedAt := time.Now()
	paused := f.snapshot(f.a)
	calls := len(f.requests())
	for time.Now().Before(disabledAt.Add(15 * time.Second)) {
		time.Sleep(150 * time.Millisecond)
		now := f.snapshot(f.a)
		if now.Count != paused.Count || now.Page != paused.Page || now.Processed != paused.Processed || now.Through != paused.Through || len(f.requests()) != calls {
			t.Fatalf("work continued after measured pilot pause: paused=%+v now=%+v", paused, now)
		}
	}
	_, err = f.coreOwner.Exec(ctx, `UPDATE activity_pilots SET enabled=true WHERE installation_id=$1`, f.a.InstallationID)
	mustProcess(t, err)
	core = f.client(aFaultAddress(a), "core") // address is attached by the topology
	_, err = core.CRMEvents.Apply(ctx, serviceapi.Command{Auth: f.auth(f.a), CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 7})
	mustProcess(t, err)
	_ = core.Close()
	f.complete(f.a, 1000)
	t.Logf("FAULT4 PASS: actual Gateway/policy SIGKILL stopped page progress; restart recovered bounded retry; pilot disable paused in%s with no calls/data/progress until15s cutoff; explicit sync resumed to1000unique", pausedAt.Sub(disabledAt))
	a.stop()
	gatewayProcess.stop()
}

type faultTopology struct {
	t                                                        *testing.T
	ctx                                                      context.Context
	dir, binary, identities, gatewayAddr, coreDSN, eventsDSN string
	coreOwner, eventsOwner                                   *pgxpool.Pool
	a, b                                                     processTenant
	from, to                                                 int64
}

func newFaultTopology(t *testing.T, ctx context.Context, adminURL string) *faultTopology {
	t.Helper()
	root, err := filepath.Abs("../..")
	mustProcess(t, err)
	dir := t.TempDir()
	f := &faultTopology{t: t, ctx: ctx, dir: dir, identities: filepath.Join(dir, "identities"), gatewayAddr: processAddress(t), from: time.Now().Add(-5 * time.Minute).Unix(), to: time.Now().Add(-3 * time.Minute).Unix()}
	for _, name := range []string{"crm-events", "service-certs"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-race", "-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build%s: %v %s", name, err, output)
		}
	}
	f.binary = filepath.Join(dir, "crm-events")
	if output, err := exec.CommandContext(ctx, filepath.Join(dir, "service-certs"), f.identities).CombinedOutput(); err != nil {
		t.Fatalf("certs %v %s", err, output)
	}
	admin, err := pgxpool.New(ctx, adminURL)
	mustProcess(t, err)
	defer admin.Close()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, role := range []string{"core", "events"} {
		db := "activity_fault_" + suffix + "_" + role + "_test"
		quoted := pgx.Identifier{db}.Sanitize()
		_, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+role+"_owner")
		mustProcess(t, err)
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p, err := pgxpool.New(c, adminURL)
			if err == nil {
				defer p.Close()
				_, _ = p.Exec(c, "DROP DATABASE "+quoted+" WITH(FORCE)")
			}
		})
		_, err = admin.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC;GRANT CONNECT ON DATABASE "+quoted+" TO "+role+"_runtime")
		mustProcess(t, err)
		owner, err := pgxpool.New(ctx, processDSN(t, adminURL, db, role+"_owner"))
		mustProcess(t, err)
		t.Cleanup(owner.Close)
		migrationDir := filepath.Join(root, "migrations")
		if role == "events" {
			migrationDir = filepath.Join(migrationDir, "crmevents")
		}
		mustProcess(t, migrations.New(owner, migrationDir).Up(ctx))
		_, err = owner.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA public TO "+role+"_runtime;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO "+role+"_runtime;GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO "+role+"_runtime")
		mustProcess(t, err)
		if role == "core" {
			f.coreOwner = owner
			f.coreDSN = processDSN(t, adminURL, db, "core_runtime")
		} else {
			f.eventsOwner = owner
			f.eventsDSN = processDSN(t, adminURL, db, "events_runtime")
		}
	}
	ring, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x31}, 32)}, 1)
	mustProcess(t, err)
	f.a = seedProcessTenant(t, ctx, f.coreOwner, ring, true)
	f.b = seedProcessTenant(t, ctx, f.coreOwner, ring, true)
	return f
}
func (f *faultTopology) mode(name string, count, page int) {
	data, _ := json.Marshal(faultMode{Name: name, Count: count, GatePage: page})
	mustProcess(f.t, os.WriteFile(filepath.Join(f.dir, "mode-next"), data, 0600))
	mustProcess(f.t, os.Rename(filepath.Join(f.dir, "mode-next"), filepath.Join(f.dir, "mode")))
}
func (f *faultTopology) reached(mode string) {
	waitProcess(f.t, f.ctx, 8*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.dir, mode+".reached")); return err == nil })
}
func (f *faultTopology) release(mode string) {
	mustProcess(f.t, os.WriteFile(filepath.Join(f.dir, mode+".release"), []byte("release"), 0600))
}
func (f *faultTopology) gateway() *processChild {
	exe, err := os.Executable()
	mustProcess(f.t, err)
	tenants, _ := json.Marshal(map[string]string{f.a.InstallationID.String(): f.a.IntegrationID.String(), f.b.InstallationID.String(): f.b.IntegrationID.String()})
	p := startProcess(f.t, f.dir, "fault-gateway", exe, []string{"-test.run=^TestFaultGatewayFixture$", "-test.v"}, map[string]string{"FAULT_GATEWAY": "1", "ACTIVITY_MODE": "grpc", "DATABASE_URL": f.coreDSN, "SERVICE_RPC_ADDRESS": f.gatewayAddr, "SERVICE_IDENTITY_DIR": filepath.Join(f.identities, "gateway"), "FAULT_DIR": f.dir, "FAULT_EVENT_AT": strconv.FormatInt(f.from+30, 10), "FIXTURE_TENANTS": string(tenants)})
	waitProcess(f.t, f.ctx, 8*time.Second, func() bool {
		c, err := f.dial(f.gatewayAddr, "core")
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Ready(f.ctx) == nil
	})
	return p
}

var faultAddresses sync.Map

func aFaultAddress(p *processChild) string {
	address, _ := faultAddresses.Load(p)
	return address.(string)
}
func (f *faultTopology) events(name, address string) *processChild {
	health := processAddress(f.t)
	p := startProcess(f.t, f.dir, "fault-events-"+name, f.binary, nil, map[string]string{"ACTIVITY_MODE": "grpc", "GATEWAY_ADDRESS": f.gatewayAddr, "CRM_EVENTS_DATABASE_URL": f.eventsDSN, "CRM_EVENTS_WORKERS": "1", "CRM_EVENTS_POLL_INTERVAL": "1h", "SERVICE_IDENTITY_DIR": filepath.Join(f.identities, "crm-events"), "SERVICE_RPC_ADDRESS": address, "SERVICE_HEALTH_ADDRESS": health})
	faultAddresses.Store(p, address)
	waitHTTPProcess(f.t, f.ctx, "http://"+health+"/ready")
	return p
}
func (f *faultTopology) receiver(address, command, signal string) *processChild {
	exe, err := os.Executable()
	mustProcess(f.t, err)
	p := startProcess(f.t, f.dir, "fault-receiver", exe, []string{"-test.run=^TestFaultReceiverFixture$", "-test.v"}, map[string]string{"FAULT_RECEIVER": "1", "ACTIVITY_MODE": "grpc", "GATEWAY_ADDRESS": f.gatewayAddr, "CRM_EVENTS_DATABASE_URL": f.eventsDSN, "SERVICE_IDENTITY_DIR": filepath.Join(f.identities, "crm-events"), "SERVICE_RPC_ADDRESS": address, "FAULT_COMMAND": command, "FAULT_COMMIT_FILE": signal})
	waitProcess(f.t, f.ctx, 8*time.Second, func() bool {
		c, err := f.dial(address, "core")
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Ready(f.ctx) == nil
	})
	return p
}
func (f *faultTopology) dial(address, identity string) (*servicerpc.Clients, error) {
	tls, err := servicerpc.TLSFromFiles(filepath.Join(f.identities, identity, "ca.crt"), filepath.Join(f.identities, identity, "tls.crt"), filepath.Join(f.identities, identity, "tls.key"), "localhost", false)
	if err != nil {
		return nil, err
	}
	return servicerpc.Dial(f.ctx, address, tls)
}
func (f *faultTopology) client(address, identity string) *servicerpc.Clients {
	c, err := f.dial(address, identity)
	mustProcess(f.t, err)
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}
func (f *faultTopology) auth(tenant processTenant) serviceapi.Auth {
	c := f.client(f.gatewayAddr, "core")
	defer c.Close()
	a, err := c.Policy.Issue(f.ctx, serviceapi.IssueRequest{Scope: tenant.Scope, ActorID: 7, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrants()})
	mustProcess(f.t, err)
	return a
}
func (f *faultTopology) reset() {
	_, err := f.eventsOwner.Exec(f.ctx, `TRUNCATE event_sources CASCADE`)
	mustProcess(f.t, err)
}
func (f *faultTopology) seed(tenant processTenant, job bool) {
	tx, err := f.eventsOwner.Begin(f.ctx)
	mustProcess(f.t, err)
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, `INSERT INTO event_sources(installation_id,integration_id,next_poll_at)VALUES($1,$2,now()+interval '1hour')`, tenant.InstallationID, tenant.IntegrationID)
	mustProcess(f.t, err)
	_, err = tx.Exec(f.ctx, `INSERT INTO event_consumers(installation_id,consumer,enabled)VALUES($1,'activity',true)`, tenant.InstallationID)
	mustProcess(f.t, err)
	if job {
		operation := uuid.New()
		id := uuid.New()
		_, err = tx.Exec(f.ctx, `INSERT INTO event_operations(id,installation_id,actor_id,kind,status)VALUES($1,$2,7,'sync','accepted')`, operation, tenant.InstallationID)
		mustProcess(f.t, err)
		_, err = tx.Exec(f.ctx, `INSERT INTO event_jobs(id,installation_id,operation_id,kind,priority,window_from,window_to,target_to)VALUES($1,$2,$3,'current',10,to_timestamp($4),to_timestamp($5),to_timestamp($5))`, id, tenant.InstallationID, operation, f.from, f.to)
		mustProcess(f.t, err)
		_, err = tx.Exec(f.ctx, `INSERT INTO event_operation_jobs(operation_id,job_id)VALUES($1,$2)`, operation, id)
		mustProcess(f.t, err)
	} else {
		_, err = tx.Exec(f.ctx, `UPDATE event_sources SET continuous_from=to_timestamp($2),continuous_to=to_timestamp($2)+interval '1minute',next_poll_at=now() WHERE installation_id=$1`, tenant.InstallationID, f.from)
		mustProcess(f.t, err)
	}
	mustProcess(f.t, tx.Commit(f.ctx))
}

type faultSnapshot struct {
	Count                               int
	Page, Pass                          int
	Token, From, To, Through, Processed int64
	State                               string
}

func (f *faultTopology) snapshot(tenant processTenant) faultSnapshot {
	var s faultSnapshot
	err := f.eventsOwner.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM crm_events WHERE installation_id=s.installation_id),j.page,j.scan_pass,s.lease_token,extract(epoch FROM j.window_from)::bigint,extract(epoch FROM j.window_to)::bigint,coalesce(extract(epoch FROM s.continuous_to)::bigint,0),j.processed,s.state FROM event_sources s JOIN event_jobs j USING(installation_id) WHERE s.installation_id=$1 ORDER BY j.created_at LIMIT 1`, tenant.InstallationID).Scan(&s.Count, &s.Page, &s.Pass, &s.Token, &s.From, &s.To, &s.Through, &s.Processed, &s.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return s
	}
	mustProcess(f.t, err)
	return s
}
func (f *faultTopology) complete(tenant processTenant, count int) {
	waitProcess(f.t, f.ctx, 45*time.Second, func() bool {
		snapshot := f.snapshot(tenant)
		return snapshot.Count == count && snapshot.State == "idle" && snapshot.Through >= f.to
	})
}
func (f *faultTopology) assertCommandRows(id string, inbox, operation, jobs int) {
	var i, o, j int
	err := f.eventsOwner.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM event_inbox WHERE command_id=$1),(SELECT count(*) FROM event_operations WHERE id=$1::uuid),(SELECT count(*) FROM event_jobs WHERE operation_id=$1::uuid)`, id).Scan(&i, &o, &j)
	mustProcess(f.t, err)
	if i != inbox || o != operation || j != jobs {
		f.t.Fatalf("durable command rows %d/%d/%d", i, o, j)
	}
}
func killFaultProcess(t *testing.T, p *processChild) {
	t.Helper()
	p.once.Do(func() {
		mustProcess(t, p.cmd.Process.Kill())
		select {
		case err := <-p.done:
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				t.Fatalf("expected real SIGKILL exit, got%v", err)
			}
			status, ok := exited.Sys().(syscall.WaitStatus)
			if !ok || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong process termination: %v", err)
			}
			t.Logf("sent and observed SIGKILL for PID%d", p.cmd.Process.Pid)
		case <-time.After(3 * time.Second):
			t.Fatal("SIGKILL did not terminate process")
		}
	})
}

type faultMode struct {
	Name            string
	Count, GatePage int
}
type faultRequest struct {
	Stale        bool
	Mode, Stage  string
	Page         int
	From, To, At int64
}
type faultHTTP struct {
	dir     string
	eventAt int64
	mu      sync.Mutex
	log     *os.File
}

func (h *faultHTTP) record(r faultRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = json.NewEncoder(h.log).Encode(r)
}
func (h *faultHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/v4/events" {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":7,"rights":{"is_admin":true,"is_active":true}}`)), Request: req}, nil
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "mode"))
	if err != nil {
		return nil, err
	}
	var mode faultMode
	if err := json.Unmarshal(raw, &mode); err != nil {
		return nil, err
	}
	page, _ := strconv.Atoi(req.URL.Query().Get("page"))
	from, _ := strconv.ParseInt(req.URL.Query().Get("filter[created_at][from]"), 10, 64)
	to, _ := strconv.ParseInt(req.URL.Query().Get("filter[created_at][to]"), 10, 64)
	record := faultRequest{Mode: mode.Name, Stage: "start", Page: page, From: from, To: to, At: time.Now().UnixNano()}
	h.record(record)
	stale := false
	if page == mode.GatePage {
		claimed, err := os.OpenFile(filepath.Join(h.dir, mode.Name+".claimed"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = claimed.Close()
			signal, _ := json.Marshal(record)
			_ = os.WriteFile(filepath.Join(h.dir, mode.Name+".reached"), signal, 0600)
			for {
				if _, err := os.Stat(filepath.Join(h.dir, mode.Name+".release")); err == nil {
					break
				}
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
			stale = mode.Name == "stale"
		}
	}
	events := []serviceapi.Event{}
	if h.eventAt >= from && h.eventAt <= to {
		for i := (page - 1) * 100; i < page*100 && i < mode.Count; i++ {
			events = append(events, serviceapi.Event{ID: fmt.Sprintf("event-%05d", i), CreatedAt: h.eventAt, CreatedBy: 7, Type: "lead_added", EntityID: 42, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)})
		}
	}
	if stale {
		events = []serviceapi.Event{{ID: "stale-forbidden", CreatedAt: h.eventAt, CreatedBy: 7, Type: "lead_added", EntityID: 42, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)}}
	}
	result := map[string]any{"_embedded": map[string]any{"events": events}}
	if page*100 < mode.Count && len(events) > 0 {
		result["_links"] = map[string]any{"next": map[string]string{"href": "https://fixture.amocrm.ru/api/v4/events?page=" + strconv.Itoa(page+1)}}
	}
	body, _ := json.Marshal(result)
	record.Stale = stale
	record.Stage = "response"
	record.At = time.Now().UnixNano()
	h.record(record)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}
func (f *faultTopology) requests() []faultRequest {
	data, err := os.ReadFile(filepath.Join(f.dir, "requests.jsonl"))
	mustProcess(f.t, err)
	var result []faultRequest
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r faultRequest
		if json.Unmarshal(line, &r) == nil {
			result = append(result, r)
		}
	}
	return result
}

func TestFaultGatewayFixture(t *testing.T) {
	if os.Getenv("FAULT_GATEWAY") != "1" {
		t.Skip("OS process fixture")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := Load("worker")
	mustProcess(t, err)
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	mustProcess(t, err)
	defer pool.Close()
	var tenants map[string]string
	mustProcess(t, json.Unmarshal([]byte(os.Getenv("FIXTURE_TENANTS")), &tenants))
	at, err := strconv.ParseInt(os.Getenv("FAULT_EVENT_AT"), 10, 64)
	mustProcess(t, err)
	dir := os.Getenv("FAULT_DIR")
	log, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	mustProcess(t, err)
	defer log.Close()
	client := amocrm.NewClient(&http.Client{Transport: &faultHTTP{dir: dir, eventAt: at, log: log}, Timeout: 10 * time.Second}, fixtureTokens{tenants})
	graph, err := StartGateway(ctx, cfg, pool, client, prometheus.NewRegistry())
	mustProcess(t, err)
	defer graph.Close()
	select {
	case <-ctx.Done():
	case <-graph.Failed():
		t.Fatal(graph.Err())
	}
}

type commitThenHold struct {
	serviceapi.CRMEvents
	command, file string
}

func (s commitThenHold) Apply(ctx context.Context, command serviceapi.Command) (serviceapi.Operation, error) {
	result, err := s.CRMEvents.Apply(ctx, command)
	if err != nil {
		return result, err
	}
	if command.CommandID == s.command {
		encoded, _ := json.Marshal(result)
		if err := os.WriteFile(s.file, encoded, 0600); err != nil {
			return serviceapi.Operation{}, err
		}
		<-ctx.Done()
		return serviceapi.Operation{}, ctx.Err()
	}
	return result, nil
}
func TestFaultReceiverFixture(t *testing.T) {
	if os.Getenv("FAULT_RECEIVER") != "1" {
		t.Skip("OS process fixture")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := Load("crm-events")
	mustProcess(t, err)
	g := newGraph(ctx)
	defer g.Close()
	client, err := g.dial(cfg.GatewayAddress, cfg.IdentityDir)
	mustProcess(t, err)
	pool, err := pgxpool.New(ctx, cfg.EventsDSN)
	mustProcess(t, err)
	defer pool.Close()
	service := crmevents.New(pool, client.Policy, client.Gateway, crmevents.DefaultConfig())
	receiver := commitThenHold{CRMEvents: service, command: os.Getenv("FAULT_COMMAND"), file: os.Getenv("FAULT_COMMIT_FILE")}
	mustProcess(t, g.serve(cfg.RPCAddress, cfg.IdentityDir, &servicerpc.Endpoints{CRMEvents: receiver, Ready: pool.Ping}))
	select {
	case <-ctx.Done():
	case <-g.Failed():
		t.Fatal(g.Err())
	}
}
