package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusRetry      Status = "retry"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusDead       Status = "dead"
	StatusCancelled  Status = "cancelled"
)

type Job struct {
	ID               uuid.UUID
	InstallationID   *uuid.UUID
	Type             string
	ActorType        *string
	ActorID          *string
	ResourceType     *string
	ResourceID       *string
	Status           Status
	Priority         int16
	Payload          json.RawMessage
	Result           json.RawMessage
	Attempts         int
	MaxAttempts      int
	RunAfter         time.Time
	LockedBy         *string
	LockedUntil      *time.Time
	LastErrorCode    *string
	LastErrorMessage *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       *time.Time
}

type EnqueueParams struct {
	InstallationID *uuid.UUID
	Type           string
	ActorType      string
	ActorID        string
	ResourceType   string
	ResourceID     string
	Priority       int16
	Payload        any
	MaxAttempts    int
	RunAfter       time.Time
}

type Handler func(context.Context, Job) (json.RawMessage, error)

type TxExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type FailureObserver func(context.Context, TxExecutor, Job, Failure, Status) error

type Error struct {
	Code       string
	Retryable  bool
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func Permanent(code string, err error) error {
	return &Error{Code: code, Retryable: false, Err: err}
}

func Retryable(code string, retryAfter time.Duration, err error) error {
	return &Error{Code: code, Retryable: true, RetryAfter: retryAfter, Err: err}
}

type Failure struct {
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func Classify(err error, attempt int) Failure {
	if err == nil {
		return Failure{}
	}

	failure := Failure{
		Code:       "internal_error",
		Message:    err.Error(),
		Retryable:  true,
		RetryAfter: Backoff(attempt, 5*time.Second, 10*time.Minute),
	}

	var jobErr *Error
	if errors.As(err, &jobErr) {
		failure.Code = jobErr.Code
		failure.Retryable = jobErr.Retryable
		if jobErr.RetryAfter > 0 {
			failure.RetryAfter = jobErr.RetryAfter
		}
	}
	if !failure.Retryable {
		failure.RetryAfter = 0
	}
	return failure
}

func Backoff(attempt int, base, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 8)
	delay := base * time.Duration(1<<shift)
	if delay > maximum {
		delay = maximum
	}
	jitterRange := max(delay/4, time.Nanosecond)
	jitter := time.Duration(rand.Int64N(int64(jitterRange)))
	if delay+jitter > maximum {
		return maximum
	}
	return delay + jitter
}

func validateEnqueue(params EnqueueParams) error {
	if params.Type == "" {
		return errors.New("job type is required")
	}
	if params.MaxAttempts < 0 {
		return fmt.Errorf("max attempts cannot be negative")
	}
	if (params.ActorType == "") != (params.ActorID == "") {
		return errors.New("actor type and id must be provided together")
	}
	if (params.ResourceType == "") != (params.ResourceID == "") {
		return errors.New("resource type and id must be provided together")
	}
	return nil
}
