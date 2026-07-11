package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
)

const (
	downConfirmationEnvironment = "MIGRATION_DOWN_CONFIRM"
	downConfirmationValue       = "revert-all-migrations"
)

func main() {
	command, err := migrationCommand(os.Args[1:], os.Getenv)
	if errors.Is(err, errUsage) {
		fmt.Fprintln(os.Stderr, "usage: migrate [up|down]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg, err := config.LoadMigrate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	pool, err := postgres.Open(ctx, cfg.DatabaseURL, "amocrm-migrate", 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	started := time.Now()
	runner := migrations.New(pool, cfg.MigrationsDir)
	if command == "down" {
		err = runner.Down(ctx)
	} else {
		err = runner.Up(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("migrations %s completed in %s\n", command, time.Since(started).Round(time.Millisecond))
}

var errUsage = errors.New("invalid migration command")

func migrationCommand(arguments []string, getenv func(string) string) (string, error) {
	command := "up"
	if len(arguments) == 1 {
		command = arguments[0]
	}
	if len(arguments) > 1 || (command != "up" && command != "down") {
		return "", errUsage
	}
	if command == "down" && getenv(downConfirmationEnvironment) != downConfirmationValue {
		return "", fmt.Errorf(
			"migrate down refused: set %s=%s after following the rollback runbook",
			downConfirmationEnvironment, downConfirmationValue,
		)
	}
	return command, nil
}
