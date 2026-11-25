package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformProductsAcrossReplicas(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
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
	first, second := startManagerProcess(t), startManagerProcess(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	client := tlsClient(t, certs, "manager-bff")
	client.Timeout = 6 * time.Second
	core := tlsClient(t, certs, "core")
	core.Timeout = 8 * time.Second
	pilot.deliver(t, core, first.private)
	editor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "catalog.manage"}}
	stats := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"stats.read"}}
	noStats := managerTestActor{subject: editor.subject, version: 2, grants: []string{"clients.read", "catalog.manage"}}
	noCatalog := managerTestActor{subject: editor.subject, version: 2, grants: []string{"clients.read", "configuration.manage"}}
	const base = managerPath + "products"
	catalogReader := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"catalog.read", "stats.read"}}
	managerRequest(t, client, http.MethodGet, first.private+base, catalogReader, managerTestCommand{}, "", http.StatusOK)
	managerRequest(t, client, http.MethodGet, first.private+base+"?overview=true", catalogReader, managerTestCommand{}, "", http.StatusForbidden)
	managerRequest(t, client, http.MethodGet, first.private+managerPath+"accounts", catalogReader, managerTestCommand{}, "", http.StatusForbidden)
	hidden := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet, first.private+base, editor, managerTestCommand{}, "", http.StatusOK).body)
	if hidden.Items == nil || len(hidden.Items) != 0 {
		t.Fatalf("unadopted P03 rows must be hidden: %+v", hidden)
	}
	productPath := base + "/" + pilot.product.String()
	createCmd := newManagerTestCommand(uuid.Nil, 0)
	createBody := fmt.Sprintf(`{"code":"pilot-review","display_name":"Reviewed product","line":"PRO","availability":"pilot",
		"integration_refs":[%q],"tariff_version":null,"trial_policy":{"kind":"once","duration_days":7},"links":[]}`, pilot.integration)
	adopted := managerRequest(t, client, http.MethodPost, first.private+base, editor, createCmd, createBody, http.StatusCreated)
	product := managerJSON[managerProduct](t, adopted.body)
	if product.ID != pilot.product || adopted.etag != `"1"` || product.Trial.Days == nil || *product.Trial.Days != 7 || len(product.Integrations) != 1 {
		t.Fatalf("catalog did not preserve P03 UUID and trial: %s", adopted.body)
	}
	termBefore := struct {
		Until          time.Time
		Version        int64
		Evidence, Rule string
	}{}
	mustManager(t, owner.QueryRow(ctx, `SELECT valid_until,policy_version,policy_evidence,policy_rule
		FROM platform_lifecycle.terms WHERE product_id=$1`, pilot.product).
		Scan(&termBefore.Until, &termBefore.Version, &termBefore.Evidence, &termBefore.Rule))
	if termBefore.Rule != "pro_7d" || termBefore.Evidence != "P05 isolated fixture" {
		t.Fatalf("unexpected original term %+v", termBefore)
	}

	draftCmd := newManagerTestCommand(uuid.Nil, 0)
	const draftBody = `{"code":"draft-widget","display_name":"Draft widget","line":"TOP","availability":"draft","integration_refs":[],
		"tariff_version":null,"trial_policy":{"kind":"none","duration_days":null},"links":[{"label":"Docs","url":"http://example.test/widget"}]}`
	draft := managerJSON[managerProduct](t, managerRequest(t, client, http.MethodPost, second.private+base, editor, draftCmd, draftBody, http.StatusCreated).body)
	if draft.Code != "draft-widget" || draft.Integrations == nil || len(draft.Integrations) != 0 {
		t.Fatalf("unmapped draft was not recorded: %+v", draft)
	}
	page := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet, first.private+base+"?limit=1", editor, managerTestCommand{}, "", http.StatusOK).body)
	if page.Next == nil || len(page.Items) != 1 || page.Items[0].ID != draft.ID {
		t.Fatalf("bad product cursor: %+v", page)
	}
	cursorPath := base + "?limit=1&cursor=" + url.QueryEscape(*page.Next)
	next := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet, second.private+cursorPath, editor, managerTestCommand{}, "", http.StatusOK).body)
	if len(next.Items) != 1 || next.Items[0].ID != pilot.product || next.Next != nil || !next.SnapshotAt.Equal(page.SnapshotAt) {
		t.Fatalf("cross-replica catalog paging changed order or snapshot: %+v", next)
	}
	reader := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "stats.read"}}
	overviewPath := base + "?overview=true&limit=1"
	managerRequest(t, client, http.MethodGet, first.private+overviewPath, noStats, managerTestCommand{}, "", http.StatusForbidden)
	firstStats := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet,
		first.private+overviewPath, reader, managerTestCommand{}, "", http.StatusOK).body)
	if len(firstStats.Items) != 1 || firstStats.Items[0].ID != pilot.product || firstStats.Next == nil ||
		firstStats.Overview == nil || firstStats.Overview.Summary.Total != 2 || firstStats.Overview.Summary.ByLine["PRO"] != 1 {
		t.Fatalf("unexpected catalog overview: %+v", firstStats)
	}
	series := firstStats.Overview.Statistics[pilot.product]
	if !series.MissingInstallLink || !series.MissingInstruction {
		t.Fatalf("missing pilot links were not flagged: %+v", series)
	}
	var installations int
	for _, month := range series.Months {
		installations += month.Count
	}
	if len(series.Months) != 12 || installations == 0 || installations != series.Installations || len(firstStats.Overview.Statistics) != 1 {
		t.Fatalf("unexpected series for monthly installations: %+v", series)
	}
	secondStats := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet,
		second.private+overviewPath+"&cursor="+url.QueryEscape(*firstStats.Next), reader, managerTestCommand{}, "", http.StatusOK).body)
	if len(secondStats.Items) != 1 || secondStats.Items[0].ID != draft.ID || secondStats.Next != nil ||
		secondStats.Overview == nil || secondStats.Overview.Summary.Total != 2 || !secondStats.SnapshotAt.Equal(firstStats.SnapshotAt) {
		t.Fatalf("catalog overview changed across pages: %+v", secondStats)
	}
	if draftStats := secondStats.Overview.Statistics[draft.ID]; draftStats.MissingInstallLink || draftStats.MissingInstruction {
		t.Fatalf("draft links were flagged: %+v", draftStats)
	}
	badRefs := fmt.Sprintf(`{"integration_refs":[%q]}`, pilot.otherIntegration)
	managerRequest(t, client, http.MethodPatch, first.private+productPath, editor, newManagerTestCommand(pilot.product, 1), badRefs, http.StatusUnprocessableEntity).
		requireCode(t, "integration_mapping_mismatch")
	managerRequest(t, client, http.MethodPatch, first.private+productPath, editor, newManagerTestCommand(pilot.product, 1),
		`{"line":"TOP","trial_policy":{"kind":"once","duration_days":90}}`, http.StatusUnprocessableEntity).requireCode(t, "trial_evidence_required")
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.verified_tariff_versions(product_id,version,evidence) VALUES($1,9,'other product proof')`, draft.ID)
	mustManager(t, err)
	managerRequest(t, client, http.MethodPatch, second.private+productPath, editor, newManagerTestCommand(pilot.product, 1),
		`{"tariff_version":9}`, http.StatusUnprocessableEntity).requireCode(t, "tariff_evidence_required")
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.verified_trial_policies(product_id,version,rule,evidence)
		VALUES($1,2,'top_90d','approved TOP trial')`, pilot.product)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.verified_tariff_versions(product_id,version,evidence)
		VALUES($1,3,'approved tariff revision')`, pilot.product)
	mustManager(t, err)
	changedCmd := newManagerTestCommand(pilot.product, 1)
	const changedBody = `{"line":"TOP","trial_policy":{"kind":"once","duration_days":90},"tariff_version":3,"display_name":"Current catalog","trial_extension_days":14,
        "tariff_options":[{"name":"Base","price_kopecks":150050,"months":12,"per_user":true},{"name":"Promo","months":2,"price_kopecks":0,"per_user":false}],
		"links":[{"label":"Инструкция","url":"https://docs.example.test/widget"}]}`
	changed := managerRequest(t, client, http.MethodPatch, first.private+productPath, editor, changedCmd, changedBody, http.StatusOK)
	current := managerJSON[managerProduct](t, changed.body)
	if changed.etag != `"2"` || current.Tariff == nil || *current.Tariff != 3 || current.Line != "TOP" {
		t.Fatalf("verified policy selection failed: %s", changed.body)
	}
	persisted := managerJSON[managerProduct](t, managerRequest(t, client, http.MethodGet, second.private+productPath, catalogReader, managerTestCommand{}, "", http.StatusOK).body)
	if persisted.TrialExtensionDays != 14 || len(persisted.TariffOptions) != 2 || persisted.TariffOptions[0] != (productTariffOption{Name: "Base", PriceKopecks: 150050, Months: 12, PerUser: true}) || persisted.TariffOptions[1].PriceKopecks != 0 {
		t.Fatalf("unexpected persisted tariffs: %+v", persisted)
	}
	managerRequest(t, client, http.MethodGet, second.private+cursorPath, editor, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	if _, err := owner.Exec(ctx, `UPDATE platform_lifecycle.initial_term_policies SET evidence='unapproved direct edit' WHERE product_id=$1`, pilot.product); err == nil {
		t.Fatal("adopted catalog allowed direct policy evidence drift")
	}
	reauthorized := wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Reauthorized, Installation: pilot.installation, Integration: pilot.integration,
		Account: "9007199254740993", Sequence: 7, Generation: 2, OccurredAt: time.Now().UTC(), Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}}
	deliverProductEvent(t, pilot, core, first.private, reauthorized)
	attentionPath := base + "?overview=true&attention=true"
	attention := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet,
		second.private+attentionPath, reader, managerTestCommand{}, "", http.StatusOK).body)
	if len(attention.Items) != 1 || attention.Items[0].ID != pilot.product ||
		attention.Overview == nil || attention.Overview.Summary.Attention != 1 {
		t.Fatalf("pilot with missing install link is absent from attention results: %+v", attention)
	}
	if row := attention.Overview.Statistics[pilot.product]; !row.MissingInstallLink || row.MissingInstruction || row.ConnectionCount != 0 {
		t.Fatalf("unexpected pilot attention flags: %+v", row)
	}
	metricsPath := productPath + "/metrics"
	metrics := managerJSON[productMetrics](t, managerRequest(t, client, http.MethodGet, first.private+metricsPath, stats, managerTestCommand{}, "", http.StatusOK).body)
	if metrics.Installations != 2 || metrics.ActiveAccess != 1 || metrics.Activations != 0 {
		t.Fatalf("reauth inflated operational metrics: %+v", metrics)
	}
	_, err = owner.Exec(ctx, `INSERT INTO platform_manager.installation_contacts(episode_id,amo_user_id,name,phone,consent_version)
		SELECT id,1,'Fixture contact','+19995550000',$2 FROM platform_lifecycle.episodes WHERE installation_id=$1 AND generation=2`,
		pilot.installation, wire.ContactConsent)
	mustManager(t, err)
	withActivation := managerJSON[productMetrics](t, managerRequest(t, client, http.MethodGet, first.private+metricsPath, stats, managerTestCommand{}, "", http.StatusOK).body)
	if withActivation.Activations != 1 || withActivation.Installations != metrics.Installations {
		t.Fatalf("activation was not counted for the installation: %+v", withActivation)
	}
	managerRequest(t, client, http.MethodGet, first.private+metricsPath, noStats, managerTestCommand{}, "", http.StatusForbidden)
	future := managerJSON[productMetrics](t, managerRequest(t, client, http.MethodGet, second.private+metricsPath+"?from=2099-01-01", stats, managerTestCommand{}, "", http.StatusOK).body)
	if future.Installations != 0 || future.Activations != 0 || future.ActiveAccess != metrics.ActiveAccess {
		t.Fatalf("historical filter changed current gauge: %+v", future)
	}

	noTrialCmd := newManagerTestCommand(pilot.product, 2)
	noTrialBody := `{"trial_policy":{"kind":"none","duration_days":null},"availability":"archived","tariff_version":null}`
	disabled := managerRequest(t, client, http.MethodPatch, second.private+productPath, editor, noTrialCmd, noTrialBody, http.StatusOK)
	if disabled.etag != `"3"` {
		t.Fatalf("disable version %q", disabled.etag)
	}
	attention = managerJSON[productPage](t, managerRequest(t, client, http.MethodGet,
		first.private+attentionPath, reader, managerTestCommand{}, "", http.StatusOK).body)
	if len(attention.Items) != 0 || attention.Overview == nil || attention.Overview.Summary.Attention != 0 {
		t.Fatalf("archived widget still requires attention for missing links: %+v", attention)
	}
	var sameTerm bool
	mustManager(t, owner.QueryRow(ctx, `SELECT valid_until=$2 AND policy_version=$3 AND policy_evidence=$4 AND policy_rule=$5
		FROM platform_lifecycle.terms WHERE product_id=$1`, pilot.product, termBefore.Until, termBefore.Version, termBefore.Evidence, termBefore.Rule).Scan(&sameTerm))
	if !sameTerm {
		t.Fatal("catalog changed the previously issued term")
	}
	var slots int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_lifecycle.initial_term_policies WHERE product_id=$1`, pilot.product).Scan(&slots))
	if slots != 0 {
		t.Fatal("none left an operational trial slot for the previous writer")
	}
	accountID := "88221"
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,88221)`, pilot.integration)
	mustManager(t, err)
	freshInstall := wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Installed, Installation: uuid.New(), Integration: pilot.integration,
		Account: accountID, Sequence: 1, Generation: 1, OccurredAt: time.Now().UTC(), Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}}
	deliverProductEvent(t, pilot, core, second.private, freshInstall)
	var issued int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_lifecycle.terms t JOIN platform_lifecycle.accounts a ON a.id=t.account_id
		WHERE t.product_id=$1 AND a.amo_account_id=88221`, pilot.product).Scan(&issued))
	if issued != 0 {
		t.Fatal("disabled initial trial issued a term to a new account")
	}
	newMetrics := managerJSON[productMetrics](t, managerRequest(t, client, http.MethodGet, first.private+metricsPath, stats, managerTestCommand{}, "", http.StatusOK).body)
	if newMetrics.Installations != 3 || newMetrics.ActiveAccess != 1 {
		t.Fatalf("new none pair changed the current active term: %+v", newMetrics)
	}
	managerRequest(t, client, http.MethodPatch, second.private+productPath, noCatalog, noTrialCmd, noTrialBody, http.StatusForbidden)
	managerRequest(t, client, http.MethodGet, first.private+createCmd.receiptPath(), noCatalog, managerTestCommand{}, "", http.StatusForbidden)
	receipt := managerJSON[managerReceipt](t, managerRequest(t, client, http.MethodGet, first.private+createCmd.receiptPath(), editor, managerTestCommand{}, "", http.StatusOK).body)
	if receipt.Kind != productCreateCommandKind || receipt.State != "completed" {
		t.Fatalf("bad catalog receipt: %+v", receipt)
	}
	first.stop(t)
	first = startManagerProcess(t)
	replayed := managerRequest(t, client, http.MethodPatch, first.private+productPath, editor, changedCmd, changedBody, http.StatusOK)
	if !bytes.Equal(replayed.body, changed.body) || replayed.etag != changed.etag {
		t.Fatal("restart changed the exact catalog response")
	}
	replayedCreate := managerRequest(t, client, http.MethodPost, first.private+base, editor, createCmd, createBody, http.StatusCreated)
	if !bytes.Equal(replayedCreate.body, adopted.body) {
		t.Fatal("create replay drifted after catalog changed")
	}
	read := managerJSON[managerProduct](t, managerRequest(t, client, http.MethodGet, first.private+productPath, editor, managerTestCommand{}, "", http.StatusOK).body)
	if read.Version != 3 || read.Trial.Kind != "none" || read.Tariff != nil || read.Availability != "archived" {
		t.Fatalf("catalog did not persist: %+v", read)
	}

	beforeTruncate := managerJSON[productPage](t, managerRequest(t, client, http.MethodGet, first.private+base+"?limit=1",
		editor, managerTestCommand{}, "", http.StatusOK).body)
	if beforeTruncate.Next == nil {
		t.Fatal("catalog fixture did not have a continuation before owner truncate")
	}
	_, err = owner.Exec(ctx, `TRUNCATE platform_lifecycle.integrations CASCADE`)
	mustManager(t, err)
	managerRequest(t, client, http.MethodGet, second.private+base+"?limit=1&cursor="+url.QueryEscape(*beforeTruncate.Next),
		editor, managerTestCommand{}, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.integrations(id,product_id)
		VALUES($1,$2),($3,(SELECT id FROM platform_lifecycle.products WHERE code='other-review'))`,
		pilot.integration, pilot.product, pilot.otherIntegration)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id)
		VALUES($1,9007199254740993),($1,88221),($2,2542)`, pilot.integration, pilot.otherIntegration)
	mustManager(t, err)
	restored := managerJSON[managerProduct](t, managerRequest(t, client, http.MethodGet, first.private+productPath,
		editor, managerTestCommand{}, "", http.StatusOK).body)
	if len(restored.Integrations) != 1 || restored.Integrations[0] != pilot.integration {
		t.Fatal("owner fixture did not restore integration refs")
	}
	freeBody := `{"code":"free-category-test","display_name":"Free","line":"FREE","availability":"draft","integration_refs":[],"tariff_version":null,"trial_policy":{"kind":"free","duration_days":null},"links":[]}`
	freeProduct := managerJSON[managerProduct](t, managerRequest(t, client, http.MethodPost, first.private+base, editor, newManagerTestCommand(uuid.Nil, 0), freeBody, http.StatusCreated).body)
	var rule string
	mustManager(t, owner.QueryRow(ctx, `SELECT rule FROM platform_lifecycle.initial_term_policies WHERE product_id=$1`, freeProduct.ID).Scan(&rule))
	if rule != "free" {
		t.Fatalf("FREE creation selected %q", rule)
	}

}

