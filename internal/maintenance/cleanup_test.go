package maintenance

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type cleanerFunc func(context.Context, Policy) (Result, error)

func (function cleanerFunc) Cleanup(ctx context.Context, policy Policy) (Result, error) {
	return function(ctx, policy)
}

func TestSchedulerRunsAtStartupAndPeriodically(t *testing.T) {
	var calls atomic.Int32
	called := make(chan struct{}, 4)
	cleaner := cleanerFunc(func(context.Context, Policy) (Result, error) {
		calls.Add(1)
		called <- struct{}{}
		return Result{LockAcquired: true}, nil
	})
	scheduler, err := NewScheduler(cleaner, slog.New(slog.NewTextHandler(io.Discard, nil)), SchedulerConfig{
		Interval: 10 * time.Millisecond,
		Timeout:  time.Second,
		Policy:   testPolicy(10, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	for index := 0; index < 2; index++ {
		select {
		case <-called:
		case <-time.After(time.Second):
			t.Fatal("scheduler did not run")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	if calls.Load() < 2 {
		t.Fatalf("cleanup calls = %d, want at least 2", calls.Load())
	}
}

func TestSchedulerContinuesAfterCleanupError(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	cleaner := cleanerFunc(func(context.Context, Policy) (Result, error) {
		if calls.Add(1) == 1 {
			return Result{}, errors.New("temporary database error")
		}
		select {
		case <-second:
		default:
			close(second)
		}
		return Result{LockAcquired: true}, nil
	})
	scheduler, err := NewScheduler(cleaner, slog.New(slog.NewTextHandler(io.Discard, nil)), SchedulerConfig{
		Interval: 10 * time.Millisecond, Timeout: time.Second,
		Policy: testPolicy(1, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not retry after error")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerConfigurationValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleaner := cleanerFunc(func(context.Context, Policy) (Result, error) { return Result{}, nil })
	tests := []SchedulerConfig{
		{Timeout: time.Second, Policy: testPolicy(1, 1)},
		{Interval: time.Second, Policy: testPolicy(1, 1)},
		{Interval: time.Second, Timeout: time.Second, Policy: Policy{SafetyMargin: -1, WebhookInboxRetention: time.Hour, WebhookDeliveryRetention: time.Hour, BatchSize: 1, MaxBatches: 1}},
		{Interval: time.Second, Timeout: time.Second, Policy: Policy{WebhookInboxRetention: time.Hour, WebhookDeliveryRetention: time.Hour, MaxBatches: 1}},
		{Interval: time.Second, Timeout: time.Second, Policy: Policy{WebhookInboxRetention: time.Hour, WebhookDeliveryRetention: time.Hour, BatchSize: 1}},
	}
	for _, config := range tests {
		if _, err := NewScheduler(cleaner, logger, config); err == nil {
			t.Fatalf("NewScheduler(%+v) unexpectedly succeeded", config)
		}
	}
}

func testPolicy(batchSize, maxBatches int) Policy {
	return Policy{
		WebhookInboxRetention: time.Hour, WebhookDeliveryRetention: time.Hour,
		BatchSize: batchSize, MaxBatches: maxBatches,
	}
}
