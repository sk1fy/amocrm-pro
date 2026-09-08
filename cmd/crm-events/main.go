package main

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
	"github.com/sk1fy/amocrm-pro/internal/componentruntime"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := componentruntime.RunStandalone(ctx, "crm-events"); err != nil {
		slog.Error("CRM Events stopped", "error", err)
		os.Exit(1)
	}
}
