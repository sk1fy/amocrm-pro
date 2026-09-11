package componentruntime

import "testing"

func TestLoadPreservesStaticAddressesAndRejectsForeignDSN(t *testing.T) {
	for _, key := range []string{
		"DATABASE_URL", "ENCRYPTION_KEYS", "AMOCRM_CLIENT_SECRET",
		"ACTIVITY_DATABASE_URL", "CRM_EVENTS_DATABASE_URL", "ACTIVITY_MODE",
		"GATEWAY_ADDRESS", "ACTIVITY_ADDRESS", "CRM_EVENTS_ADDRESS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("ACTIVITY_MODE", "grpc")
	t.Setenv("ACTIVITY_DATABASE_URL", "postgres://activity_runtime@activity-postgres/activity_owners_source_test")
	cfg, err := Load("activity")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayAddress != "worker:9090" || cfg.ActivityAddress != "activity:9091" || cfg.EventsAddress != "crm-events:9092" {
		t.Fatalf("expected static host:port defaults, got gateway=%q activity=%q events=%q", cfg.GatewayAddress, cfg.ActivityAddress, cfg.EventsAddress)
	}

	t.Setenv("GATEWAY_ADDRESS", "worker.core.test:9090")
	t.Setenv("ACTIVITY_ADDRESS", "activity.product.test:9091")
	t.Setenv("CRM_EVENTS_ADDRESS", "crm-events.product.test:9092")
	cfg, err = Load("activity")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayAddress != "worker.core.test:9090" || cfg.EventsAddress != "crm-events.product.test:9092" || cfg.ActivityAddress != "activity.product.test:9091" {
		t.Fatalf("static transfer addresses were not preserved: %+v", cfg)
	}

	t.Setenv("CRM_EVENTS_DATABASE_URL", "postgres://events_runtime@events-postgres-target/events_transfer_restored_test")
	if _, err := Load("activity"); err == nil {
		t.Fatal("Activity accepted a foreign CRM Events DSN after address change")
	}
	t.Setenv("CRM_EVENTS_DATABASE_URL", "")
	t.Setenv("ACTIVITY_DATABASE_URL", "postgres://activity_runtime@activity-postgres-target/activity_owners_restored_test")
	t.Setenv("CRM_EVENTS_DATABASE_URL", "postgres://events_runtime@events-postgres-target/events_transfer_restored_test")
	if _, err := Load("api"); err == nil {
		t.Fatal("Core grpc accepted product DSNs pointed at transfer aliases")
	}
}
