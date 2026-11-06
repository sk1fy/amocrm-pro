package admincrm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
)

type fakeResolver struct {
	mu         sync.Mutex
	connection Connection
	err        error
}

func (f *fakeResolver) ResolveConnection(context.Context, int64) (Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connection, f.err
}

func (f *fakeResolver) set(connection Connection, err error) {
	f.mu.Lock()
	f.connection, f.err = connection, err
	f.mu.Unlock()
}

type fakeReader struct {
	calls atomic.Int32
	gate  chan struct{}
	users []amocrm.CRMUser
	err   error
}

type readerFunc func(context.Context, uuid.UUID) ([]amocrm.CRMUser, error)

func (f readerFunc) CRMUsers(ctx context.Context, id uuid.UUID) ([]amocrm.CRMUser, error) {
	return f(ctx, id)
}

func (f *fakeReader) CRMUsers(ctx context.Context, _ uuid.UUID) ([]amocrm.CRMUser, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.users, f.err
}

func crmUser(id int64, name string, active, admin bool) amocrm.CRMUser {
	group, role := int64(7), int64(9)
	return amocrm.CRMUser{ID: id, Name: name, Rights: amocrm.CRMRights{
		IsAdmin: admin, IsActive: &active, GroupID: &group, RoleID: &role,
		Leads: map[string]string{"view": "A"},
	}}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testConnection() Connection {
	return Connection{InstallationID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), IntegrationID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), IntegrationCode: "widget-fixture-a", credentialVersion: 1}
}

func TestUsersProjectsSortsAndCaches(t *testing.T) {
	now := &clock{now: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}
	reader := &fakeReader{users: []amocrm.CRMUser{crmUser(30, "Третий", false, false), crmUser(10, "Первый", true, true)}}
	service := &Service{Resolver: &fakeResolver{connection: testConnection()}, CRM: reader, Now: now.Now}

	first, err := service.Users(t.Context(), 31415926)
	if err != nil {
		t.Fatal(err)
	}
	want := []User{{ID: 10, Name: "Первый", IsActive: true, IsAdmin: true}, {ID: 30, Name: "Третий"}}
	if fmt.Sprint(first.Users) != fmt.Sprint(want) {
		t.Fatalf("users=%+v", first.Users)
	}
	if !first.FetchedAt.Equal(now.Now()) || !first.FreshUntil.Equal(now.Now().Add(FreshFor)) || first.Connection != testConnection() {
		t.Fatalf("snapshot=%+v", first)
	}
	first.Users[0].Name = "mutated"

	now.Add(FreshFor - time.Second)
	second, err := service.Users(t.Context(), 31415926)
	if err != nil {
		t.Fatal(err)
	}
	if reader.calls.Load() != 1 || second.Users[0].Name != "Первый" {
		t.Fatalf("calls=%d users=%+v", reader.calls.Load(), second.Users)
	}

	now.Add(time.Second)
	if _, err := service.Users(t.Context(), 31415926); err != nil {
		t.Fatal(err)
	}
	if reader.calls.Load() != 2 {
		t.Fatalf("expired snapshot must be read again, calls=%d", reader.calls.Load())
	}
}

func TestUsersDoesNotServeCacheOfAnotherConnection(t *testing.T) {
	resolver := &fakeResolver{connection: testConnection()}
	reader := &fakeReader{users: []amocrm.CRMUser{crmUser(1, "Один", true, false)}}
	service := &Service{Resolver: resolver, CRM: reader}
	if _, err := service.Users(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	other := testConnection()
	other.InstallationID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	resolver.set(other, nil)
	snapshot, err := service.Users(t.Context(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if reader.calls.Load() != 2 || snapshot.Connection != other {
		t.Fatalf("calls=%d connection=%+v", reader.calls.Load(), snapshot.Connection)
	}

	resolver.set(Connection{}, ErrNoAuthorizedConnection)
	if _, err := service.Users(t.Context(), 5); !errors.Is(err, ErrNoAuthorizedConnection) {
		t.Fatalf("revoked connection must not serve cache: %v", err)
	}
}

func TestUsersErrorsAreClassifiedAndNotCached(t *testing.T) {
	reader := &fakeReader{err: &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429, RetryAfter: 3 * time.Second}}
	service := &Service{Resolver: &fakeResolver{connection: testConnection()}, CRM: reader}
	_, err := service.Users(t.Context(), 9)
	var readError *ReadError
	if !errors.As(err, &readError) || readError.State != StateRateLimited || readError.RetryAfter != 3*time.Second || readError.Connection != testConnection() {
		t.Fatalf("err=%#v", err)
	}
	reader.err = nil
	reader.users = []amocrm.CRMUser{}
	snapshot, err := service.Users(t.Context(), 9)
	if err != nil || len(snapshot.Users) != 0 || snapshot.Users == nil || reader.calls.Load() != 2 {
		t.Fatalf("snapshot=%+v err=%v calls=%d", snapshot, err, reader.calls.Load())
	}
}

func TestUsersSharesOneReadAndSurvivesCallerCancel(t *testing.T) {
	reader := &fakeReader{gate: make(chan struct{}), users: []amocrm.CRMUser{crmUser(1, "Один", true, false)}}
	service := &Service{Resolver: &fakeResolver{connection: testConnection()}, CRM: reader}
	cancelled, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Users(cancelled, 1)
		firstDone <- err
	}()
	for reader.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Users(t.Context(), 1)
		secondDone <- err
	}()
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first=%v", err)
	}
	close(reader.gate)
	if err := <-secondDone; err != nil {
		t.Fatalf("second caller must receive the shared read: %v", err)
	}
	if reader.calls.Load() != 1 {
		t.Fatalf("calls=%d", reader.calls.Load())
	}
}

