package amocrm

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testClock replaces wall time so window assertions never depend on scheduler
// delays. Timers fire only from Advance.
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*testTimer
}

type testTimer struct {
	at      time.Time
	channel chan time.Time
	done    bool
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2036, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) After(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &testTimer{at: c.now.Add(d), channel: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer.channel, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		timer.done = true
	}
}

// Advance visits each timer deadline instead of firing a backlog at the
// destination time. Tests for delayed wakeups use Jump explicitly.
func (c *testClock) Advance(d time.Duration) {
	target := c.Now().Add(d)
	for {
		c.mu.Lock()
		next := target
		for _, timer := range c.timers {
			if !timer.done && timer.at.Before(next) {
				next = timer.at
			}
		}
		if next.Before(c.now) {
			next = c.now
		}
		c.mu.Unlock()
		c.Jump(next.Sub(c.Now()))
		// Let woken callers complete admission or install their next timer.
		time.Sleep(3 * time.Millisecond)
		if !c.Now().Before(target) {
			return
		}
	}
}

func (c *testClock) Jump(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	pending := c.timers[:0:0]
	var due []*testTimer
	for _, timer := range c.timers {
		switch {
		case timer.done:
		case !timer.at.After(now):
			timer.done = true
			due = append(due, timer)
		default:
			pending = append(pending, timer)
		}
	}
	c.timers = pending
	c.mu.Unlock()
	for _, timer := range due {
		timer.channel <- now
	}
}

// settled waits until a counter that other goroutines update stops changing,
// so a test never reads a half-finished admission round.
func settled(t *testing.T, value func() int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, since := value(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		current := value()
		if current != last {
			last, since = current, time.Now()
			continue
		}
		if time.Since(since) >= 100*time.Millisecond {
			return current
		}
	}
	t.Fatalf("counter never settled, last value %d", last)
	return last
}

func testLimiterConfig() LimiterConfig {
	config := DefaultLimiterConfig()
	config.MaxWait = time.Minute
	return config
}

func newTestLimiter(t *testing.T, config LimiterConfig) (*limiter, *testClock) {
	t.Helper()
	if err := config.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	clock := newTestClock()
	return newLimiterWithClock(config, clock), clock
}

type callGroup struct {
	admitted *atomic.Int64
	refused  *atomic.Int64
	join     func()
}

func (g callGroup) admittedCount() int { return int(g.admitted.Load()) }

// call starts n waits on one pair and reports how many were admitted.
func call(l *limiter, key budgetKey, n int) callGroup {
	group := callGroup{admitted: new(atomic.Int64), refused: new(atomic.Int64)}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.wait(context.Background(), key); err != nil {
				group.refused.Add(1)
				return
			}
			group.admitted.Add(1)
		}()
	}
	group.join = wg.Wait
	return group
}

func pairOf(accountID int64, integration uuid.UUID) budgetKey {
	return budgetKey{AccountID: accountID, IntegrationID: integration}
}

// One integration installed in two accounts holds two independent 7 rps
// budgets, so neither account slows the other down.
func TestPairBudgetsAreIsolatedPerAccount(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	integration := uuid.New()
	first := call(l, pairOf(1, integration), 14)
	second := call(l, pairOf(2, integration), 14)
	settled(t, l.snapshotWaiting)

	clock.Advance(time.Second - time.Microsecond)
	// Admissions are spaced by at least ceil(1/rate), including live checks.
	if admitted := settled(t, first.admittedCount); admitted != 7 {
		t.Fatalf("account 1 admitted %d requests in one second, want 7", admitted)
	}
	if admitted := settled(t, second.admittedCount); admitted != 7 {
		t.Fatalf("account 2 admitted %d requests in one second, want 7", admitted)
	}
	clock.Advance(time.Minute)
	first.join()
	second.join()
}

// Two integrations of one account keep separate pair budgets: a saturated
// integration does not eat the other one's 7 rps.
func TestPairsOfOneAccountKeepSeparateBudgets(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	busy := call(l, pairOf(7, uuid.New()), 30)
	quiet := call(l, pairOf(7, uuid.New()), 5)
	settled(t, l.snapshotWaiting)

	clock.Advance(time.Second - time.Microsecond)
	if admitted := settled(t, busy.admittedCount); admitted != 7 {
		t.Fatalf("saturated integration admitted %d, want its own 7 rps", admitted)
	}
	if admitted := settled(t, quiet.admittedCount); admitted != 5 {
		t.Fatalf("second integration admitted %d of its 5 requests", admitted)
	}
	clock.Advance(time.Minute)
	busy.join()
	quiet.join()
}

