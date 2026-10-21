package distribution

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type parallelReferencesCRM struct {
	crmFake
	installation uuid.UUID
	started      chan string
	release      chan struct{}
	failUsers    bool
	canceled     atomic.Int32
}

func (f *parallelReferencesCRM) wait(ctx context.Context, installation uuid.UUID, stage string) error {
	if installation != f.installation {
		return errors.New("wrong installation")
	}
	f.started <- stage
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.release:
	}
	if f.failUsers {
		if stage == "users" {
			return amocrm.ErrTransport
		}
		<-ctx.Done()
		f.canceled.Add(1)
		return ctx.Err()
	}
	return nil
}

func (f *parallelReferencesCRM) DistributionAccountTimezone(ctx context.Context, installation uuid.UUID, account int64) (string, error) {
	if account != 123 {
		return "", errors.New("wrong account")
	}
	return "UTC", f.wait(ctx, installation, "timezone")
}
func (f *parallelReferencesCRM) DistributionUsers(ctx context.Context, installation uuid.UUID) ([]amocrm.DistributionUser, error) {
	return []amocrm.DistributionUser{{ID: 7}}, f.wait(ctx, installation, "users")
}
func (f *parallelReferencesCRM) DistributionPipelines(ctx context.Context, installation uuid.UUID) ([]amocrm.Pipeline, error) {
	return []amocrm.Pipeline{{ID: 9}}, f.wait(ctx, installation, "pipelines")
}

func TestReferenceSourcesParallelAndCancelWithoutPartialResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "parallel reads", true: "cancel siblings"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			f := &parallelReferencesCRM{installation: uuid.New(), started: make(chan string, 3), release: make(chan struct{}), failUsers: fail}
			type result struct {
				data referenceSources
				err  error
			}
			done := make(chan result, 1)
			go func() {
				data, err := readReferenceSources(ctx, f, Binding{InstallationID: f.installation, AccountID: 123})
				done <- result{data, err}
			}()
			// All reads must begin before any source is released. A sequential
			// implementation cannot pass this barrier, regardless of machine speed.
			seen := map[string]bool{}
			for len(seen) < 3 {
				select {
				case stage := <-f.started:
					seen[stage] = true
				case <-ctx.Done():
					t.Fatal("reference reads did not start concurrently")
				}
			}
			close(f.release)
			got := <-done
			if fail {
				if !errors.Is(got.err, amocrm.ErrTransport) || got.data.timezone != "" || got.data.users != nil || got.data.pipelines != nil || f.canceled.Load() != 2 {
					t.Fatalf("failure returned partial data or left reads running: %+v", got)
				}
			} else if got.err != nil || got.data.timezone != "UTC" || got.data.timezoneFetchedAt.IsZero() || len(got.data.users) != 1 || len(got.data.pipelines) != 1 {
				t.Fatalf("incomplete reference result: %+v", got)
			}
		})
	}
}
