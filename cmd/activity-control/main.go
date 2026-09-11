// activity-control operates Core-owned pilot admission and failed deliveries.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
	"os"
	"strings"
	"time"
)

const usage = "usage: activity-control list | inspect|retry COMMAND_UUID | pilot-enable|pilot-disable INSTALLATION_UUID | panel-list|panel-create|panel-get|panel-patch|panel-rotate|panel-employees ..."

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
	if len(args) > 0 && strings.HasPrefix(args[0], "panel-") {
		return runPanel(args)
	}
	command, id, err := parseArgs(args)
	if err != nil {
		return err
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
	switch command {
	case "list":
		rows, err := activitybridge.ListDeliveries(ctx, pool)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "inspect":
		row, err := activitybridge.InspectDelivery(ctx, pool, id)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(row)
	case "retry":
		err = activitybridge.RetryDelivery(ctx, pool, id)
	default:
		err = activitybridge.SetPilot(ctx, pool, id, command == "pilot-enable")
	}
	if err != nil {
		return err
	}
	fmt.Println("Core operator change committed and audited")
	return nil
}

func parseArgs(args []string) (string, uuid.UUID, error) {
	if len(args) == 0 {
		return "", uuid.Nil, errors.New(usage)
	}
	command := args[0]
	switch command {
	case "list":
		if len(args) != 1 {
			return "", uuid.Nil, errors.New(usage)
		}
		return command, uuid.Nil, nil
	case "inspect", "retry", "pilot-enable", "pilot-disable":
		if len(args) != 2 {
			return "", uuid.Nil, errors.New(usage)
		}
		id, err := uuid.Parse(args[1])
		if err != nil {
			return "", uuid.Nil, fmt.Errorf("a valid UUID is required")
		}
		return command, id, nil
	default:
		return "", uuid.Nil, errors.New("unknown operator command")
	}
}
