package amocrm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Outbound budgets are keyed by the amoCRM account and the integration
// installed in it. One integration installed in two accounts gets two
// independent pair budgets; every pair of one account also shares that
// account's ceiling. Both budgets are debited for every outgoing API v4
// request. See docs/adr/0029-outbound-budget-account-scope.md.
const (
	DefaultPairRPS        = 7
	DefaultAccountRPS     = 50
	MaxAccountRPS         = 50
	DefaultBurst          = 1
	DefaultBudgetMaxWait  = 5 * time.Second
	DefaultPairWaiters    = 64
	DefaultAccountWaiters = 512
	DefaultProcessWaiters = 4096
	DefaultBudgetTTL      = 5 * time.Minute
	DefaultBudgetEntries  = 16384
)

// budgetKey is the canonical pair scope. AccountID 0 is only used by bootstrap
// discovery, which debits the canonical account once the ID becomes known.
type budgetKey struct {
	AccountID     int64
	IntegrationID uuid.UUID
}

// LimiterConfig describes both budgets. Rates are requests per second and
// bursts are token-bucket depths. Burst 1 spaces admissions by 1/rate;
// token-bucket bounds include the initial token.
type LimiterConfig struct {
	PairRPS    float64
	PairBurst  int
	AccountRPS float64
	// AccountBurst bounds the whole account, never more than MaxAccountRPS
	// worth of tokens.
	AccountBurst int
	// PairRPSByAccount raises the pair budget for accounts that bought an
	// extended amoCRM API package. It applies to every integration of that
	// account and never raises the account ceiling.
	PairRPSByAccount map[int64]float64
	// Replicas must be 1 until a shared coordinator is implemented. Process-local
	// budgets cannot safely support multiple owners, even with divided rates.
	Replicas       int
	MaxWait        time.Duration
	PairWaiters    int
	AccountWaiters int
	ProcessWaiters int
	InactiveTTL    time.Duration
	MaxEntries     int
}

// DefaultLimiterConfig is the amoCRM baseline: 7 rps per pair, 50 rps per
// account, with one initial token per scope.
func DefaultLimiterConfig() LimiterConfig {
	return LimiterConfig{
		PairRPS: DefaultPairRPS, PairBurst: DefaultBurst,
		AccountRPS: DefaultAccountRPS, AccountBurst: DefaultBurst,
		Replicas: 1, MaxWait: DefaultBudgetMaxWait,
		PairWaiters: DefaultPairWaiters, AccountWaiters: DefaultAccountWaiters,
		ProcessWaiters: DefaultProcessWaiters,
		InactiveTTL:    DefaultBudgetTTL, MaxEntries: DefaultBudgetEntries,
	}
}

