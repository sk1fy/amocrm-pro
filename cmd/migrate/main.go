package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/platform/config"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/platform/postgres"
)

func main() {
	command := "up"
	if len(os.Args) == 2 {
		command = os.Args[1]
	}
	if len(os.Args) > 2 || (command != "up" && command != "down") {
		fmt.Fprintln(os.Stderr, "usage: migrate [up|down]")
		os.Exit(2)
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
