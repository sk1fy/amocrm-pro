package crmevents

import (
	"context"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestEnrichmentClaimRespectsBatchLimitAtVolume(t *testing.T) {
	pool := eventsPool(t)
	store := NewPostgres(pool, DefaultConfig())
	src := rel02InsertSource(t, pool, time.Now().UTC())
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,object_id,state) SELECT $1,'task',n::text,n,'pending' FROM generate_series(1,200) n`, src.Principal.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimEnrichment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Objects) != serviceapi.EnrichmentBatchLimit {
		t.Fatalf("claim returned %d objects, want bounded batch %d", len(claim.Objects), serviceapi.EnrichmentBatchLimit)
	}
}