// Validate rejects configurations that would silently grant more outbound
// capacity than amoCRM allows.
func (c LimiterConfig) Validate() error {
	if err := validRPS("pair RPS", c.PairRPS); err != nil {
		return err
	}
	if err := validRPS("account RPS", c.AccountRPS); err != nil {
		return err
	}
	if c.PairRPS > c.AccountRPS {
		return fmt.Errorf("pair RPS %g must not exceed account RPS %g", c.PairRPS, c.AccountRPS)
	}
	if c.PairBurst < 1 || c.PairBurst > 100 {
		return fmt.Errorf("pair burst must be between 1 and 100, got %d", c.PairBurst)
	}
	if c.AccountBurst < 1 || c.AccountBurst > 100 {
		return fmt.Errorf("account burst must be between 1 and 100, got %d", c.AccountBurst)
	}
	for accountID, rps := range c.PairRPSByAccount {
		if accountID <= 0 {
			return fmt.Errorf("pair RPS override account id must be positive, got %d", accountID)
		}
		if err := validRPS(fmt.Sprintf("pair RPS override for account %d", accountID), rps); err != nil {
			return err
		}
		if rps > c.AccountRPS {
			return fmt.Errorf("pair RPS override for account %d is %g, above the account ceiling %g", accountID, rps, c.AccountRPS)
		}
	}
	if c.Replicas != 1 {
		return fmt.Errorf("replica count must be 1: multiple outbound owners require shared budget coordination, got %d", c.Replicas)
	}
	if c.MaxWait < 100*time.Millisecond || c.MaxWait > time.Minute {
		return fmt.Errorf("budget max wait must be between 100ms and 1m, got %s", c.MaxWait)
	}
	if c.PairWaiters < 1 || c.AccountWaiters < c.PairWaiters || c.ProcessWaiters < c.AccountWaiters {
		return fmt.Errorf("waiter limits must satisfy 1 <= pair (%d) <= account (%d) <= process (%d)",
			c.PairWaiters, c.AccountWaiters, c.ProcessWaiters)
	}
	if c.ProcessWaiters > 1_000_000 {
		return fmt.Errorf("process waiter limit must be at most 1000000, got %d", c.ProcessWaiters)
	}
	if c.InactiveTTL < time.Second || c.InactiveTTL > time.Hour {
		return fmt.Errorf("budget inactive TTL must be between 1s and 1h, got %s", c.InactiveTTL)
	}
	if c.MaxEntries < 64 || c.MaxEntries > 1_000_000 {
		return fmt.Errorf("budget entry limit must be between 64 and 1000000, got %d", c.MaxEntries)
	}
	return nil
}

func validRPS(what string, rps float64) error {
	if math.IsNaN(rps) || rps < 0.1 || rps > MaxAccountRPS {
		return fmt.Errorf("%s must be between 0.1 and %d, got %g", what, MaxAccountRPS, rps)
	}
	return nil
}

// clock isolates the limiter from scheduler jitter so window tests are
// deterministic.
type clock interface {
	Now() time.Time
	After(time.Duration) (<-chan time.Time, func())
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	return timer.C, func() { timer.Stop() }
}

// slotState keeps a bounded calendar of shared admission times. A new
// reservation may use a gap without taking capacity from existing slots.
type plannedSlot struct {
	at time.Time
	id uint64
}
type slotState struct {
	tokens  float64
	updated time.Time
	slots   []plannedSlot
}

func refillTokens(tokens float64, from, to time.Time, limit rate.Limit, burst int) float64 {
	return math.Min(float64(burst), tokens+to.Sub(from).Seconds()*float64(limit))
}

func (s *slotState) advance(now time.Time, limit rate.Limit, burst int) {
	n := 0
	for n < len(s.slots) && !s.slots[n].at.After(now) {
		slot := s.slots[n]
		s.tokens = refillTokens(s.tokens, s.updated, slot.at, limit, burst) - 1
		s.updated = slot.at
		n++
	}
	s.slots = append(s.slots[:0], s.slots[n:]...)
	s.tokens = refillTokens(s.tokens, s.updated, now, limit, burst)
	s.updated = now
}

// ready tests both the candidate and the already reserved suffix. This
// prevents inserting a debit that would make a later reservation invalid.
func (s *slotState) ready(candidate time.Time, limit rate.Limit, burst int) time.Time {
	for {
		tokens, updated := s.tokens, s.updated
		inserted := false
		retry := false
		for _, slot := range s.slots {
			if !inserted && candidate.Before(slot.at) {
				tokens = refillTokens(tokens, updated, candidate, limit, burst)
				if tokens < 1-1e-9 {
					candidate = candidate.Add(time.Duration(math.Ceil((1 - tokens) / float64(limit) * float64(time.Second))))
					retry = true
					break
				}
				tokens--
				updated = candidate
				inserted = true
			}
			tokens = refillTokens(tokens, updated, slot.at, limit, burst)
			if inserted && tokens < 1-1e-9 {
				candidate = slot.at.Add(time.Nanosecond)
				retry = true
				break
			}
			tokens--
			updated = slot.at
		}
		if retry {
			continue
		}
		if inserted {
			return candidate
		}
		tokens = refillTokens(tokens, updated, candidate, limit, burst)
		if tokens >= 1-1e-9 {
			return candidate
		}
		candidate = candidate.Add(time.Duration(math.Ceil((1 - tokens) / float64(limit) * float64(time.Second))))
	}
}

