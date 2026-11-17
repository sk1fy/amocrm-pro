package productplatform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformManagerAcrossReplicas(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR",
		"PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(name) == "" {
			t.Skip("run make platform-test for Platform manager process checks")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	first := startManagerProcess(t)
	second := startManagerProcess(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	manager, core := tlsClient(t, certs, "manager-bff"), tlsClient(t, certs, "core")
	manager.Timeout = 7 * time.Second
	expectStatus(t, &http.Client{Timeout: time.Second}, first.public+managerPath+"installations", http.StatusNotFound)
	pilot.deliver(t, core, first.private)

	subject := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	firstPage := managerList(t, manager, first.private, subject, "")
	if firstPage.Summary != (installationSummary{Total: 3, Unreviewed: 3}) {
		t.Fatalf("summary did not count the three confirmed episodes: %+v", firstPage.Summary)
	}
	var removed, reinstalled, other managerInstallation
	for _, item := range firstPage.Items {
		switch {
		case item.Lifecycle == "removed":
			removed = item
		case item.Kind == "reinstall":
			reinstalled = item
		default:
			other = item
		}
	}
	if removed.ID == uuid.Nil || removed.LastLifecycle != "uninstalled" || removed.Authorization != "disabled" ||
		removed.RemovedAt == nil || removed.Version != 4 || !removed.Access.Active || removed.Access.Source != "own_trial" {
		t.Fatalf("closed episode lost confirmed state or pair term: %+v", removed)
	}
	if reinstalled.ID == uuid.Nil || reinstalled.ID == removed.ID || reinstalled.Account != removed.Account ||
		reinstalled.AmoAccount != "9007199254740993" || reinstalled.Generation != 2 || reinstalled.Version != 2 ||
		reinstalled.LastLifecycle != "reinstalled" || reinstalled.Authorization != "reauth_required" || reinstalled.RemovedAt != nil ||
		reinstalled.Access.ValidUntil == nil || removed.Access.ValidUntil == nil ||
		!reinstalled.Access.ValidUntil.Equal(*removed.Access.ValidUntil) || reinstalled.Access.ComputedAt != firstPage.SnapshotAt ||
		reinstalled.ProductName != "Pilot % Review" || reinstalled.Activation != "unavailable" || reinstalled.CRMWorkflow != "unsupported" {
		t.Fatalf("reinstall lost scoped lifecycle or term provenance: %+v", reinstalled)
	}
	if other.ID == uuid.Nil || !other.InstalledAt.Equal(reinstalled.InstalledAt) || other.Access.Active || other.Access.Source != "none" {
		t.Fatalf("unexpected other installation: %+v", other)
	}

	matching := managerList(t, manager, first.private, subject, "?q=%25&line=PRO&activation=unavailable&kind=reinstall&last_lifecycle_event=reinstalled&review=unreviewed&access_state=active&product_id="+reinstalled.Product.String())
	if len(matching.Items) != 1 || matching.Items[0].ID != reinstalled.ID || matching.Summary.Total != 1 {
		t.Fatalf("combined filters did not match the reinstall only: %+v", matching)
	}
	if literal := managerList(t, manager, first.private, subject, "?q=%25"); literal.Summary.Total != 2 {
		t.Fatalf("search interpreted a literal percent as a wildcard: %+v", literal)
	}
	if byCode := managerList(t, manager, first.private, subject, "?q=PILOT-REVIEW"); byCode.Summary.Total != 2 {
		t.Fatalf("search did not match product code without regard to case: %+v", byCode)
	}
	if byID := managerList(t, manager, first.private, subject, "?q=9007199254740993"); byID.Summary.Total != 2 {
		t.Fatalf("search did not match the exact large account ID: %+v", byID)
	}
	scopeDay := pilot.opened.In(pilot.zone).Format(time.DateOnly)
	datePage := managerList(t, manager, first.private, subject, "?from="+scopeDay+"&to="+scopeDay)
	if len(datePage.Items) != 2 || datePage.Summary.Total != 2 {
		t.Fatalf("business date did not include midnight and exclude preceding second: %+v", datePage)
	}
	zero := managerList(t, manager, first.private, subject, "?activation=activated")
	if zero.Items == nil || len(zero.Items) != 0 || zero.Next != nil || zero.Summary.Total != 0 {
		t.Fatalf("confirmed empty list lost its wire shape: %+v", zero)
	}

	for _, raw := range []string{"unknown=1", "limit=0", "limit=101", "limit=1&limit=2", "q=%00", "q=%ff",
		"from=2026-11-02&to=2026-11-01", "last_lifecycle_event=authorization_changed", "from=2026-11-1"} {
		managerRequest(t, manager, http.MethodGet, first.private+managerPath+"installations?"+raw,
			subject, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "validation_failed")
	}
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+"installations",
		managerTestActor{}, managerTestCommand{}, "", http.StatusUnauthorized).requireCode(t, "authentication_required")
	managerRequest(t, core, http.MethodGet, first.private+managerPath+"installations",
		subject, managerTestCommand{}, "", http.StatusForbidden).requireCode(t, "permission_denied")

	streamSort := "?limit=1&sort=installed_at"
	snapshot := managerList(t, manager, first.private, subject, streamSort)
	if len(snapshot.Items) != 1 || snapshot.Items[0].ID != removed.ID || snapshot.Next == nil {
		t.Fatalf("ascending first page did not contain the old episode and cursor: %+v", snapshot)
	}
	cursorQuery := streamSort + "&cursor=" + url.QueryEscape(*snapshot.Next)
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"installations"+cursorQuery+"A",
		subject, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"installations"+cursorQuery+"&q=pilot",
		subject, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	foreign := managerTestActor{subject: uuid.New(), version: 1, grants: subject.grants}
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"installations"+cursorQuery,
		foreign, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	newGrants := managerTestActor{subject: subject.subject, version: 2, grants: subject.grants}
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"installations"+cursorQuery,
		newGrants, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")

	original := newManagerTestCommand(reinstalled.ID, reinstalled.Version)
	originalReview := `{"reason":"confirmed","reviewed":true}`
	responses := make(chan observedManagerResponse, 2)
	for _, peer := range []string{first.private, second.private} {
		go func() {
			responses <- managerCall(manager, http.MethodPut, peer+original.reviewPath(), subject, original, originalReview)
		}()
	}
	accepted, replay := <-responses, <-responses
	accepted.requireStatus(t, http.StatusOK)
	replay.requireStatus(t, http.StatusOK)
	if !bytes.Equal(accepted.body, replay.body) || accepted.etag != `"3"` || accepted.etag != replay.etag {
		t.Fatalf("concurrent duplicates returned different outcomes: %+v %+v", accepted, replay)
	}
	reviewed := managerJSON[managerInstallation](t, accepted.body)
	if !reviewed.Review.Reviewed || reviewed.Review.By == nil || *reviewed.Review.By != subject.subject ||
		reviewed.Review.At == nil || reviewed.Version != 3 {
		t.Fatalf("review did not record a new version, actor, and time: %+v", reviewed)
	}
	eventID := pilot.event(t, original, subject, "installation_reviewed", "confirmed", 3)
	pilot.countEffects(t, 1)

	continued := managerList(t, manager, second.private, subject, cursorQuery)
	if continued.Next == nil || len(continued.Items) != 1 {
		t.Fatalf("second page has wrong size: %+v", continued)
	}
	last := managerList(t, manager, second.private, subject, streamSort+"&cursor="+url.QueryEscape(*continued.Next))
	for _, page := range []installationPage{continued, last} {
		if page.SnapshotAt != snapshot.SnapshotAt || page.Summary != snapshot.Summary || page.QueryHash != snapshot.QueryHash ||
			len(page.Items) != 1 || page.Items[0].Review.Reviewed || page.Items[0].Access.ComputedAt != snapshot.SnapshotAt {
			t.Fatalf("live review changed a frozen page: %+v", page)
		}
	}
	if last.Next != nil || strings.Compare(continued.Items[0].ID.String(), last.Items[0].ID.String()) >= 0 {
		t.Fatalf("ascending keyset was unstable at equal timestamps: %+v %+v", continued.Items, last.Items)
	}
	descending := managerList(t, manager, first.private, subject, "?limit=1")
	if descending.Next == nil || descending.Items[0].ID != last.Items[0].ID || descending.Summary.Reviewed != 1 {
		t.Fatalf("fresh descending list did not see the new review or stable tie order: %+v", descending)
	}
	descSecond := managerList(t, manager, second.private, subject, "?limit=1&sort=-installed_at&cursor="+url.QueryEscape(*descending.Next))
	if descSecond.Next == nil || descSecond.Items[0].ID != continued.Items[0].ID {
		t.Fatalf("descending keyset failed at equal timestamps: %+v", descSecond)
	}

	receipt := managerRequest(t, manager, http.MethodGet, second.private+original.receiptPath(),
		subject, managerTestCommand{}, "", http.StatusOK)
	if got := managerJSON[managerReceipt](t, receipt.body); got.ID != original.id || got.State != "completed" || got.Kind != reviewCommandKind ||
		bytes.Contains(receipt.body, []byte("operation_id")) {
		t.Fatalf("synchronous receipt has the wrong shape: %s", receipt.body)
	}
	managerRequest(t, manager, http.MethodGet, second.private+original.receiptPath(), foreign,
		managerTestCommand{}, "", http.StatusNotFound).requireCode(t, "resource_not_found")
	revoked := managerTestActor{subject: subject.subject, version: 2, grants: []string{"clients.read"}}
	managerRequest(t, manager, http.MethodGet, second.private+original.receiptPath(),
		revoked, managerTestCommand{}, "", http.StatusForbidden).requireCode(t, "permission_denied")
	managerRequest(t, manager, http.MethodPut, second.private+original.reviewPath(),
		revoked, original, originalReview, http.StatusForbidden).requireCode(t, "permission_denied")
	for _, mismatch := range []struct {
		actor managerTestActor
		cmd   managerTestCommand
		body  string
	}{
		{foreign, original, originalReview},
		{subject, managerTestCommand{id: uuid.New(), key: original.key, target: reinstalled.ID, version: 2}, originalReview},
		{subject, managerTestCommand{id: original.id, key: uuid.New(), target: reinstalled.ID, version: 2}, originalReview},
		{subject, managerTestCommand{id: original.id, key: original.key, target: other.ID, version: 2}, originalReview},
		{subject, managerTestCommand{id: original.id, key: original.key, target: reinstalled.ID, version: 3}, originalReview},
		{subject, original, `{"reviewed":true,"reason":"changed"}`},
	} {
		managerRequest(t, manager, http.MethodPut, second.private+mismatch.cmd.reviewPath(), mismatch.actor,
			mismatch.cmd, mismatch.body, http.StatusConflict).requireCode(t, "idempotency_conflict")
	}

	first.stop(t)
	restarted := startManagerProcess(t)
	same := managerRequest(t, manager, http.MethodPut, restarted.private+original.reviewPath(),
		newGrants, original, `{"reviewed": true, "reason": "confirmed"}`, http.StatusOK)
	if !bytes.Equal(same.body, accepted.body) || same.etag != accepted.etag {
		t.Fatalf("replay after restart and new session changed original outcome: %+v", same)
	}

	pilot.verifyReviewRaces(t, manager, restarted.private, second.private, reinstalled.ID, subject, accepted, original, originalReview, eventID)
	pilot.verifyReviewRollback(t, manager, second.private, reinstalled.ID, subject)
	pilot.verifyTermExpiry(t, manager, restarted.private, second.private, subject, reinstalled)
	pilot.verifyScope(t, manager, restarted.private, second.private, subject, original, originalReview, reinstalled)
	pilot.verifySnapshotCaps(t, manager, restarted.private, second.private, subject)
}