func deliverProductEvent(t *testing.T, p managerPilot, client *http.Client, base string, e wire.Event) {
	t.Helper()
	raw, err := json.Marshal(e)
	mustManager(t, err)
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, base+coreEventsPath, bytes.NewReader(raw))
	mustManager(t, err)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	mustManager(t, err)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("lifecycle %s returned %d", e.Kind, response.StatusCode)
	}
}

func TestPlatformProductCreateReplayAfterBootstrap(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(name) == "" {
			t.Skip("run make platform-test for Platform manager process checks")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_lifecycle.manifest,platform_lifecycle.products,platform_lifecycle.accounts,
		platform_lifecycle.cursors,platform_lifecycle.inbox,platform_lifecycle.reconciliation,platform_manager.snapshots CASCADE`)
	mustManager(t, err)
	client := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "manager-bff")
	client.Timeout = 5 * time.Second
	first, second := startManagerProcess(t), startManagerProcess(t)
	editor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "catalog.manage"}}
	integration := uuid.New()
	cmd := newManagerTestCommand(uuid.Nil, 0)
	body := fmt.Sprintf(`{"code":"late-bootstrap","display_name":"Late product","line":"TOP","availability":"draft",
		"integration_refs":[%q],"tariff_version":null,"trial_policy":{"kind":"none","duration_days":null},"links":[]}`, integration)
	path := managerPath + "products"
	rejected := managerRequest(t, client, http.MethodPost, first.private+path, editor, cmd, body, http.StatusUnprocessableEntity)
	rejected.requireCode(t, "integration_mapping_mismatch")
	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: []wire.Mapping{
		{Integration: integration, ProductCode: "late-bootstrap", Accounts: []string{"2542"}},
	}})
	mustManager(t, err)
	mustManager(t, BootstrapLifecycle(ctx, owner, manifest))
	var id uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT id FROM platform_lifecycle.products WHERE code='late-bootstrap'`).Scan(&id))
	predicted := uuid.NewSHA1(uuid.NameSpaceURL, []byte("amocrm.platform.v1/products/late-bootstrap"))
	if id == predicted {
		t.Fatal("fixture did not install an independently assigned P03 product UUID")
	}
	replayed := managerRequest(t, client, http.MethodPost, second.private+path, editor, cmd, body, http.StatusUnprocessableEntity)
	if !bytes.Equal(replayed.body, rejected.body) {
		t.Fatalf("bootstrap changed the recorded rejection: %s / %s", rejected.body, replayed.body)
	}
	managerRequest(t, client, http.MethodPost, first.private+path, editor, cmd, strings.Replace(body, "Late product", "Changed product", 1), http.StatusConflict).
		requireCode(t, "idempotency_conflict")
	acceptedCmd := newManagerTestCommand(uuid.Nil, 0)
	accepted := managerRequest(t, client, http.MethodPost, first.private+path, editor, acceptedCmd, body, http.StatusCreated)
	if product := managerJSON[managerProduct](t, accepted.body); product.ID != id {
		t.Fatalf("new valid command did not adopt the P03 product: %s", accepted.body)
	}

	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET catalog_version=NULL,availability=NULL,links=NULL,
		tariff_version=NULL,catalog_policy_version=NULL WHERE id=$1`, id)
	mustManager(t, err)
	managerRequest(t, client, http.MethodGet, second.private+acceptedCmd.receiptPath(), editor, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, client, http.MethodPost, second.private+path, editor, acceptedCmd, body, http.StatusNotFound)
}

func TestManagerBindsTechnicalIntegration(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_BINARY") == "" {
		t.Skip("requires Platform manager process")
	}
	server := startManagerProcess(t)
	manager := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "manager-bff")
	core := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "core")
	editor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "catalog.manage"}}
	id := uuid.New()
	base := server.private + managerPath + "products"
	body := fmt.Sprintf(`{"code":"check-%s","display_name":"Тест","line":"FREE","availability":"pilot","integration_refs":[],"tariff_version":null,"trial_policy":{"kind":"none","duration_days":null},"links":[]}`, id)
	p := managerJSON[managerProduct](t, managerRequest(t, manager, http.MethodPost, base, editor, newManagerTestCommand(uuid.Nil, 0), body, http.StatusCreated).body)
	patch := fmt.Sprintf(`{"integration_refs":[%q]}`, id)
	managerRequest(t, manager, http.MethodPatch, base+"/"+p.ID.String(), editor, newManagerTestCommand(p.ID, 1), patch, http.StatusUnprocessableEntity).requireCode(t, "integration_mapping_mismatch")
	registration := fmt.Sprintf(`{"integration_id":%q,"active":true}`, id)
	res, err := core.Post(server.private+wire.CatalogPath, "application/json", strings.NewReader(registration))
	mustManager(t, err)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("register: %d", res.StatusCode)
	}
	updated := managerJSON[managerProduct](t, managerRequest(t, manager, http.MethodPatch, base+"/"+p.ID.String(), editor, newManagerTestCommand(p.ID, 1), patch, http.StatusOK).body)
	if len(updated.Integrations) != 1 || updated.Integrations[0] != id {
		t.Fatalf("incorrect binding: %v", updated.Integrations)
	}
}
