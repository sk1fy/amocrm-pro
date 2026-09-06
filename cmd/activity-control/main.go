// activity-control operates Core-owned pilot admission and failed deliveries.
package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"os"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: activity-control pilot-enable|pilot-disable INSTALLATION_UUID | retry COMMAND_UUID")
	}
	if args[0] != "pilot-enable" && args[0] != "pilot-disable" && args[0] != "retry" {
		return fmt.Errorf("unknown operator command")
	}
	id, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("a valid UUID is required")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("Core DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, dsn, "activity-control", 1)
	if err != nil {
		return err
	}
	defer pool.Close()
	if args[0] == "retry" {
		err = activitybridge.RetryDelivery(ctx, pool, id)
	} else {
		err = activitybridge.SetPilot(ctx, pool, id, args[0] == "pilot-enable")
	}
	if err != nil {
		return err
	}
	fmt.Println("Core operator change committed and audited")
	return nil
}