type managerPilot struct {
	ctx                                                                     context.Context
	owner                                                                   *pgxpool.Pool
	integration, otherIntegration, installation, otherInstallation, product uuid.UUID
	opened                                                                  time.Time
	zone                                                                    *time.Location
}

func (p *managerPilot) fixture(t *testing.T) {
	t.Helper()
	_, err := p.owner.Exec(p.ctx, `TRUNCATE platform_lifecycle.manifest,platform_lifecycle.products,platform_lifecycle.accounts,
		platform_lifecycle.cursors,platform_lifecycle.inbox,platform_lifecycle.reconciliation,platform_manager.snapshots CASCADE`)
	mustManager(t, err)
	p.integration, p.otherIntegration, p.installation, p.otherInstallation = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	p.zone, err = time.LoadLocation("Europe/Moscow")
	mustManager(t, err)
	today := time.Now().In(p.zone).AddDate(0, 0, -1)
	p.opened = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, p.zone).UTC()
	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: []wire.Mapping{
		{Integration: p.integration, ProductCode: "pilot-review", Accounts: []string{"9007199254740993"}},
		{Integration: p.otherIntegration, ProductCode: "other-review", Accounts: []string{"2542"}},
	}})
	mustManager(t, err)
	mustManager(t, BootstrapLifecycle(p.ctx, p.owner, manifest))
	mustManager(t, p.owner.QueryRow(p.ctx, `UPDATE platform_lifecycle.products SET display_name='Pilot % Review',line='PRO'
		WHERE code='pilot-review' RETURNING id`).Scan(&p.product))
	_, err = p.owner.Exec(p.ctx, `INSERT INTO platform_lifecycle.initial_term_policies(product_id,rule,version,evidence)
		VALUES($1,'pro_7d',1,'P05 isolated fixture')`, p.product)
	mustManager(t, err)
}

