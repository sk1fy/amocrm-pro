package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunAllPropagatesListenerFailureAndStopsPeers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	peer := New("127.0.0.1:0", http.NotFoundHandler())
	broken := New("127.0.0.1:invalid", http.NotFoundHandler())

	err := RunAll(context.Background(), logger, time.Second, peer, broken)
	if err == nil || !strings.Contains(err.Error(), "serve http") {
		t.Fatalf("expected listener failure, got %v", err)
	}
}

func TestRunAllRequiresServer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := RunAll(context.Background(), logger, time.Second); err == nil {
		t.Fatal("expected empty server group rejection")
	}
}