// Eight saturated integrations of one account demand 56 rps; the account
// ceiling keeps them at 50 rps and none of them starves.
func TestAccountCeilingBoundsEveryPairOfThatAccount(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	groups := make([]callGroup, 8)
	for i := range groups {
		groups[i] = call(l, pairOf(9, uuid.New()), 20)
	}
	settled(t, l.snapshotWaiting)
	total := func() int {
		sum := 0
		for _, group := range groups {
			sum += group.admittedCount()
		}
		return sum
	}

	clock.Advance(time.Second)
	if admitted := settled(t, total); admitted > 51 || admitted == 0 {
		t.Fatalf("account admitted %d requests in one second, want between 1 and 51", admitted)
	}
	clock.Advance(4 * time.Second)
	if admitted := settled(t, total); admitted != 160 {
		t.Fatalf("drained %d of 160 requests", admitted)
	}
	for i, group := range groups {
		if group.admittedCount() != 20 {
			t.Fatalf("integration %d was starved: %d of 20 requests", i, group.admittedCount())
		}
	}
	for _, group := range groups {
		group.join()
	}
}

// An override raises every pair of the chosen account only.
func TestAccountOverrideRaisesOnlyThatAccount(t *testing.T) {
	config := testLimiterConfig()
	config.PairRPSByAccount = map[int64]float64{100: 20}
	l, clock := newTestLimiter(t, config)
	upgraded := call(l, pairOf(100, uuid.New()), 25)
	upgradedSecond := call(l, pairOf(100, uuid.New()), 25)
	standard := call(l, pairOf(200, uuid.New()), 30)
	settled(t, l.snapshotWaiting)

	clock.Advance(time.Second - time.Microsecond)
	if admitted := settled(t, upgraded.admittedCount); admitted != 20 {
		t.Fatalf("upgraded pair admitted %d, want 20 rps", admitted)
	}
	if admitted := settled(t, upgradedSecond.admittedCount); admitted != 20 {
		t.Fatalf("second upgraded pair admitted %d, want 20 rps", admitted)
	}
	if admitted := settled(t, standard.admittedCount); admitted != 7 {
		t.Fatalf("account without the override admitted %d, want 7 rps", admitted)
	}
	clock.Advance(time.Minute)
	upgraded.join()
	upgradedSecond.join()
	standard.join()
}

// Three upgraded pairs demand 60 rps; the account ceiling stays 50 rps.
func TestRaisedPairsStillRespectTheAccountCeiling(t *testing.T) {
	config := testLimiterConfig()
	config.PairRPSByAccount = map[int64]float64{100: 20}
	l, clock := newTestLimiter(t, config)
	groups := make([]callGroup, 3)
	for i := range groups {
		groups[i] = call(l, pairOf(100, uuid.New()), 25)
	}
	settled(t, l.snapshotWaiting)
	total := func() int {
		sum := 0
		for _, group := range groups {
			sum += group.admittedCount()
		}
		return sum
	}

	clock.Advance(time.Second)
	if admitted := settled(t, total); admitted > 51 {
		t.Fatalf("upgraded pairs admitted %d requests in one second, above the 50 rps account ceiling", admitted)
	}
	clock.Advance(time.Minute)
	for _, group := range groups {
		group.join()
	}
}

// Sustained overload must shed instead of growing an unbounded queue.
func TestQueueFullShedsWithOverloadedError(t *testing.T) {
	config := testLimiterConfig()
	config.PairWaiters = 4
	config.AccountWaiters = 8
	config.ProcessWaiters = 8
	l, clock := newTestLimiter(t, config)
	key := pairOf(3, uuid.New())
	queued := call(l, key, 5)
	if waiting := settled(t, l.snapshotWaiting); waiting != 4 {
		t.Fatalf("queue holds %d waiters, want the configured 4", waiting)
	}

	err := l.wait(context.Background(), key)
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("full queue returned %v, want ErrOverloaded", err)
	}
	var api *APIError
	if !errors.As(err, &api) || api.Kind != ErrorOverloaded || !api.Retryable {
		t.Fatalf("overload must be a retryable classified error, got %#v", err)
	}
	if queued.refused.Load() != 0 {
		t.Fatal("already queued callers were shed instead of served")
	}
	clock.Advance(time.Minute)
	queued.join()
	if waiting := l.snapshotWaiting(); waiting != 0 {
		t.Fatalf("waiters leaked: %d", waiting)
	}
}

