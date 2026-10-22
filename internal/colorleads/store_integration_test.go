package colorleads

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"os"
	"testing"
	"time"
)

type fixtureGuard struct{ deny bool }

func (g *fixtureGuard) Admit(_ context.Context, _ pgx.Tx, _ widgetauth.Principal, f Fence, _ bool) error {
	if g.deny || f.Epoch != 3 || f.Generation != 2 {
		return ErrRouteRejected
	}
	return nil
}
func (g *fixtureGuard) AdmitImport(context.Context, pgx.Tx, ImportCommand) error {
	if g.deny {
		return ErrRouteRejected
	}
	return nil
}

// This checks PostgreSQL persistence and atomic token/CAS boundaries with a
// fixture guard. It does NOT prove the separate PV-03 route-lock protocol.
func TestPostgresPersistenceCASAndImport(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DROP TABLE color_leads_imports,color_leads_documents`)
	integration, installation := uuid.New(), uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,'x','https://test/oauth')`, integration, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'test.amocrm.ru','active')`, installation, integration); err != nil {
		t.Fatal(err)
	}
	p := widgetauth.Principal{IntegrationID: integration, InstallationID: installation, AccountID: 123, UserID: 7, TokenID: uuid.NewString(), Issuer: "https://test.amocrm.ru", TokenRetainUntil: time.Now().Add(time.Minute)}
	g := &fixtureGuard{}
	s := NewStore(pool, g)
	f := Fence{Epoch: 3, Generation: 2}
	saved, err := s.WriteSettings(ctx, p, f, json.RawMessage(`{"main_settings":[]}`), 0)
	if err != nil || saved.Revision != 1 {
		t.Fatalf("save %+v %v", saved, err)
	}
	if _, err = s.WriteSettings(ctx, p, f, json.RawMessage(`{}`), 1); err != ErrUnauthorized {
		t.Fatalf("JWT replay: %v", err)
	}
	p.TokenID = uuid.NewString()
	if _, err = s.WriteSettings(ctx, p, f, json.RawMessage(`{}`), 0); err != ErrRevisionConflict {
		t.Fatalf("CAS: %v", err)
	}
	// Rejected CAS rolls back token consumption, allowing the corrected command.
	if _, err = s.WriteSettings(ctx, p, f, json.RawMessage(`{}`), 1); err != nil {
		t.Fatal(err)
	}
	p.TokenID = uuid.NewString()
	wrong := p
	wrong.AccountID = 999
	if _, err = s.ReadSettings(ctx, wrong, f); err != ErrUnauthorized {
		t.Fatalf("cross tenant %v", err)
	}
	g.deny = true
	if _, err = s.ReadSettings(ctx, p, f); err != ErrRouteRejected {
		t.Fatalf("route %v", err)
	}
	g.deny = false
	got, err := s.ReadSettings(ctx, p, f)
	if err != nil || got.Revision != 2 {
		t.Fatalf("read %+v %v", got, err)
	}
	c := ImportCommand{InstallationID: installation, Generation: 3, Epoch: 4, TransferID: "transfer", Actor: "operator", SourceDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Snapshot: Snapshot{Settings: json.RawMessage(`{}`), Colors: []Color{}}}
	g.deny = true
	if _, err = s.ImportSnapshot(ctx, g, c); err != ErrRouteRejected {
		t.Fatalf("denied import %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM color_leads_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied import persisted receipt: %d %v", count, err)
	}
	g.deny = false
	first, err := s.ImportSnapshot(ctx, g, c)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ImportSnapshot(ctx, g, c)
	if err != nil || !again.Replayed || first.Digest != again.Digest {
		t.Fatalf("import retry %+v %v", again, err)
	}
	oldSource := c.SourceDigest
	c.SourceDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err = s.ImportSnapshot(ctx, g, c); err != ErrRevisionConflict {
		t.Fatalf("source mismatch %v", err)
	}
	c.SourceDigest = oldSource
	c.Snapshot.Settings = json.RawMessage(`{"main_settings":[]}`)
	if _, err = s.ImportSnapshot(ctx, g, c); err != ErrRevisionConflict {
		t.Fatalf("import changed %v", err)
	}
}