func TestUsersDoesNotJoinReadFromPreviousCredentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		connection := testConnection()
		resolver := &fakeResolver{connection: connection}
		release := make(chan struct{})
		var calls atomic.Int32
		reader := readerFunc(func(ctx context.Context, _ uuid.UUID) ([]amocrm.CRMUser, error) {
			if calls.Add(1) == 1 {
				select {
				case <-release:
					return []amocrm.CRMUser{crmUser(1, "До обновления", true, false)}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []amocrm.CRMUser{crmUser(2, "После обновления", true, false)}, nil
		})
		service := &Service{Resolver: resolver, CRM: reader}
		assertNewUsers := func(snapshot Snapshot, err error) {
			t.Helper()
			if err != nil || len(snapshot.Users) != 1 || snapshot.Users[0].ID != 2 {
				t.Errorf("users=%+v err=%v", snapshot.Users, err)
			}
		}
		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			assertNewUsers(service.Users(t.Context(), 1))
		}()
		synctest.Wait()
		connection.credentialVersion++
		resolver.set(connection, nil)
		assertNewUsers(service.Users(t.Context(), 1))
		close(release)
		<-firstDone
		assertNewUsers(service.Users(t.Context(), 1))
		if calls.Load() != 2 {
			t.Fatalf("CRM reads=%d, want one per credential version", calls.Load())
		}
	})
}

func TestUsersCacheIsBounded(t *testing.T) {
	now := &clock{now: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}
	service := &Service{Resolver: &fakeResolver{connection: testConnection()}, CRM: &fakeReader{users: []amocrm.CRMUser{}}, Now: now.Now}
	for id := int64(1); id <= maxCacheEntries+5; id++ {
		now.Add(time.Millisecond)
		if _, err := service.Users(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	size := len(service.cache)
	_, oldest := service.cache[1]
	_, newest := service.cache[maxCacheEntries+5]
	service.mu.Unlock()
	if size != maxCacheEntries || oldest || !newest {
		t.Fatalf("size=%d oldest=%v newest=%v", size, oldest, newest)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state string
		after time.Duration
	}{
		{"unauthorized", &amocrm.APIError{Kind: amocrm.ErrorUnauthorized, StatusCode: 401}, StateAuthError, 0},
		{"forbidden", &amocrm.APIError{Kind: amocrm.ErrorForbidden, StatusCode: 403}, StateAuthError, 0},
		{"rate limited", &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429, RetryAfter: 2 * time.Second}, StateRateLimited, 2 * time.Second},
		{"overloaded", &amocrm.APIError{Kind: amocrm.ErrorOverloaded}, StateRateLimited, 0},
		{"temporary", &amocrm.APIError{Kind: amocrm.ErrorTemporary, StatusCode: 503}, StateNetworkError, 0},
		{"payment", &amocrm.APIError{Kind: amocrm.ErrorPayment, StatusCode: 402}, StateInternalError, 0},
		{"refresh unknown", fmt.Errorf("token: %w", oauth.ErrRefreshOutcomeUnknown), StateAuthError, 0},
		{"credentials gone", fmt.Errorf("token: %w", pgx.ErrNoRows), StateAuthError, 0},
		{"incomplete", amocrm.ErrIncompleteResponse, StateIncomplete, 0},
		{"too large", amocrm.ErrResponseTooLarge, StateIncomplete, 0},
		{"transport", fmt.Errorf("do: %w", amocrm.ErrTransport), StateNetworkError, 0},
		{"deadline", context.DeadlineExceeded, StateNetworkError, 0},
		{"other", errors.New("decrypt failed"), StateInternalError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, after := Classify(tc.err)
			if state != tc.state || after != tc.after {
				t.Fatalf("state=%s after=%s", state, after)
			}
		})
	}
}