func (s *slotState) debit(at time.Time, id uint64) {
	i := sort.Search(len(s.slots), func(i int) bool { return s.slots[i].at.After(at) })
	s.slots = append(s.slots, plannedSlot{})
	copy(s.slots[i+1:], s.slots[i:])
	s.slots[i] = plannedSlot{at: at, id: id}
}

func (s *slotState) cancel(id uint64) {
	for i, slot := range s.slots {
		if slot.id == id {
			s.slots = append(s.slots[:i], s.slots[i+1:]...)
			return
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type budget struct {
	plan    slotState
	bucket  *rate.Limiter
	waiting int
	lastUse time.Time
}

type limiter struct {
	mu        sync.Mutex
	config    LimiterConfig
	clock     clock
	metrics   *Metrics
	pairs     map[budgetKey]*budget
	accounts  map[int64]*budget
	waiting   int
	lastSweep time.Time
	sequence  uint64
}

func newLimiter(config LimiterConfig) (*limiter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return newLimiterWithClock(config, systemClock{}), nil
}

func newLimiterWithClock(config LimiterConfig, c clock) *limiter {
	overrides := make(map[int64]float64, len(config.PairRPSByAccount))
	for accountID, rps := range config.PairRPSByAccount {
		overrides[accountID] = rps
	}
	config.PairRPSByAccount = overrides
	return &limiter{
		config:    config,
		clock:     c,
		pairs:     make(map[budgetKey]*budget),
		accounts:  make(map[int64]*budget),
		lastSweep: c.Now(),
	}
}

func (l *limiter) setMetrics(m *Metrics) { l.metrics = m }

// pairLimit applies an override only to pairs in its account.
func (l *limiter) pairLimit(accountID int64) rate.Limit {
	rps := l.config.PairRPS
	if override, ok := l.config.PairRPSByAccount[accountID]; ok {
		rps = override
	}
	return rate.Limit(rps)
}

func (l *limiter) accountLimit() rate.Limit { return rate.Limit(l.config.AccountRPS) }

// wait plans one common slot, then checks both live buckets atomically at
// admission. A late timer cannot compress multiple planned slots into a burst.
func (l *limiter) wait(ctx context.Context, key budgetKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := l.clock.Now()
	admit, err := l.reserve(ctx, key, admitWaiting)
	if err != nil {
		return err
	}
	delay := admit.delay
	for {
		if delay > 0 {
			timer, stop := l.clock.After(delay)
			select {
			case <-ctx.Done():
				stop()
				l.cancel(key, admit)
				return ctx.Err()
			case <-timer:
				stop()
			}
		}
		if err := ctx.Err(); err != nil {
			l.cancel(key, admit)
			return err
		}
		l.mu.Lock()
		now := l.clock.Now()
		if now.After(started.Add(l.config.MaxWait)) {
			l.mu.Unlock()
			l.cancel(key, admit)
			return l.overloaded("wait")
		}
		pair, account := l.pairs[key], l.accounts[key.AccountID]
		pairTokens, accountTokens := pair.bucket.TokensAt(now), account.bucket.TokensAt(now)
		if pairTokens >= 1 && accountTokens >= 1 {
			pair.bucket.AllowN(now, 1)
			account.bucket.AllowN(now, 1)
			pair.lastUse, account.lastUse = now, now
			l.mu.Unlock()
			l.release(key)
			return nil
		}
		seconds := math.Max(math.Max(0, 1-pairTokens)/float64(l.pairLimit(key.AccountID)), math.Max(0, 1-accountTokens)/float64(l.accountLimit()))
		delay = time.Duration(math.Ceil(seconds * float64(time.Second)))
		l.mu.Unlock()
		if now.Add(delay).After(started.Add(l.config.MaxWait)) {
			l.cancel(key, admit)
			return l.overloaded("wait")
		}
		if deadline, ok := ctx.Deadline(); ok && now.Add(delay).After(deadline) {
			l.cancel(key, admit)
			return context.DeadlineExceeded
		}
	}
}

// charge debits both budgets for a request that already reached amoCRM. The
// tokens are never refunded: a canceled caller must not hide a sent request,
// and queue pressure must not turn a completed request into a lost debit.
func (l *limiter) charge(ctx context.Context, key budgetKey) error {
	admit, err := l.reserve(ctx, key, debitSent)
	if err != nil {
		return err
	}
	if admit.delay <= 0 {
		l.release(key)
		return ctx.Err()
	}
	timer, stop := l.clock.After(admit.delay)
	defer stop()
	select {
	case <-ctx.Done():
		l.release(key)
		return ctx.Err()
	case <-timer:
		l.release(key)
		return nil
	}
}

type admission struct {
	id    uint64
	delay time.Duration
}

// admitWaiting is a request that has not been sent yet and may be shed;
// debitSent is a request that already reached amoCRM and must be paid for.
type admissionMode int

const (
	admitWaiting admissionMode = iota
	debitSent
)

// reserve assigns a common feasible slot under one lock. Existing slots are
// preserved; other pairs may fill unused account capacity between them.
func (l *limiter) reserve(ctx context.Context, key budgetKey, mode admissionMode) (admission, error) {
	l.mu.Lock()
	now := l.clock.Now()
	l.sweepLocked(now)
	if mode == admitWaiting && l.waiting >= l.config.ProcessWaiters {
		l.mu.Unlock()
		return admission{}, l.overloaded("process")
	}
	pair, err := budgetLocked(l, l.pairs, key, l.pairLimit(key.AccountID), l.config.PairBurst, now)
	if err != nil {
		l.mu.Unlock()
		return admission{}, err
	}
	account, err := budgetLocked(l, l.accounts, key.AccountID, l.accountLimit(), l.config.AccountBurst, now)
	if err != nil {
		l.mu.Unlock()
		return admission{}, err
	}
	if mode == admitWaiting {
		if pair.waiting >= l.config.PairWaiters {
			l.mu.Unlock()
			return admission{}, l.overloaded("pair")
		}
		if account.waiting >= l.config.AccountWaiters {
			l.mu.Unlock()
			return admission{}, l.overloaded("account")
		}
	}

	pair.plan.advance(now, l.pairLimit(key.AccountID), l.config.PairBurst)
	account.plan.advance(now, l.accountLimit(), l.config.AccountBurst)
	pairAt := pair.plan.ready(now, l.pairLimit(key.AccountID), l.config.PairBurst)
	accountAt := account.plan.ready(now, l.accountLimit(), l.config.AccountBurst)
	at := maxTime(pairAt, accountAt)
	for {
		next := maxTime(pair.plan.ready(at, l.pairLimit(key.AccountID), l.config.PairBurst), account.plan.ready(at, l.accountLimit(), l.config.AccountBurst))
		if next.Equal(at) {
			break
		}
		at = next
	}
	delay := at.Sub(now)
	if mode == admitWaiting {
		if delay > l.config.MaxWait {
			l.mu.Unlock()
			return admission{}, l.overloaded("wait")
		}
		if deadline, ok := ctx.Deadline(); ok && at.After(deadline) {
			l.mu.Unlock()
			return admission{}, fmt.Errorf("amoCRM budget slot is after the caller deadline: %w", context.DeadlineExceeded)
		}
	}
	l.sequence++
	admit := admission{id: l.sequence, delay: delay}
	pair.plan.debit(at, admit.id)
	account.plan.debit(at, admit.id)
	if mode == debitSent {
		// Sent bootstrap requests must also debit live admission buckets, even
		// when their caller is canceled. Their reservations are never refunded.
		pair.bucket.ReserveN(now, 1)
		account.bucket.ReserveN(now, 1)
	}
	pair.waiting++
	account.waiting++
	l.waiting++
	pair.lastUse = now
	account.lastUse = now
	if l.metrics != nil {
		l.metrics.waiting.Set(float64(l.waiting))
	}
	l.mu.Unlock()

	if l.metrics != nil {
		if pairAt.After(now) {
			l.metrics.throttled.WithLabelValues("pair").Inc()
		}
		if accountAt.After(now) {
			l.metrics.throttled.WithLabelValues("account").Inc()
		}
	}
	return admit, nil
}

// budgetLocked returns the bucket for one scope, creating it at full depth. A
// recreated bucket is never richer than the swept one: entries are only
// evicted once their tokens are fully replenished.
func budgetLocked[K comparable](l *limiter, table map[K]*budget, id K, limit rate.Limit, burst int, now time.Time) (*budget, error) {
	entry, ok := table[id]
	if ok {
		if entry.bucket.Limit() != limit {
			// Changing the rate must not hand out a fresh full burst.
			entry.bucket.SetLimitAt(now, limit)
		}
		return entry, nil
	}
	if len(table) >= l.config.MaxEntries {
		return nil, l.overloaded("registry")
	}
	entry = &budget{bucket: rate.NewLimiter(limit, burst), plan: slotState{tokens: float64(burst), updated: now}, lastUse: now}
	table[id] = entry
	return entry, nil
}

func (l *limiter) release(key budgetKey) {
	l.mu.Lock()
	if pair := l.pairs[key]; pair != nil && pair.waiting > 0 {
		pair.waiting--
	}
	if account := l.accounts[key.AccountID]; account != nil && account.waiting > 0 {
		account.waiting--
	}
	if l.waiting > 0 {
		l.waiting--
	}
	if l.metrics != nil {
		l.metrics.waiting.Set(float64(l.waiting))
	}
	l.mu.Unlock()
}

// Cancellation removes this caller's planned debits without moving existing
// slots. No live tokens were spent by an unsent caller.
func (l *limiter) cancel(key budgetKey, admit admission) {
	l.mu.Lock()
	l.pairs[key].plan.cancel(admit.id)
	l.accounts[key.AccountID].plan.cancel(admit.id)
	l.mu.Unlock()
	l.release(key)
}

func (l *limiter) overloaded(scope string) error {
	if l.metrics != nil {
		l.metrics.rejected.WithLabelValues(scope).Inc()
	}
	return &APIError{
		Kind:       ErrorOverloaded,
		Retryable:  true,
		RetryAfter: l.config.MaxWait,
	}
}

func (l *limiter) snapshotWaiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiting
}

// sweepLocked drops idle budgets to bound memory. An entry leaves only when
// nobody waits on it and its bucket is full, so re-creating it grants exactly
// the same capacity and cannot be used to bypass the limit.
func (l *limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.config.InactiveTTL/2 {
		return
	}
	l.lastSweep = now
	for key, entry := range l.pairs {
		if evictable(entry, now, l.config.PairBurst, l.config.InactiveTTL) {
			delete(l.pairs, key)
		}
	}
	for id, entry := range l.accounts {
		if evictable(entry, now, l.config.AccountBurst, l.config.InactiveTTL) {
			delete(l.accounts, id)
		}
	}
}

func evictable(entry *budget, now time.Time, burst int, ttl time.Duration) bool {
	entry.plan.advance(now, entry.bucket.Limit(), burst)
	return entry.waiting == 0 && now.Sub(entry.lastUse) >= ttl &&
		entry.bucket.TokensAt(now) >= float64(burst) && len(entry.plan.slots) == 0 && entry.plan.tokens >= float64(burst)
}
