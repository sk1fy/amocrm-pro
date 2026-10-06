package distribution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
)

func dpScopedRequest(t *testing.T, key string, accountID, leadID, ts int64, groupID string) *http.Request {
	t.Helper()
	body := map[string]any{
		"event": map[string]any{
			"type": 15, "type_code": "lead_appeared_in_status", "time": ts,
			"data": map[string]any{"id": leadID, "element_type": 2, "status_id": 30, "pipeline_id": 20, "direction_of_movement": "went_to_trigger"},
		},
		"action":     map[string]any{"settings": map[string]any{"widget": map[string]any{"settings": map[string]any{"key": key, "groupId": groupID}}}},
		"subdomain":  "test",
		"account_id": accountID,
	}
	raw, _ := json.Marshal(body)
	return httptest.NewRequest(http.MethodPost, "/api/v1/widget/distribution/dp", strings.NewReader(string(raw)))
}

// Regression for the per-group Digital Pipeline credential: issuance is
// idempotent and stores only hash+sealed copy, and the receiver strictly scopes
// group/account/active-installation/current-binding-revision while the legacy
// shared webhook key stays compatible.
func TestDPCredentialScopedReceiver(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	scope := f.Assignment.Scope
	installID, accountID := scope.InstallationID, scope.AccountID

	legacy := "legacy-shared-secret"
	legacyHash := sha256.Sum256([]byte(legacy))
	if _, e := f.Pool.Exec(ctx, `UPDATE installations SET webhook_key_hash=$1, webhook_status='active' WHERE id=$2`, legacyHash[:], installID); e != nil {
		t.Fatal(e)
	}

	ring, e := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x42}, cryptox.KeySize)}, 1)
	if e != nil {
		t.Fatal(e)
	}
	h := &Handler{Store: f.Store, Cipher: ring}
	groupID := uuid.New()

	key, e := h.dpCredentialIssue(ctx, scope.Binding(), groupID)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(key, "dp_") || len(key) < 32 || len(key) > 128 {
		t.Fatalf("unexpected dp key shape (len=%d)", len(key))
	}
	again, e := h.dpCredentialIssue(ctx, scope.Binding(), groupID)
	if e != nil || again != key {
		t.Fatalf("issuance not idempotent")
	}
	var storedHash, ciphertext []byte
	var bindingRevision int64
	if e = f.Pool.QueryRow(ctx, `SELECT key_hash, key_ciphertext, binding_revision FROM distribution_dp_credentials WHERE group_id=$1`, groupID).Scan(&storedHash, &ciphertext, &bindingRevision); e != nil {
		t.Fatal(e)
	}
	want := sha256.Sum256([]byte(key))
	if !bytes.Equal(storedHash, want[:]) || len(ciphertext) == 0 || bindingRevision != scope.BindingRevision || bytes.Contains(ciphertext, []byte(key)) {
		t.Fatalf("credential storage must be hash+sealed only")
	}

	recv := &DPReceiver{Pool: f.Pool, Jobs: f.Jobs, Installations: installations.NewStore(f.Pool), Store: f.Store, MaxBody: MaxBody}

	ok := httptest.NewRecorder()
	recv.Receive(ok, dpScopedRequest(t, key, accountID, 10, 1491300016, groupID.String()))
	if ok.Code != http.StatusAccepted {
		t.Fatalf("scoped trigger must 202, got %d %s", ok.Code, ok.Body.String())
	}
	var inboxGroup uuid.UUID
	if e = f.Pool.QueryRow(ctx, `SELECT group_id FROM distribution_dp_inbox`).Scan(&inboxGroup); e != nil || inboxGroup != groupID {
		t.Fatalf("inbox group mismatch: %s %v", inboxGroup, e)
	}
	dup := httptest.NewRecorder()
	recv.Receive(dup, dpScopedRequest(t, key, accountID, 10, 1491300016, groupID.String()))
	if dup.Code != http.StatusOK {
		t.Fatalf("duplicate must 200, got %d", dup.Code)
	}
	forged := httptest.NewRecorder()
	recv.Receive(forged, dpScopedRequest(t, key, accountID, 11, 1491300017, uuid.New().String()))
	if forged.Code != http.StatusNotFound {
		t.Fatalf("forged group must 404, got %d", forged.Code)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE distribution_dp_credentials SET binding_revision=binding_revision+1 WHERE group_id=$1`, groupID); e != nil {
		t.Fatal(e)
	}
	stale := httptest.NewRecorder()
	recv.Receive(stale, dpScopedRequest(t, key, accountID, 12, 1491300018, groupID.String()))
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale binding revision must 404, got %d", stale.Code)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE distribution_dp_credentials SET binding_revision=$2 WHERE group_id=$1`, groupID, scope.BindingRevision); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE installations SET status='disabled' WHERE id=$1`, installID); e != nil {
		t.Fatal(e)
	}
	inactive := httptest.NewRecorder()
	recv.Receive(inactive, dpScopedRequest(t, key, accountID, 13, 1491300019, groupID.String()))
	if inactive.Code != http.StatusNotFound {
		t.Fatalf("inactive installation must 404, got %d", inactive.Code)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, installID); e != nil {
		t.Fatal(e)
	}
	legacyRec := httptest.NewRecorder()
	recv.Receive(legacyRec, dpScopedRequest(t, legacy, accountID, 14, 1491300020, uuid.New().String()))
	if legacyRec.Code != http.StatusAccepted {
		t.Fatalf("legacy key must stay compatible, got %d %s", legacyRec.Code, legacyRec.Body.String())
	}
}

// Regression across the real signed bridge policy for the credential endpoint:
// the scope/revision are authenticated by the HMAC headers and the service
// grant, and a forged/foreign shape is refused before any key is minted.
func TestDPCredentialBridgeEndpoint(t *testing.T) {
	f := assignmentFixture(t)
	asg := f.Assignment.Scope
	const key = "service"
	scope := Scope{KeyID: key, CompanyID: asg.CompanyID, InstallationID: asg.InstallationID}
	ring, e := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x42}, cryptox.KeySize)}, 1)
	if e != nil {
		t.Fatal(e)
	}
	h := &Handler{Store: f.Store, Cipher: ring}
	secret := strings.Repeat("k", 32)
	router := chi.NewRouter()
	h.RegisterService(router, Auth{Keys: map[string]string{key: secret}, Store: f.Store})
	groupID := uuid.New()
	call := func(s Scope, binding uuid.UUID, revision int64, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/distribution/bindings/"+binding.String()+"/dp-credentials?bindingRevision="+strconv.FormatInt(revision, 10), bytes.NewReader(raw))
		Sign(req, s, secret, raw)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	out := call(scope, asg.BindingID, asg.BindingRevision, map[string]any{"groupId": groupID})
	if out.Code != 200 || !strings.Contains(out.Body.String(), "dp_") || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("valid issue failed: %d %s cc=%q", out.Code, out.Body, out.Header().Get("Cache-Control"))
	}
	if o := call(scope, asg.BindingID, asg.BindingRevision, map[string]any{}); o.Code != 400 {
		t.Fatalf("missing group must 400, got %d", o.Code)
	}
	if o := call(scope, asg.BindingID, asg.BindingRevision+1, map[string]any{"groupId": groupID}); o.Code != 409 {
		t.Fatalf("wrong revision must 409, got %d", o.Code)
	}
	foreign := Scope{KeyID: key, CompanyID: uuid.New(), InstallationID: asg.InstallationID}
	if o := call(foreign, asg.BindingID, asg.BindingRevision, map[string]any{"groupId": groupID}); o.Code != 401 && o.Code != 403 {
		t.Fatalf("foreign company must be denied, got %d", o.Code)
	}
}