func (p managerPilot) deliver(t *testing.T, client *http.Client, base string) {
	t.Helper()
	for i, kind := range []wire.Kind{wire.Installed, wire.Reauthorized, wire.AuthorizationChanged, wire.Uninstalled,
		wire.Reinstalled, wire.AuthorizationChanged, wire.Installed} {
		var e wire.Event
		e.ID, e.Version, e.Kind = uuid.New(), 1, kind
		e.Installation, e.Integration, e.Account = p.installation, p.integration, "9007199254740993"
		e.Sequence, e.Generation, e.OccurredAt = int64(i+1), 1, p.opened.Add(time.Duration(i-4)*time.Second)
		e.Evidence = wire.Evidence{ID: uuid.New(), Source: "core_oauth"}
		switch kind {
		case wire.AuthorizationChanged:
			e.Evidence.Source, e.Authorization = "core_policy", wire.ReauthRequired
		case wire.Uninstalled:
			e.Evidence.Source = "core_uninstall"
		}
		if i == 4 || i == 5 {
			e.Generation = 2
		}
		if i == 6 {
			e.Installation, e.Integration, e.Account = p.otherInstallation, p.otherIntegration, "2542"
			e.Sequence, e.OccurredAt = 1, p.opened
		}
		raw, err := json.Marshal(e)
		mustManager(t, err)
		req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, base+coreEventsPath, bytes.NewReader(raw))
		mustManager(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		mustManager(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		mustManager(t, err)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("confirmed Core event %s returned %d: %s", kind, resp.StatusCode, body)
		}
	}
}