// A slot further away than MaxWait is refused at once instead of parking the
// caller for an unbounded time.
func TestSlotBeyondMaxWaitIsRefused(t *testing.T) {
	config := testLimiterConfig()
	config.MaxWait = 500 * time.Millisecond
	l, clock := newTestLimiter(t, config)
	key := pairOf(4, uuid.New())

	// At 7 rps the first call is free and the next three land at 143ms, 286ms
	// and 429ms; a fifth would land past the 500ms ceiling.
	queued := call(l, key, 4)
	settled(t, l.snapshotWaiting)
	if err := l.wait(context.Background(), key); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("wait beyond MaxWait returned %v, want ErrOverloaded", err)
	}
	clock.Advance(time.Minute)
	queued.join()
}

// A caller whose deadline falls before its slot fails fast and keeps no
// waiter slot.
func TestDeadlineBeforeSlotFailsFast(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	key := pairOf(5, uuid.New())
	if err := l.wait(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), clock.Now().Add(10*time.Millisecond))
	defer cancel()
	if err := l.wait(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short deadline returned %v, want DeadlineExceeded", err)
	}
	if waiting := l.snapshotWaiting(); waiting != 0 {
		t.Fatalf("refused caller kept a waiter slot: %d", waiting)
	}
}

// Cancelling a queued call frees its waiter slot and returns its reservations
// so the freed capacity serves the next caller.
func TestCancellationReleasesSlotAndReservations(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	key := pairOf(6, uuid.New())
	if err := l.wait(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	abandoned := make(chan error, 1)
	go func() { abandoned <- l.wait(ctx, key) }()
	if waiting := settled(t, l.snapshotWaiting); waiting != 1 {
		t.Fatalf("queued caller not registered: %d", waiting)
	}
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}
	if waiting := settled(t, l.snapshotWaiting); waiting != 0 {
		t.Fatalf("cancelled caller leaked a waiter slot: %d", waiting)
	}

	next := make(chan error, 1)
	go func() { next <- l.wait(context.Background(), key) }()
	settled(t, l.snapshotWaiting)
	clock.Advance(143 * time.Millisecond)
	if err := <-next; err != nil {
		t.Fatalf("returned reservation was not reused: %v", err)
	}
}

// Once demand drops below the available rate the queue drains completely.
func TestQueueDrainsWhenDemandDropsBelowRate(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	queued := call(l, pairOf(8, uuid.New()), 21)
	settled(t, l.snapshotWaiting)

	clock.Advance(3 * time.Second)
	queued.join()
	if queued.refused.Load() != 0 || queued.admitted.Load() != 21 {
		t.Fatalf("drain admitted=%d refused=%d, want 21 and 0", queued.admitted.Load(), queued.refused.Load())
	}
	if waiting := l.snapshotWaiting(); waiting != 0 {
		t.Fatalf("waiters left after drain: %d", waiting)
	}
}

