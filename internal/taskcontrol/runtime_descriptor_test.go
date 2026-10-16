package taskcontrol

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestRuntimeDescriptorRequiresExactInstallationGeneration(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	install, integration, company := uuid.New(), uuid.New(), uuid.New()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri) VALUES($1,'runtime-descriptor-test',$2,'synthetic',1,'https://example.test/callback')`, []any{integration, uuid.NewString()}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'task-control',true)`, []any{integration}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'tenant.amocrm.ru','active')`, []any{install, integration}},
		{`INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled) VALUES('descriptor-key',$1,123,$2,true)`, []any{company, integration}},
	}
	for _, q := range statements {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := Store{Pool: pool}
	read := func() Status {
		out, err := store.Read(ctx, "descriptor-key", company, 123, uuid.New(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := read(); out.RuntimeVersion != "" || len(out.RuntimeCapabilities) != 0 {
		t.Fatal("unconfirmed package advertised", out)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_control_runtime_descriptors(installation_id,generation,runtime_version,confirmed_by) VALUES($1,1,'1.2.0','test-operator')`, install); err != nil {
		t.Fatal(err)
	}
	if out := read(); out.RuntimeVersion != "1.2.0" || len(out.RuntimeCapabilities) != 1 || out.RuntimeCapabilities[0] != "dynamicRulesV2" {
		t.Fatal("confirmed package missing", out)
	}
	// A successful reinstall must not inherit package evidence from generation 1.
	for _, state := range []string{"uninstalled", "active"} {
		if _, err := pool.Exec(ctx, `UPDATE installations SET status=$2 WHERE id=$1`, install, state); err != nil {
			t.Fatal(err)
		}
	}
	if out := read(); out.Generation != 2 || out.RuntimeVersion != "" || len(out.RuntimeCapabilities) != 0 {
		t.Fatal("old deployment evidence survived reinstall", out)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_control_runtime_descriptors(installation_id,generation,runtime_version,confirmed_by) VALUES($1,2,'1.1.4','test-operator')`, install); err != nil {
		t.Fatal(err)
	}
	if out := read(); out.RuntimeVersion != "1.1.4" || len(out.RuntimeCapabilities) != 0 {
		t.Fatal(out)
	}

	if _, err := pool.Exec(ctx, `UPDATE task_control_runtime_descriptors SET runtime_version='1.2.1' WHERE installation_id=$1 AND generation=2`, install); err != nil {
		t.Fatal(err)
	}
	if out := read(); out.RuntimeVersion != "1.2.1" || len(out.RuntimeCapabilities) != 1 || out.RuntimeCapabilities[0] != "dynamicRulesV2" {
		t.Fatal("migration package lost v2 capability", out)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM task_control_runtime_descriptors WHERE installation_id=$1`, install); err != nil {
		t.Fatal(err)
	}
	if out := read(); out.RuntimeVersion != "" {
		t.Fatal("withdrawn descriptor still advertised", out)
	}
}