type managerTestActor struct {
	subject uuid.UUID
	version int64
	grants  []string
}

type managerTestCommand struct {
	id, key, target uuid.UUID
	version         int64
}

func newManagerTestCommand(target uuid.UUID, version int64) managerTestCommand {
	return managerTestCommand{id: uuid.New(), key: uuid.New(), target: target, version: version}
}

func (c managerTestCommand) reviewPath() string {
	return managerPath + "installations/" + c.target.String() + "/review"
}
func (c managerTestCommand) receiptPath() string { return managerPath + "commands/" + c.id.String() }

type observedManagerResponse struct {
	status   int
	body     []byte
	etag     string
	retry    string
	cache    string
	location string
	err      error
}

func managerCall(client *http.Client, method, url string, actor managerTestActor, cmd managerTestCommand, body string) observedManagerResponse {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return observedManagerResponse{err: err}
	}
	if actor.subject != uuid.Nil {
		now := time.Now().UTC()
		envelope := map[string]any{
			"actor": map[string]any{"kind": "employee", "audience": "managers", "subject_id": actor.subject,
				"session_id": uuid.New(), "grants": actor.grants, "grants_version": actor.version},
			"request_id": uuid.New(), "issued_at": now, "deadline_at": now.Add(10 * time.Second),
		}
		if cmd.id != uuid.Nil {
			envelope["command_id"] = cmd.id
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			return observedManagerResponse{err: err}
		}
		req.Header.Set("X-Platform-Actor", base64.RawURLEncoding.EncodeToString(raw))
	}
	if cmd.id != uuid.Nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", cmd.key.String())
	}
	if method == http.MethodPut {
		req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(cmd.version, 10)))
	}
	res, err := client.Do(req)
	if err != nil {
		return observedManagerResponse{err: err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return observedManagerResponse{
		status: res.StatusCode, body: raw, etag: res.Header.Get("ETag"), retry: res.Header.Get("Retry-After"),
		cache: res.Header.Get("Cache-Control"), location: res.Header.Get("Location"), err: err,
	}
}

func (res observedManagerResponse) requireStatus(t *testing.T, status int) {
	t.Helper()
	mustManager(t, res.err)
	if res.status != status || res.cache != "no-store" {
		t.Fatalf("manager status=%d cache=%q, want %d and no-store: %s", res.status, res.cache, status, res.body)
	}
}

func (res observedManagerResponse) requireCode(t *testing.T, code string) managerErrorBody {
	t.Helper()
	want := managerJSON[managerErrorResponse](t, res.body).Error
	if want.Code != code || want.RequestID == uuid.Nil || want.Message == "" {
		t.Fatalf("unexpected manager error: %s", res.body)
	}
	return want
}

func managerRequest(t *testing.T, client *http.Client, method, url string, actor managerTestActor,
	cmd managerTestCommand, body string, status int) observedManagerResponse {
	t.Helper()
	res := managerCall(client, method, url, actor, cmd, body)
	res.requireStatus(t, status)
	return res
}

func managerList(t *testing.T, client *http.Client, base string, actor managerTestActor, query string) installationPage {
	t.Helper()
	res := managerRequest(t, client, http.MethodGet, base+managerPath+"installations"+query,
		actor, managerTestCommand{}, "", http.StatusOK)
	return managerJSON[installationPage](t, res.body)
}

func managerJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var result T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("invalid manager wire response %s: %v", body, err)
	}
	return result
}