// Sweeping idle budgets bounds memory, and a re-created budget starts with the
// same single token, so cleanup cannot be used to bypass the limit.
func TestSweepDropsIdleBudgetsWithoutGrantingCapacity(t *testing.T) {
	config := testLimiterConfig()
	config.InactiveTTL = 2 * time.Second
	l, clock := newTestLimiter(t, config)
	key := pairOf(11, uuid.New())
	if err := l.wait(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	if err := l.wait(context.Background(), pairOf(12, uuid.New())); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	pairs := len(l.pairs)
	l.mu.Unlock()
	if pairs != 1 {
		t.Fatalf("idle budgets kept: %d entries, want only the fresh one", pairs)
	}

	if err := l.wait(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	next := make(chan error, 1)
	go func() { next <- l.wait(context.Background(), key) }()
	settled(t, l.snapshotWaiting)
	clock.Advance(142 * time.Millisecond)
	select {
	case err := <-next:
		t.Fatalf("re-created budget admitted a second request inside the same slot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Second)
	if err := <-next; err != nil {
		t.Fatal(err)
	}
}

// charge debits a request that already reached amoCRM, and cancelling
// afterwards must not refund it.
func TestChargeKeepsTokensAfterCancellation(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	key := pairOf(13, uuid.New())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.charge(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("charge returned %v, want Canceled", err)
	}
	next := make(chan error, 1)
	go func() { next <- l.wait(context.Background(), key) }()
	if waiting := settled(t, l.snapshotWaiting); waiting != 1 {
		t.Fatal("cancelled charge refunded its token")
	}
	clock.Advance(143 * time.Millisecond)
	if err := <-next; err != nil {
		t.Fatal(err)
	}
}

func TestLimiterConfigValidation(t *testing.T) {
	cases := map[string]func(*LimiterConfig){
		"pair above account ceiling": func(c *LimiterConfig) { c.PairRPS = 60 },
		"account above amoCRM ceiling": func(c *LimiterConfig) {
			c.AccountRPS = 120
		},
		"zero burst":       func(c *LimiterConfig) { c.PairBurst = 0 },
		"negative account": func(c *LimiterConfig) { c.PairRPSByAccount = map[int64]float64{-1: 10} },
		"override above ceiling": func(c *LimiterConfig) {
			c.PairRPSByAccount = map[int64]float64{5: 80}
		},
		"wait too long":       func(c *LimiterConfig) { c.MaxWait = 2 * time.Hour },
		"inverted waiters":    func(c *LimiterConfig) { c.PairWaiters = 4096; c.AccountWaiters = 8 },
		"too many replicas":   func(c *LimiterConfig) { c.Replicas = 99 },
		"tiny entry registry": func(c *LimiterConfig) { c.MaxEntries = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := DefaultLimiterConfig()
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
	if err := DefaultLimiterConfig().Validate(); err != nil {
		t.Fatalf("default configuration rejected: %v", err)
	}
}

// Sustained overload: one account receives 35 rps for one integration and
// 30 rps for another while both pairs may send 7 rps. The queue, the number
// of parked calls and therefore memory must stay bounded, and the limiter
// must recover once the load stops.
func TestSustainedOverloadKeepsQueueAndMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("load scenario runs for a few seconds")
	}
	config := DefaultLimiterConfig()
	config.MaxWait = time.Second
	config.PairWaiters = 16
	config.AccountWaiters = 64
	config.ProcessWaiters = 128
	budgets, err := newLimiter(config)
	if err != nil {
		t.Fatal(err)
	}
	account := int64(21)
	pairs := []budgetKey{pairOf(account, uuid.New()), pairOf(account, uuid.New())}
	arrivals := []int{35, 30}

	var admitted, shed, other atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	started := time.Now()
	load, stopLoad := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopLoad()
	for i, key := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second / time.Duration(arrivals[i]))
			defer ticker.Stop()
			for {
				select {
				case <-load.Done():
					return
				case <-ticker.C:
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					if waiting := int64(budgets.snapshotWaiting()); waiting > peak.Load() {
						peak.Store(waiting)
					}
					switch err := budgets.wait(context.Background(), key); {
					case err == nil:
						admitted.Add(1)
					case errors.Is(err, ErrOverloaded):
						shed.Add(1)
					default:
						other.Add(1)
					}
				}()
			}
		}()
	}
	wg.Wait()

	if shed.Load() == 0 {
		t.Fatal("overload was absorbed by an unbounded queue instead of being shed")
	}
	if other.Load() != 0 {
		t.Fatalf("%d calls failed for an unclassified reason", other.Load())
	}
	if peak.Load() > int64(config.ProcessWaiters) {
		t.Fatalf("parked calls peaked at %d, above the %d process limit", peak.Load(), config.ProcessWaiters)
	}
	// Two pairs sustain 7 rps each, plus one burst token per pair.
	ceiling := int64(2*DefaultPairRPS*time.Since(started).Seconds()) + 2
	if got := admitted.Load(); got > ceiling {
		t.Fatalf("admitted %d requests, above the %d allowed by two 7 rps pairs", got, ceiling)
	}
	if waiting := budgets.snapshotWaiting(); waiting != 0 {
		t.Fatalf("queue did not drain after the load stopped: %d waiting", waiting)
	}
}

// Multiple process-local owners cannot preserve the shared burst/window
// bounds. Fail closed until a common coordinator is available.
func TestMultipleOutboundReplicasAreRejected(t *testing.T) {
	for _, replicas := range []int{2, 16} {
		config := DefaultLimiterConfig()
		config.Replicas = replicas
		if _, err := newLimiter(config); err == nil {
			t.Fatalf("accepted %d independent outbound owners", replicas)
		}
	}
}

func TestPairSlotsStaySpacedAfterAccountCongestion(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	for i := 0; i < 100; i++ {
		if _, err := l.reserve(context.Background(), pairOf(42, uuid.New()), admitWaiting); err != nil {
			t.Fatal(err)
		}
	}
	key := pairOf(42, uuid.New())
	first, err := l.reserve(context.Background(), key, admitWaiting)
	if err != nil {
		t.Fatal(err)
	}
	previous := clock.Now().Add(first.delay)
	clock.Advance(time.Second)
	for i := 0; i < 10; i++ {
		slot, err := l.reserve(context.Background(), key, admitWaiting)
		if err != nil {
			t.Fatal(err)
		}
		at := clock.Now().Add(slot.delay)
		if gap := at.Sub(previous); gap < time.Second/7 {
			t.Fatalf("pair slots only %v apart", gap)
		}
		previous = at
	}
}

func TestAccountSlotsStaySpacedBehindSlowPair(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	key := pairOf(42, uuid.New())
	var slots []time.Time
	for i := 0; i < 20; i++ {
		slot, err := l.reserve(context.Background(), key, admitWaiting)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, clock.Now().Add(slot.delay))
	}
	clock.Advance(time.Second)
	for i := 0; i < 10; i++ {
		slot, err := l.reserve(context.Background(), pairOf(42, uuid.New()), admitWaiting)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, clock.Now().Add(slot.delay))
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Before(slots[j]) })
	for i := 1; i < len(slots); i++ {
		if gap := slots[i].Sub(slots[i-1]); gap < time.Second/50 {
			t.Fatalf("account slots only %v apart", gap)
		}
	}
}

