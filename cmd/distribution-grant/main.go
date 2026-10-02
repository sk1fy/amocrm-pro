// distribution-grant provisions one scoped server identity. Secrets are supplied
// separately by deployment; this audited command never reads or prints them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"os"
	"regexp"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	f := flag.NewFlagSet("distribution-grant", flag.ContinueOnError)
	kid := f.String("key-id", "", "")
	company := f.String("company-id", "", "")
	install := f.String("installation-id", "", "")
	actor := f.String("actor", "", "")
	enabled := f.String("enabled", "", "")
	if f.Parse(args) != nil || f.NArg() != 0 || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(*kid) || *actor == "" || len(*actor) > 200 || (*enabled != "true" && *enabled != "false") {
		return errors.New("required: --key-id --company-id --installation-id --actor --enabled true|false")
	}
	cid, ce := uuid.Parse(*company)
	iid, ie := uuid.Parse(*install)
	if ce != nil || ie != nil || cid == uuid.Nil || iid == uuid.Nil {
		return errors.New("invalid scope UUID")
	}
	cfg, err := config.LoadOperator()
	if err != nil {
		return errors.New("invalid operator environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.ServiceName, 1)
	if err != nil {
		return errors.New("operator database unavailable")
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("operator database unavailable")
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES($1,$2,$3,$4) ON CONFLICT(key_id,company_id,installation_id) DO UPDATE SET enabled=excluded.enabled`, *kid, cid, iid, *enabled == "true")
	if err != nil {
		return errors.New("scope grant rejected")
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,metadata) VALUES($1,'operator',$2,'distribution.service_grant',jsonb_build_object('key_id',$3::text,'company_id',$4::text,'enabled',$5::boolean))`, iid, *actor, *kid, cid.String(), *enabled == "true")
	if err != nil {
		return errors.New("audit write rejected")
	}
	if tx.Commit(ctx) != nil {
		return errors.New("scope grant commit uncertain; inspect database before retry")
	}
	fmt.Println("scope grant applied")
	return nil
}
