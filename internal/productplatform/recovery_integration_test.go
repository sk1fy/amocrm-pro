package productplatform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

type recoveryCommand struct {
	ID, Key, Target  uuid.UUID
	Version          int64
	Body, Path, ETag string
	Response         []byte
}

func (c recoveryCommand) command() managerTestCommand {
	return managerTestCommand{id: c.ID, key: c.Key, target: c.Target, version: c.Version}
}

type recoveryFacts struct {
	Actor                      uuid.UUID
	Event                      wire.Event
	Commands                   []recoveryCommand
	Feed                       []uuid.UUID
	Access                     accessState
	Lifecycle, Reviews, Outbox int
}

// TestPlatformRecovery is deliberately phased: the shell owns dump, restore and image changes.
func TestPlatformRecovery(t *testing.T) {
	phase := os.Getenv("PLATFORM_RECOVERY_PHASE")
	if phase == "" {
		t.Skip("run scripts/platform-recovery-test.sh")
	}
	if phase != "seed" && phase != "verify" {
		t.Fatalf("unknown recovery phase %q", phase)
	}
	for _, key := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN", "PLATFORM_RECOVERY_FACTS"} {
		if os.Getenv(key) == "" {
			t.Fatalf("missing required recovery input %s", key)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	var facts recoveryFacts
	if phase == "seed" {
		pilot.fixture(t)
		facts.Actor = uuid.New()
		facts.Event = wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Installed, Installation: pilot.installation, Integration: pilot.integration, Account: "9007199254740993", Sequence: 1, Generation: 1, OccurredAt: time.Now().UTC(), Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}}
	} else {
		raw, err := os.ReadFile(os.Getenv("PLATFORM_RECOVERY_FACTS"))
		mustManager(t, err)
		mustManager(t, json.Unmarshal(raw, &facts))
	}
	actor := managerTestActor{subject: facts.Actor, version: 1, grants: []string{"clients.read", "clients.write", "feed.read"}}
	// Each phase gets an ephemeral signing key; the evidence file never contains a private key or bearer proof.
	keyConfig, priv := recoveryAccessKeys(t)
	process := startManagerBinaryProcess(t, os.Getenv("PLATFORM_TEST_BINARY"), "PLATFORM_ACCESS_PROOF_PUBLIC_KEYS="+keyConfig)
	manager := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "manager-bff")
	core := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "core")
	public := &http.Client{Timeout: 3 * time.Second}
	expectStatus(t, public, process.public+"/ready", http.StatusOK)
	expectStatus(t, public, process.management+"/ready", http.StatusOK)
	expectStatus(t, public, process.public+managerPath+"installations", http.StatusNotFound)
	expectStatus(t, core, process.private+managerPath+"installations", http.StatusForbidden)
	if res, err := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "").Get(process.private + managerPath + "installations"); err == nil {
		res.Body.Close()
		t.Fatal("mTLS accepted no client certificate")
	}
	deliver := func() {
		raw, err := json.Marshal(facts.Event)
		mustManager(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, process.private+coreEventsPath, bytes.NewReader(raw))
		mustManager(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := core.Do(req)
		mustManager(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		mustManager(t, err)
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("lifecycle delivery status %d: %s", res.StatusCode, body)
		}
	}
	deliver()
	if phase == "seed" {
		page := managerList(t, manager, process.private, actor, "")
		if len(page.Items) != 1 {
			t.Fatalf("expected one synthetic installation: %+v", page)
		}
		installation := page.Items[0]
		commands := []recoveryCommand{
			{Target: installation.ID, Version: installation.Version, Path: managerPath + "installations/" + installation.ID.String() + "/review", Body: `{"reviewed":true,"reason":"recovery proof"}`},
			{Target: installation.Account, Version: 1, Path: managerPath + "accounts/" + installation.Account.String() + "/profile", Body: `{"display_name":"Synthetic recovery account","classification":"Без партнёра","note":"Synthetic recovery note","reason":"recovery proof"}`},
		}
		for i, c := range commands {
			c.ID, c.Key = uuid.New(), uuid.New()
			method := http.MethodPut
			if i == 1 {
				method = http.MethodPatch
			}
			response := managerRequest(t, manager, method, process.private+c.Path, actor, c.command(), c.Body, http.StatusOK)
			c.Response, c.ETag = response.body, response.etag
			facts.Commands = append(facts.Commands, c)
		}
	}
	for i, c := range facts.Commands {
		method := http.MethodPut
		if i == 1 {
			method = http.MethodPatch
		}
		replay := managerRequest(t, manager, method, process.private+c.Path, actor, c.command(), c.Body, http.StatusOK)
		if !bytes.Equal(replay.body, c.Response) || replay.etag != c.ETag {
			t.Fatalf("recovered command %s changed saved response or ETag", c.ID)
		}
		receipt := managerJSON[managerReceipt](t, managerRequest(t, manager, http.MethodGet, process.private+c.command().receiptPath(), actor, managerTestCommand{}, "", http.StatusOK).body)
		if receipt.ID != c.ID || receipt.State != "completed" {
			t.Fatalf("lost original command receipt: %+v", receipt)
		}
	}
	page := managerList(t, manager, process.private, actor, "")
	if len(page.Items) != 1 || !page.Items[0].Review.Reviewed || page.Items[0].Version != 2 {
		t.Fatalf("recovered installation facts changed: %+v", page)
	}
	detail := managerRequest(t, manager, http.MethodGet, process.private+managerPath+"accounts/"+page.Items[0].Account.String(), actor, managerTestCommand{}, "", http.StatusOK)
	if detail.etag != facts.Commands[1].ETag || !bytes.Contains(detail.body, []byte(`"display_name":"Synthetic recovery account"`)) {
		t.Fatalf("recovered profile changed: %s", detail.body)
	}
	feed := managerJSON[eventPage](t, managerRequest(t, manager, http.MethodGet, process.private+managerPath+"feed", actor, managerTestCommand{}, "", http.StatusOK).body)
	if len(feed.Items) != 3 {
		t.Fatalf("expected installed, reviewed and profile events: %+v", feed.Items)
	}
	for _, kind := range []string{"installed", "installation_reviewed", "account_profile_updated"} {
		if !slices.ContainsFunc(feed.Items, func(event businessEvent) bool { return event.Kind == kind }) {
			t.Fatalf("feed missing %s", kind)
		}
	}
	ids := make([]uuid.UUID, 0, len(feed.Items))
	for _, event := range feed.Items {
		ids = append(ids, event.ID)
	}
	if phase == "seed" {
		facts.Feed = ids
	} else if !slices.Equal(ids, facts.Feed) {
		t.Fatalf("feed facts changed: %v, want %v", ids, facts.Feed)
	}
	access := recoveryPublicAccess(t, ctx, process.public, priv, platformaccess.Scope{Account: facts.Event.Account, Product: "pilot-review", Integration: facts.Event.Integration, Installation: facts.Event.Installation, Generation: 1, AuthorizationSequence: 1})
	access.ComputedAt = time.Time{}
	if phase == "seed" {
		facts.Access = access
	} else {
		want, _ := json.Marshal(facts.Access)
		got, _ := json.Marshal(access)
		if !bytes.Equal(got, want) {
			t.Fatalf("public access facts changed: %s / %s", got, want)
		}
	}
	if !access.Active || access.Source != "own_trial" {
		t.Fatalf("expected preserved trial access: %+v", access)
	}
	var lifecycle, reviews, outbox int
	mustManager(t, owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM platform_lifecycle.inbox),(SELECT count(*) FROM platform_manager.review_events),(SELECT count(*) FROM platform_manager.event_outbox)`).Scan(&lifecycle, &reviews, &outbox))
	if phase == "seed" {
		facts.Lifecycle, facts.Reviews, facts.Outbox = lifecycle, reviews, outbox
	} else if lifecycle != facts.Lifecycle || reviews != facts.Reviews || outbox != facts.Outbox {
		t.Fatalf("replay duplicated lifecycle/review/outbox effects: %d/%d/%d", lifecycle, reviews, outbox)
	}
	if lifecycle != 1 || reviews != 1 {
		t.Fatalf("unexpected seeded effects: %d/%d", lifecycle, reviews)
	}
	process.stop(t)
	if phase == "seed" {
		raw, err := json.MarshalIndent(facts, "", "  ")
		mustManager(t, err)
		mustManager(t, os.WriteFile(os.Getenv("PLATFORM_RECOVERY_FACTS"), raw, 0600))
	}
	t.Logf("recovery %s: checked mTLS, readiness, public access, profile, original receipts/replays and %d feed events", phase, len(ids))
}

func recoveryAccessKeys(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	mustManager(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	mustManager(t, err)
	path := filepath.Join(t.TempDir(), "public.pem")
	mustManager(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600))
	config, err := json.Marshal(map[string]string{"recovery": path})
	mustManager(t, err)
	return string(config), priv
}

func recoveryPublicAccess(t *testing.T, ctx context.Context, base string, key ed25519.PrivateKey, scope platformaccess.Scope) accessState {
	t.Helper()
	now := time.Now()
	claims, err := json.Marshal(platformaccess.Claims{Scope: scope, Issuer: platformaccess.Issuer, Audience: platformaccess.Audience, IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()})
	mustManager(t, err)
	message := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"recovery"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	proof := message + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(message)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/access/"+scope.Account+"/"+scope.Product, nil)
	mustManager(t, err)
	req.Header.Set("Authorization", "Bearer "+proof)
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	mustManager(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	mustManager(t, err)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("public access status %d: %s", res.StatusCode, raw)
	}
	return managerJSON[accessState](t, raw)
}