func TestLateTimersDoNotCompressPairAdmissions(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	group := call(l, pairOf(42, uuid.New()), 10)
	settled(t, l.snapshotWaiting)
	clock.Jump(2 * time.Second)
	if got := settled(t, group.admittedCount); got != 2 {
		t.Fatalf("late timers admitted %d calls, want initial call plus one live token", got)
	}
	clock.Advance(3 * time.Second)
	group.join()
	if got := group.admittedCount(); got != 10 {
		t.Fatalf("admitted %d of 10 calls", got)
	}
}

func TestCancellationOfMiddleSlotPreservesRemainingPlans(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	key := pairOf(42, uuid.New())
	first, err := l.reserve(context.Background(), key, admitWaiting)
	if err != nil {
		t.Fatal(err)
	}
	middle, err := l.reserve(context.Background(), key, admitWaiting)
	if err != nil {
		t.Fatal(err)
	}
	last, err := l.reserve(context.Background(), key, admitWaiting)
	if err != nil {
		t.Fatal(err)
	}
	l.cancel(key, middle)
	replacement, err := l.reserve(context.Background(), key, admitWaiting)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.delay != middle.delay {
		t.Fatalf("replacement slot %v, want canceled slot %v", replacement.delay, middle.delay)
	}
	if first.delay != 0 || last.delay < 2*time.Second/7 {
		t.Fatal("remaining planned slots changed")
	}
	l.mu.Lock()
	slots := append([]plannedSlot(nil), l.accounts[key.AccountID].plan.slots...)
	l.mu.Unlock()
	for i := 1; i < len(slots); i++ {
		if slots[i].at.Sub(slots[i-1].at) < time.Second/50 {
			t.Fatal("account slots overlap after cancellation")
		}
	}
	if clock.Now().Add(last.delay).Before(clock.Now().Add(replacement.delay)) {
		t.Fatal("replacement displaced the last reservation")
	}
}

func TestLateTimersDoNotCompressAccountAdmissions(t *testing.T) {
	l, clock := newTestLimiter(t, testLimiterConfig())
	groups := make([]callGroup, 8)
	for i := range groups {
		groups[i] = call(l, pairOf(42, uuid.New()), 10)
	}
	settled(t, l.snapshotWaiting)
	total := func() int {
		n := 0
		for _, group := range groups {
			n += group.admittedCount()
		}
		return n
	}
	clock.Jump(2 * time.Second)
	if got := settled(t, total); got != 2 {
		t.Fatalf("late account timers admitted %d calls, want initial call plus one live token", got)
	}
	clock.Advance(5 * time.Second)
	for _, group := range groups {
		group.join()
	}
	if got := total(); got != 80 {
		t.Fatalf("drained %d of 80 calls", got)
	}
}