func mustManager(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type managerProcess struct {
	public, private, management string
	command                     *exec.Cmd
	done                        chan error
	stopped                     bool
}

func startManagerProcess(t *testing.T, overrides ...string) *managerProcess {
	t.Helper()
	certs := filepath.Join(os.Getenv("PLATFORM_TEST_CERTS_DIR"), "platform")
	command := exec.Command(os.Getenv("PLATFORM_TEST_BINARY"))
	command.Env = append(os.Environ(),
		"PLATFORM_ACCESS_DATABASE_URL="+os.Getenv("PLATFORM_TEST_ACCESS_DSN"),
		"PLATFORM_JOBS_DATABASE_URL="+os.Getenv("PLATFORM_TEST_JOBS_DSN"),
		"PLATFORM_ACCESS_PROOF_PUBLIC_KEYS=", "PLATFORM_ACCESS_PILOT_ORIGIN=",
		"PLATFORM_TLS_CERT_FILE="+filepath.Join(certs, "tls.crt"),
		"PLATFORM_TLS_KEY_FILE="+filepath.Join(certs, "tls.key"),
		"PLATFORM_TLS_CLIENT_CA_FILE="+filepath.Join(certs, "ca.crt"),
		"PLATFORM_MANAGER_CURSOR_KEY=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		"PLATFORM_MANAGER_TIME_ZONE=Europe/Moscow", "PLATFORM_JOBS_POOL_MAX_CONNS=4",
		"PLATFORM_WORKERS=0", "PLATFORM_PUBLIC_HTTP_ADDRESS=127.0.0.1:0", "PLATFORM_PRIVATE_HTTP_ADDRESS=127.0.0.1:0",
		"PLATFORM_MANAGEMENT_HTTP_ADDRESS=127.0.0.1:0")
	command.Env = append(command.Env, overrides...)
	logs, err := command.StderrPipe()
	mustManager(t, err)
	mustManager(t, command.Start())
	proc := &managerProcess{command: command, done: make(chan error, 1)}
	t.Cleanup(func() { proc.stop(t) })
	started := make(chan [3]string, 1)
	go func() {
		var other []string
		scanner := bufio.NewScanner(logs)
		for scanner.Scan() {
			line := scanner.Text()
			const marker = "Platform listening "
			_, addresses, ok := strings.Cut(line, marker)
			if !ok {
				if len(other) < 10 {
					other = append(other, line)
				}
				continue
			}
			parts := strings.Fields(addresses)
			if len(parts) == 3 {
				started <- [3]string{
					"http://" + strings.TrimPrefix(parts[0], "public="),
					"https://localhost:" + strings.TrimPrefix(parts[1], "private=127.0.0.1:"),
					"http://" + strings.TrimPrefix(parts[2], "management="),
				}
			}
		}
		if err := command.Wait(); err != nil {
			proc.done <- fmt.Errorf("Platform process: %w (%s)", err, strings.Join(other, "\n"))
		} else {
			proc.done <- nil
		}
	}()
	select {
	case addresses := <-started:
		proc.public, proc.private, proc.management = addresses[0], addresses[1], addresses[2]
	case err := <-proc.done:
		proc.stopped = true
		t.Fatalf("Platform exited before listeners opened: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Platform listeners")
	}
	return proc
}

func (p *managerProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.command.Process.Signal(os.Interrupt)
	select {
	case err := <-p.done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(15 * time.Second):
		_ = p.command.Process.Kill()
		t.Error("Platform did not stop after interrupt")
	}
}

func (p managerPilot) event(t *testing.T, command managerTestCommand, actor managerTestActor, kind, reason string, version int64) uuid.UUID {
	t.Helper()
	var eventID uuid.UUID
	mustManager(t, p.owner.QueryRow(p.ctx, `
		SELECT e.id FROM platform_manager.review_events e JOIN platform_manager.event_outbox o ON o.event_id=e.id
		WHERE e.command_id=$1 AND e.episode_id=$2 AND e.actor_id=$3 AND e.kind=$4 AND e.reason=$5 AND e.version=$6
		  AND e.occurred_at<=e.recorded_at AND e.recorded_at<=o.created_at AND o.payload->>'id'=e.id::text
		  AND o.payload->>'kind'=e.kind AND NOT(o.payload ? 'reason')`,
		command.id, command.target, actor.subject, kind, reason, version).Scan(&eventID))
	return eventID
}

func (p managerPilot) countEffects(t *testing.T, expected int) {
	t.Helper()
	var history, outbox int
	mustManager(t, p.owner.QueryRow(p.ctx, `SELECT
		(SELECT count(*) FROM platform_manager.review_events), (SELECT count(*) FROM platform_manager.event_outbox)`).
		Scan(&history, &outbox))
	if history != expected || outbox != expected {
		t.Fatalf("review history/outbox = %d/%d, want %d each", history, outbox, expected)
	}
}

func (p managerPilot) verifyReviewRaces(t *testing.T, client *http.Client, first, second string, target uuid.UUID, actor managerTestActor,
	originalResult observedManagerResponse, original managerTestCommand, originalBody string, originalEvent uuid.UUID) {
	t.Helper()
	unreviewBody := `{"reviewed":false,"reason":"recheck"}`
	type attempt struct {
		cmd    managerTestCommand
		result observedManagerResponse
	}
	completions := make(chan attempt, 2)
	for _, peer := range []string{first, second} {
		go func() {
			c := newManagerTestCommand(target, 3)
			completions <- attempt{c, managerCall(client, http.MethodPut, peer+c.reviewPath(), actor, c, unreviewBody)}
		}()
	}
	success, rejected := <-completions, <-completions
	if success.result.status == http.StatusConflict {
		success, rejected = rejected, success
	}
	success.result.requireStatus(t, http.StatusOK)
	rejected.result.requireStatus(t, http.StatusConflict)
	if info := rejected.result.requireCode(t, "version_conflict"); info.Version != 4 || info.Retryable {
		t.Fatalf("competing command did not report current version: %+v", info)
	}
	removed := managerJSON[managerInstallation](t, success.result.body)
	if removed.Version != 4 || removed.Review.Reviewed || removed.Review.By != nil || removed.Review.At != nil ||
		success.result.etag != `"4"` {
		t.Fatalf("unreview left review state behind: %+v", removed)
	}
	p.event(t, success.cmd, actor, "installation_unreviewed", "recheck", 4)
	p.countEffects(t, 2)
	if p.event(t, original, actor, "installation_reviewed", "confirmed", 3) != originalEvent {
		t.Fatal("intervening unreview changed the first event ID")
	}
	old := managerRequest(t, client, http.MethodPut, second+original.reviewPath(), actor, original, originalBody, http.StatusOK)
	if !bytes.Equal(old.body, originalResult.body) || old.etag != originalResult.etag {
		t.Fatalf("intervening write changed original response: %+v", old)
	}
	stale := managerRequest(t, client, http.MethodPut, second+rejected.cmd.reviewPath(), actor, rejected.cmd, unreviewBody, http.StatusConflict)
	if !bytes.Equal(stale.body, rejected.result.body) {
		t.Fatalf("terminal rejected replay changed the original response: %s vs %s", stale.body, rejected.result.body)
	}
	receipt := managerRequest(t, client, http.MethodGet, first+rejected.cmd.receiptPath(),
		actor, managerTestCommand{}, "", http.StatusOK)
	if value := managerJSON[managerReceipt](t, receipt.body); value.State != "rejected" || value.ID != rejected.cmd.id {
		t.Fatalf("stale review receipt = %+v", value)
	}
	noop := newManagerTestCommand(target, 4)
	escapedReason := `{"reviewed":false,"reason":"` + strings.Repeat(`\ud83d\ude00`, 1000) + `"}`
	result := managerRequest(t, client, http.MethodPut, first+noop.reviewPath(), actor,
		noop, escapedReason, http.StatusOK)
	if result.etag != `"4"` {
		t.Fatalf("no-op review advanced version: %s", result.etag)
	}
	p.countEffects(t, 2)
}

func (p managerPilot) verifyReviewRollback(t *testing.T, client *http.Client, base string, target uuid.UUID, actor managerTestActor) {
	t.Helper()
	_, err := p.owner.Exec(p.ctx, `
		CREATE FUNCTION platform_manager.test_fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'isolated test outbox failure'; END
		$$;
		CREATE TRIGGER test_fail_outbox BEFORE INSERT ON platform_manager.event_outbox
		FOR EACH ROW EXECUTE FUNCTION platform_manager.test_fail_outbox();`)
	mustManager(t, err)
	defer func() {
		_, _ = p.owner.Exec(p.ctx, `DROP FUNCTION IF EXISTS platform_manager.test_fail_outbox() CASCADE`)
	}()
	c := newManagerTestCommand(target, 4)
	body := `{"reviewed":true,"reason":"try again"}`
	failed := managerRequest(t, client, http.MethodPut, base+c.reviewPath(), actor, c, body, http.StatusServiceUnavailable)
	if info := failed.requireCode(t, "dependency_unavailable"); !info.Retryable || failed.retry != "1" {
		t.Fatalf("database failure did not carry retry contract: %+v", failed)
	}
	managerRequest(t, client, http.MethodGet, base+c.receiptPath(), actor, managerTestCommand{}, "",
		http.StatusNotFound).requireCode(t, "resource_not_found")
	p.countEffects(t, 2)
	page := managerList(t, client, base, actor, "?kind=reinstall")
	if len(page.Items) != 1 || page.Items[0].Review.Reviewed || page.Items[0].Version != 4 {
		t.Fatalf("outbox failure left review state behind: %+v", page)
	}
	_, err = p.owner.Exec(p.ctx, `DROP FUNCTION platform_manager.test_fail_outbox() CASCADE`)
	mustManager(t, err)
	for _, invalid := range []string{
		`{}`, `{"reviewed":null}`, `{"reviewed":false,"reviewed":true}`, `{"Reviewed":true}`,
		`{"reviewed":true,"reason":null}`, `{"reviewed":true,"reason":"bad\u0000reason"}`,
	} {
		managerRequest(t, client, http.MethodPut, base+c.reviewPath(), actor, c, invalid,
			http.StatusBadRequest).requireCode(t, "validation_failed")
	}
	ret := managerRequest(t, client, http.MethodPut, base+c.reviewPath(), actor, c, body, http.StatusOK)
	if ret.etag != `"5"` {
		t.Fatalf("retry after rollback has ETag %s", ret.etag)
	}
	p.event(t, c, actor, "installation_reviewed", "try again", 5)
	p.countEffects(t, 3)
}

func (p managerPilot) verifyTermExpiry(t *testing.T, client *http.Client, first, second string,
	actor managerTestActor, reinstalled managerInstallation) {
	t.Helper()
	var ends time.Time
	mustManager(t, p.owner.QueryRow(p.ctx, `UPDATE platform_lifecycle.terms
		SET valid_until=statement_timestamp()+interval '2 seconds' WHERE product_id=$1 RETURNING valid_until`,
		reinstalled.Product).Scan(&ends))
	defer func() {
		_, err := p.owner.Exec(p.ctx, `UPDATE platform_lifecycle.terms SET valid_until=$2 WHERE product_id=$1`,
			reinstalled.Product, reinstalled.Access.ValidUntil)
		mustManager(t, err)
	}()
	query := "?limit=1&access_state=active&product_id=" + reinstalled.Product.String()
	page := managerList(t, client, first, actor, query)
	if page.Next == nil || page.Summary.Total != 2 || !page.Items[0].Access.Active || !page.SnapshotAt.Before(ends) {
		t.Fatalf("snapshot did not observe the still-current pair term: %+v", page)
	}
	time.Sleep(max(time.Until(ends)+20*time.Millisecond, 0))
	continued := managerList(t, client, second, actor, query+"&cursor="+url.QueryEscape(*page.Next))
	if len(continued.Items) != 1 || !continued.Items[0].Access.Active || continued.Items[0].Access.ComputedAt != page.SnapshotAt ||
		continued.Summary != page.Summary || continued.Next != nil {
		t.Fatalf("term expiry changed a previously issued snapshot: %+v", continued)
	}
	if fresh := managerList(t, client, second, actor, query); fresh.Summary.Total != 0 {
		t.Fatalf("a fresh query still considers the expired term active: %+v", fresh)
	}
}

func (p managerPilot) verifyScope(t *testing.T, client *http.Client, first, second string,
	actor managerTestActor, original managerTestCommand, originalBody string, reinstalled managerInstallation) {
	t.Helper()
	query := "?limit=1"
	page := managerList(t, client, first, actor, query)
	if page.Next == nil {
		t.Fatal("scope test needs a second page")
	}
	continuation := query + "&cursor=" + url.QueryEscape(*page.Next)
	ambiguous := uuid.New()
	_, err := p.owner.Exec(p.ctx, `INSERT INTO platform_lifecycle.integrations(id,product_id) VALUES($1,$2)`, ambiguous, p.product)
	mustManager(t, err)
	_, err = p.owner.Exec(p.ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,9007199254740993)`, ambiguous)
	mustManager(t, err)
	managerRequest(t, client, http.MethodGet, second+managerPath+"installations"+continuation,
		actor, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	if hidden := managerList(t, client, second, actor, "?product_id="+reinstalled.Product.String()); hidden.Summary.Total != 0 {
		t.Fatalf("ambiguous pair was readable: %+v", hidden)
	}
	managerRequest(t, client, http.MethodGet, second+original.receiptPath(), actor, managerTestCommand{}, "",
		http.StatusNotFound).requireCode(t, "resource_not_found")
	managerRequest(t, client, http.MethodPut, second+original.reviewPath(), actor, original, originalBody,
		http.StatusNotFound).requireCode(t, "resource_not_found")
	_, err = p.owner.Exec(p.ctx, `DELETE FROM platform_lifecycle.scope WHERE integration_id=$1`, ambiguous)
	mustManager(t, err)
	_, err = p.owner.Exec(p.ctx, `DELETE FROM platform_lifecycle.integrations WHERE id=$1`, ambiguous)
	mustManager(t, err)
	_, err = p.owner.Exec(p.ctx, `UPDATE platform_manager.snapshots SET expires_at=now()-interval '1 second' WHERE subject_id=$1`, actor.subject)
	mustManager(t, err)
	managerRequest(t, client, http.MethodGet, second+managerPath+"installations"+continuation,
		actor, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	_, err = p.owner.Exec(p.ctx, `REVOKE SELECT ON platform_manager.installations FROM platform_jobs`)
	mustManager(t, err)
	defer func() {
		_, err := p.owner.Exec(p.ctx, `GRANT SELECT ON platform_manager.installations TO platform_jobs`)
		mustManager(t, err)
	}()
	managerRequest(t, client, http.MethodGet, first+managerPath+"installations",
		actor, managerTestCommand{}, "", http.StatusServiceUnavailable).requireCode(t, "dependency_unavailable")
}

func (p managerPilot) verifySnapshotCaps(t *testing.T, client *http.Client, first, second string, actor managerTestActor) {
	t.Helper()
	peers := []string{first, second}
	urlPath := managerPath + "installations?limit=1"
	results := make(chan observedManagerResponse, 8)
	for i := 0; i < 8; i++ {
		go func() {
			results <- managerCall(client, http.MethodGet, peers[i%2]+urlPath, actor, managerTestCommand{}, "")
		}()
	}
	var issued []string
	for i := 0; i < 8; i++ {
		res := <-results
		res.requireStatus(t, http.StatusOK)
		page := managerJSON[installationPage](t, res.body)
		if page.Next == nil {
			t.Fatal("concurrent admit returned no cursor")
		}
		if slices.Contains(issued, *page.Next) {
			t.Fatal("fresh list request reused a cursor")
		}
		issued = append(issued, *page.Next)
	}
	var continuations int
	for _, cursor := range issued {
		got := managerCall(client, http.MethodGet, second+urlPath+"&cursor="+url.QueryEscape(cursor), actor, managerTestCommand{}, "")
		if got.status == http.StatusOK {
			got.requireStatus(t, http.StatusOK)
			continuations++
		} else {
			got.requireStatus(t, http.StatusBadRequest)
			got.requireCode(t, "invalid_cursor")
		}
	}
	if continuations != 4 {
		t.Fatalf("concurrent employee admissions left %d live cursors, want 4", continuations)
	}
	var total, expired int
	mustManager(t, p.owner.QueryRow(p.ctx, `SELECT count(*),count(*) FILTER (WHERE expires_at<now())
		FROM platform_manager.snapshots`).Scan(&total, &expired))
	if total != 4 || expired != 0 {
		t.Fatalf("snapshot GC and employee limit left %d rows, %d expired", total, expired)
	}
	others := make(chan observedManagerResponse, 32)
	for i := 0; i < 32; i++ {
		go func() {
			other := managerTestActor{subject: uuid.New(), version: 1, grants: actor.grants}
			others <- managerCall(client, http.MethodGet, peers[i%2]+urlPath, other, managerTestCommand{}, "")
		}()
	}
	var throttled int
	for i := 0; i < 32; i++ {
		result := <-others
		if result.status == http.StatusOK {
			result.requireStatus(t, http.StatusOK)
			continue
		}
		result.requireStatus(t, http.StatusTooManyRequests)
		info := result.requireCode(t, "rate_limited")
		if !info.Retryable || result.retry != "300" {
			t.Fatalf("snapshot capacity did not return Retry-After: %+v", result)
		}
		throttled++
	}
	mustManager(t, p.owner.QueryRow(p.ctx, `SELECT count(*) FROM platform_manager.snapshots`).Scan(&total))
	if total != 32 || throttled != 4 {
		t.Fatalf("global admission exceeded hard cap: snapshots=%d throttled=%d", total, throttled)
	}
	_, err := p.owner.Exec(p.ctx, `WITH episodes AS (
		INSERT INTO platform_lifecycle.episodes(id,installation_id,generation,opened_at,closed_at)
		SELECT gen_random_uuid(),$1,n,now(),now() FROM generate_series(1000,11000) n RETURNING id,generation
	), inbox AS (
		INSERT INTO platform_lifecycle.inbox(event_id,installation_id,sequence,body)
		SELECT id,$1,generation,'fixture'::bytea FROM episodes RETURNING event_id
	)
	INSERT INTO platform_lifecycle.history(event_id,episode_id,kind,occurred_at)
	SELECT event_id,event_id,'reinstalled',now() FROM inbox`, p.installation)
	mustManager(t, err)
	managerRequest(t, client, http.MethodGet, first+urlPath, actor, managerTestCommand{}, "",
		http.StatusBadRequest).requireCode(t, "query_too_broad")
}
