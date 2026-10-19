// task-control-runtime-descriptor records operator-confirmed package deployment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
)

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	f := flag.NewFlagSet("task-control-runtime-descriptor", flag.ContinueOnError)
	install := f.String("installation-id", "", "verified installation UUID")
	generation := f.Int64("generation", 0, "current verified generation")
	version := f.String("runtime-version", "", "operator-confirmed deployed package: 1.1.4|1.2.0|1.2.1|1.2.2|1.2.3|1.2.4; empty with --clear")
	actor := f.String("actor", "", "operator identity for audit")
	clear := f.Bool("clear", false, "withdraw deployment evidence")
	if f.Parse(args) != nil || f.NArg() != 0 || *generation <= 0 || strings.TrimSpace(*actor) == "" || len(*actor) > 200 || (!*clear && *version != "1.1.4" && *version != "1.2.0" && *version != "1.2.1" && *version != "1.2.2" && *version != "1.2.3" && *version != "1.2.4") || (*clear && *version != "") {
		return errors.New("required: --installation-id --generation --runtime-version 1.1.4|1.2.0|1.2.1|1.2.2|1.2.3|1.2.4 --actor; or --clear without --runtime-version")
	}
	id, err := uuid.Parse(*install)
	if err != nil || id == uuid.Nil {
		return errors.New("invalid installation UUID")
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
		return err
	}
	defer tx.Rollback(ctx)
	var current int64
	// Match status reader's lock order: installation before generation evidence.
	err = tx.QueryRow(ctx, `SELECT v.generation FROM installations i JOIN integration_services s ON s.integration_id=i.integration_id AND s.service_code='task-control' AND s.enabled JOIN task_control_installation_versions v ON v.installation_id=i.id WHERE i.id=$1 AND i.status='active' FOR UPDATE OF i,v`, id).Scan(&current)
	if err != nil || current != *generation {
		return errors.New("active Task Control installation and exact current generation required")
	}
	if *clear {
		_, err = tx.Exec(ctx, `DELETE FROM task_control_runtime_descriptors WHERE installation_id=$1 AND generation=$2`, id, current)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO task_control_runtime_descriptors(installation_id,generation,runtime_version,confirmed_by) VALUES($1,$2,$3,$4) ON CONFLICT(installation_id,generation) DO UPDATE SET runtime_version=excluded.runtime_version,confirmed_by=excluded.confirmed_by,confirmed_at=now()`, id, current, *version, *actor)
	}
	if err != nil {
		return errors.New("descriptor write rejected")
	}
	_, err = tx.Exec(ctx, `UPDATE task_control_installation_versions SET revision=revision+1 WHERE installation_id=$1`, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,metadata) VALUES($1,'operator',$2,'task-control.runtime_descriptor',jsonb_build_object('generation',$3::bigint,'runtime_version',$4::text,'cleared',$5::boolean))`, id, *actor, current, *version, *clear)
	if err != nil {
		return errors.New("audit write rejected")
	}
	if tx.Commit(ctx) != nil {
		return errors.New("descriptor commit uncertain; inspect database before retry")
	}
	fmt.Println("runtime descriptor applied")
	return nil
}
