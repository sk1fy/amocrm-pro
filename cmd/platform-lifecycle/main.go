package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

const usage = "usage: platform-lifecycle bootstrap-core|bootstrap-platform MANIFEST or replay INSTALLATION_UUID FIRST LAST or snapshot-delegation INSTALLATION_UUID INTEGRATION_UUID"

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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if len(args) < 2 {
		return errors.New(usage)
	}
	dsn := os.Getenv("DATABASE_URL")
	switch args[0] {
	case "bootstrap-core", "bootstrap-platform":
		if len(args) != 2 {
			return errors.New(usage)
		}
		file, err := os.Open(args[1])
		if err != nil {
			return errors.New("could not read pilot manifest")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, wire.MaxBody+1))
		if err != nil {
			return errors.New("could not read pilot manifest")
		}
		_, digest, err := wire.DecodeManifest(raw)
		if err != nil {
			return err
		}
		if args[0] == "bootstrap-platform" {
			owner, err := storage.Open(ctx, dsn, storage.Owner, 1)
			if err != nil {
				return err
			}
			defer owner.Close()
			if err := productplatform.BootstrapLifecycle(ctx, owner, raw); err != nil {
				return err
			}
		} else {
			pool, err := postgres.Open(ctx, dsn, "platform-lifecycle", 1)
			if err != nil {
				return errors.New("could not connect to Core")
			}
			defer pool.Close()
			if err := corelifecycle.NewStore(pool).Bootstrap(ctx, raw); err != nil {
				return err
			}
		}
		fmt.Println(digest)
		return nil
	case "replay", "snapshot-delegation":
		return coreOperation(ctx, dsn, args)
	default:
		return errors.New(usage)
	}
}

func coreOperation(ctx context.Context, dsn string, args []string) error {
	installation, err := uuid.Parse(args[1])
	if err != nil || installation == uuid.Nil {
		return errors.New("valid installation UUID required")
	}
	pool, err := postgres.Open(ctx, dsn, "platform-lifecycle", 1)
	if err != nil {
		return errors.New("could not connect to Core")
	}
	defer pool.Close()
	store := corelifecycle.NewStore(pool)
	if args[0] == "replay" {
		if len(args) != 4 {
			return errors.New(usage)
		}
		first, firstErr := strconv.ParseInt(args[2], 10, 64)
		last, lastErr := strconv.ParseInt(args[3], 10, 64)
		if firstErr != nil || lastErr != nil {
			return errors.New(usage)
		}
		return store.Replay(ctx, installation, first, last)
	}
	if len(args) != 3 {
		return errors.New(usage)
	}
	integration, err := uuid.Parse(args[2])
	if err != nil || integration == uuid.Nil {
		return errors.New("valid integration UUID required")
	}
	service, err := corelifecycle.NewSnapshotService(store, os.Getenv("PLATFORM_SNAPSHOT_KEY_FILE"))
	if err != nil {
		return err
	}
	delegation, err := service.Issue(ctx, installation, integration)
	if err != nil {
		return err
	}
	fmt.Println(delegation)
	return nil
}
