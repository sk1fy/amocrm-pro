package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

func New(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func Run(ctx context.Context, server *http.Server, logger *slog.Logger, shutdownTimeout time.Duration) error {
	serverError := make(chan error, 1)
	go func() {
		logger.Info("http server started", "address", server.Addr)
		serverError <- server.ListenAndServe()
	}()

	select {
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}
	logger.Info("http server stopped")
	return nil
}

// RunAll supervises independent listeners as one process. The first listener
// exit cancels the others, then every listener is allowed to shut down before
// the first non-cancellation error is returned.
func RunAll(
	ctx context.Context,
	logger *slog.Logger,
	shutdownTimeout time.Duration,
	servers ...*http.Server,
) error {
	if len(servers) == 0 {
		return errors.New("at least one HTTP server is required")
	}
	groupContext, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan error, len(servers))
	for _, server := range servers {
		server := server
		go func() {
			results <- Run(groupContext, server, logger, shutdownTimeout)
		}()
	}

	errorsSeen := make([]error, 0, len(servers))
	errorsSeen = append(errorsSeen, <-results)
	cancel()
	for range len(servers) - 1 {
		errorsSeen = append(errorsSeen, <-results)
	}
	for _, err := range errorsSeen {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}
