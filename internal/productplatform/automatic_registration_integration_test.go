package productplatform_test

import (
	"bytes"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogInstallationBeforeActivation(t *testing.T) {
	ctx := t.Context()
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	if certs == "" {
		t.Skip("requires local Core and Platform databases and service certificates")
	}
	core := testkit.Postgres(t)
	testkit.Reset(t, core)
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	require(t, err)
	defer owner.Close()
	writer, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	require(t, err)
	defer writer.Close()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{3}, 32)}, 1)
	require(t, err)
	o := oauth.NewStore(core, keys)
	integration := createIntegration(t, ctx, o, "catalog-new")
	journal := corelifecycle.NewStore(core)
	server := startInbox(t, productplatform.NewLifecycleStore(writer), certs)
	client, err := corelifecycle.NewCatalogClient(server, filepath.Join(certs, "core"))
	require(t, err)
	defer client.Close()
	activate, err := corelifecycle.NewActivationClient(server, filepath.Join(certs, "core"))
	require(t, err)
	defer activate.Close()
	delivery, err := corelifecycle.DeliveryHandler(journal, server, filepath.Join(certs, "core"))
	require(t, err)
	const earlier int64 = 613456
	old, err := o.SaveInstallation(ctx, integration, oauth.Account{ID: earlier, CurrentUserID: 765}, "sample.amocrm.ru", credential())
	require(t, err)
	_, err = o.SaveInstallation(ctx, integration, oauth.Account{ID: earlier, CurrentUserID: 999}, "sample.amocrm.ru", credential())
	require(t, err)
	require(t, journal.SyncCatalog(ctx, client, nil))
	assertCount(t, ctx, owner, 1, "SELECT count(*) FROM platform_lifecycle.core_integrations WHERE id=$1 AND active", integration.ID)
	product := uuid.New()
	_, err = owner.Exec(ctx, "INSERT INTO platform_lifecycle.products(id,code,display_name,line) VALUES($1,$2,'Test','FREE')", product, "test-"+product.String())
	require(t, err)
	_, err = owner.Exec(ctx, "INSERT INTO platform_lifecycle.product_trial_revisions(product_id,version,rule) VALUES($1,1,'none')", product)
	require(t, err)
	_, err = owner.Exec(ctx, "UPDATE platform_lifecycle.products SET catalog_version=1,catalog_policy_version=1,availability='pilot',links='[]' WHERE id=$1", product)
	require(t, err)
	_, err = owner.Exec(ctx, "INSERT INTO platform_lifecycle.integrations(id,product_id) VALUES($1,$2)", integration.ID, product)
	require(t, err)
	require(t, journal.SyncCatalog(ctx, client, nil))
	require(t, journal.SyncCatalog(ctx, client, nil))
	current, err := o.SaveInstallation(ctx, integration, oauth.Account{ID: earlier + 1, CurrentUserID: 432}, "another.amocrm.ru", credential())
	require(t, err)
	assertCount(t, ctx, core, 765, "SELECT installed_by FROM installations WHERE id=$1", old.ID)
	assertCount(t, ctx, core, 432, "SELECT installed_by FROM installations WHERE id=$1", current.ID)
	for _, installation := range []oauth.InstallationResult{old, current} {
		sequence := 1
		if installation.ID == old.ID {
			sequence = 2
		}
		for seq := 1; seq <= sequence; seq++ {
			var jid uuid.UUID
			require(t, core.QueryRow(ctx, `SELECT j.id FROM jobs j JOIN platform_lifecycle_ledger l ON l.event_id::text=j.resource_id
                WHERE j.type=$1 AND j.installation_id=$2 AND l.sequence=$3`, corelifecycle.JobType, installation.ID, seq).Scan(&jid))
			job, err := jobs.NewStore(core).GetForInstallation(ctx, jid, installation.ID)
			require(t, err)
			_, err = delivery(ctx, job)
			require(t, err)
			_, err = delivery(ctx, job)
			require(t, err)
		}
		assertCount(t, ctx, owner, 1, "SELECT count(*) FROM platform_manager.installation_directory WHERE installation_id=$1 AND reviewed_at IS NULL", installation.ID)
		p := widgetauth.Principal{IntegrationID: integration.ID, InstallationID: installation.ID, AccountID: installation.AccountID}
		ok, err := journal.CanUseWidget(ctx, p, client, activate)
		require(t, err)
		if ok {
			t.Fatal("installation granted work before activation")
		}
		scope := wire.ActivationScope{Installation: p.InstallationID, Integration: p.IntegrationID, Account: p.AccountID, Generation: 1, AuthorizationSequence: int64(sequence)}
		for range 2 {
			result, err := activate.Apply(ctx, wire.ActivationRequest{Scope: scope, Contact: &wire.ActivationContact{UserID: 12, Name: "Test", Phone: "+79991112233", Consent: wire.ContactConsent}})
			require(t, err)
			if !result.Activated {
				t.Fatal("missing contact")
			}
		}
		require(t, journal.SyncCatalog(ctx, client, nil))
		assertCount(t, ctx, core, 1, "SELECT count(*) FROM widget_activation_confirmations WHERE installation_id=$1 AND generation=1", installation.ID)
		assertCount(t, ctx, owner, 1, "SELECT count(*) FROM platform_lifecycle.episodes WHERE installation_id=$1 AND installed_by IS NOT NULL", installation.ID)
		ok, err = journal.CanUseWidget(ctx, p, client, activate)
		require(t, err)
		if !ok {
			t.Fatal("activation did not open access")
		}
		assertCount(t, ctx, owner, 1, "SELECT count(*) FROM platform_lifecycle.episodes e JOIN platform_manager.installation_contacts c ON c.episode_id=e.id WHERE e.installation_id=$1 AND reviewed_at IS NULL", installation.ID)
	}
}
