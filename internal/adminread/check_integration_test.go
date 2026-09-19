package adminread

import (
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
	"time"
)

func TestVerificationReadUsesCurrentTerminalProjection(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := t.Context()
	integration := insertAdminIntegration(t, pool, "verification-fixture")
	id := uuid.New()
	now := time.Now().UTC()
	insertAdminInstallation(t, pool, id, integration, 91050505, "verification.amocrm.test", "active", `{}`, now)
	insertAdminCredentials(t, pool, id, now.Add(time.Hour))
	receipt := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO admin_commands(id,key_hash,request_hash,actor_id,target_type,target_id,command,state,outcome,installation_id,result)VALUES($1,decode(repeat('aa',32),'hex'),decode(repeat('bb',32),'hex'),'automation:fixture','installation',$2,'check','succeeded','verified_ok',$3,'{}')`, receipt, id.String(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO installation_checks(installation_id,receipt_id,credential_version,installation_status,classification,observed_at,next_check_at)SELECT $1,$2,token_version,'active','verified_ok',$3,$3 FROM oauth_credentials WHERE installation_id=$1`, id, receipt, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &store{pool: pool}
	card, err := store.getInstallation(ctx, id, now)
	if err != nil || card.Installation.AuthorizationCheck.Freshness != "fresh" || card.Authorization.Unverified {
		t.Fatalf("card=%+v err=%v", card, err)
	}
	// A newer pending command does not hide the latest terminal result.
	_, err = pool.Exec(ctx, `INSERT INTO admin_commands(id,key_hash,request_hash,actor_id,target_type,target_id,command,state,installation_id)VALUES($1,decode(repeat('cc',32),'hex'),decode(repeat('dd',32),'hex'),'automation:fixture','installation',$2,'check','pending',$3)`, uuid.New(), id.String(), id)
	if err != nil {
		t.Fatal(err)
	}
	cards, _, _, err := store.listAccounts(ctx, installationListFilter{Verification: "ok", Limit: 1})
	if err != nil || len(cards) != 1 {
		t.Fatalf("verified list=%v %v", cards, err)
	}
	_, err = pool.Exec(ctx, `UPDATE oauth_credentials SET token_version=token_version+1 WHERE installation_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	card, err = store.getInstallation(ctx, id, now)
	if err != nil || card.Installation.AuthorizationCheck.Freshness != "unknown" {
		t.Fatalf("new oauth retained old check %+v %v", card, err)
	}
	cards, _, _, err = store.listAccounts(ctx, installationListFilter{Verification: "unknown", Limit: 1})
	if err != nil || len(cards) != 1 {
		t.Fatalf("unknown list=%v %v", cards, err)
	}
}
