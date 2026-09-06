package main

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/componentruntime"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := componentruntime.RunStandalone(ctx, "activity"); err != nil {
		slog.Error("Activity stopped", "error", err)
		os.Exit(1)
	}
}
