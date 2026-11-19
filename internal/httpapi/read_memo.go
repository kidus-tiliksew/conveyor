package httpapi

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const readMemoCapacity = 128
const readMemoLifetime = 5 * time.Second

// readMemoByteBudget bounds the encoded bytes the memo holds across all of
// its entries. An entry larger than the budget is served and never stored,
// and older entries are evicted first until a new entry fits.
const readMemoByteBudget = 8 << 20

// requestConfigurationKey holds one request's configuration per workspace,
// so a context that changes workspace reads that workspace's configuration.
type requestConfigurationKey struct{ workspace string }

type loadedRequestConfiguration struct {
	config *config.Config
}

func (s *Server) requestConfiguration(ctx context.Context) (context.Context, *config.Config, error) {
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	key := requestConfigurationKey{workspace: workspaceID}
	if loaded, ok := ctx.Value(key).(loadedRequestConfiguration); ok {
		return ctx, loaded.config, nil
	}
	var cfg *config.Config
	var err error
	if s.ConfigProvider != nil {
		cfg, err = s.ConfigProvider(ctx)
		if err != nil {
			return ctx, nil, err
		}
	}
	return context.WithValue(ctx, key, loadedRequestConfiguration{config: cfg}), cfg, nil
}

// loadedConfiguration returns the configuration this request already loaded
// for workspaceID.
func loadedConfiguration(ctx context.Context, workspaceID string) (loadedRequestConfiguration, bool) {
	loaded, ok := ctx.Value(requestConfigurationKey{workspace: workspaceID}).(loadedRequestConfiguration)
	return loaded, ok
}

type readMemoHorizonKey struct{}

// readMemoHorizon belongs to one refresh, never to a cached response. until
// ends the tag's and the entry's validity: the earliest of the memo lifetime,
// the next UTC minute, every display instant such as a proposal's next age
// second, and every real projection deadline. deadline is the earliest real
// projection deadline the refresh read, such as a stated deadline, the review
// lease horizon, an order time, or a readiness expiry: an instant after which
// a fresh projection can state something different. The lifetime, the minute
// boundary, and display instants end only the tag and never lower deadline,
// so a slow build is not read again because one of them passed
// (component-next-action invariant 58).
type readMemoHorizon struct {
	now      time.Time
	until    time.Time
	deadline time.Time
}

// newReadMemoHorizon starts a memo refresh at now.
func newReadMemoHorizon(now time.Time) *readMemoHorizon {
	return &readMemoHorizon{now: now, until: memoReadUntil(now)}
}

// include records a real projection deadline: it ends the tag and the entry
// and can make readMemo.get build again.
func (h *readMemoHorizon) include(until time.Time) {
	if !until.After(h.now) {
		return
	}
	h.expire(until)
	if h.deadline.IsZero() || until.Before(h.deadline) {
		h.deadline = until
	}
}

// expire records an instant that ends only the tag and the entry.
func (h *readMemoHorizon) expire(until time.Time) {
	if until.After(h.now) && (h.until.IsZero() || until.Before(h.until)) {
		h.until = until
	}
}

// memoExpiry bounds the enclosing read's tag and memo entry by a display
// instant, such as a proposal's next age second, without making it a
// projection deadline.
func memoExpiry(ctx context.Context, until time.Time) {
	if h, ok := ctx.Value(readMemoHorizonKey{}).(*readMemoHorizon); ok {
		h.expire(until)
	}
}

func memoHorizon(ctx context.Context, until time.Time) {
	if h, ok := ctx.Value(readMemoHorizonKey{}).(*readMemoHorizon); ok {
		h.include(until)
	}
}

// memoEntryHorizon bounds an enclosing read by a memo value it used. The
// value's validity ends the enclosing tag's validity, but only the value's
// own real deadline counts as a projection deadline: the memo lifetime that
// capped the value is not one.
func memoEntryHorizon(ctx context.Context, value readMemoValue) {
	h, ok := ctx.Value(readMemoHorizonKey{}).(*readMemoHorizon)
	if !ok {
		return
	}
	h.expire(value.validUntil)
	if !value.deadline.IsZero() {
		h.include(value.deadline)
	}
}

type readMemoValue struct {
	body       []byte
	etag       string
	validUntil time.Time
	// deadline is the build's earliest real projection instant
	// (readMemoHorizon.deadline). Zero means the build read none.
	deadline time.Time
	header   map[string]string
	// unstored marks a result whose inputs changed during its build: it
	// answers its own caller once, carries no ETag, and is never stored.
	unstored bool
	// reads are the merge-readiness peeks the build made, handed to an
	// enclosing statement read on every answer.
	reads []readinessRead
}

// size is the encoded bytes an entry holds against readMemoByteBudget.
func (v readMemoValue) size() int {
	size := len(v.body) + len(v.etag)
	for key, value := range v.header {
		size += len(key) + len(value)
	}
	return size
}

func (v readMemoValue) clone() readMemoValue {
	v.body = append([]byte(nil), v.body...)
	v.reads = append([]readinessRead(nil), v.reads...)
	if v.header != nil {
		h := make(map[string]string, len(v.header))
		for k, value := range v.header {
			h[k] = value
		}
		v.header = h
	}
	return v
}

type readMemoEntry struct {
	value    readMemoValue
	sequence uint64
}

type readMemoFlight struct {
	done chan struct{}
	err  error
}

// readMemo caches encodings, not request-owned objects. All notice kinds share
// one generation because the hub can coalesce notices from different workspaces.
type readMemo struct {
	mu         sync.Mutex
	entries    map[string]readMemoEntry
	flights    map[string]*readMemoFlight
	generation uint64
	sequence   uint64
	// bytes is the summed size of every held entry.
	bytes   int
	notices []*changes.Subscription
	clock   func() time.Time
	// budget overrides readMemoByteBudget in tests.
	budget int
	// waiting, when set, runs as a caller starts waiting on another
	// caller's flight; tests use it as a barrier.
	waiting func()
}

// UseReadMemoClock replaces the response memo's clock. Store conformance
// suites hold it still, so a memo entry misses only when a notice drops it.
func (s *Server) UseReadMemoClock(clock func() time.Time) {
	if s.readMemo != nil {
		s.readMemo.clock = clock
	}
}

// readMemoBudgetKey carries one request's readMemoBudget.
type readMemoBudgetKey struct{}

// readMemoBudget counts the deadline rebuilds one request has spent.
type readMemoBudget struct{ spent atomic.Int32 }

// spend takes one deadline rebuild and reports whether one was left.
func (b *readMemoBudget) spend() bool {
	if b.spent.Add(1) > readMemoRebuilds-1 {
		b.spent.Add(-1)
		return false
	}
	return true
}

// readMemoRebuilds bounds how many times one caller builds again because a
// notice arrived during its build. Notices are process-wide, so a stream of
// unrelated notices could otherwise discard every build of a busy daemon;
// the last build answers its caller once, untagged and unstored.
const readMemoRebuilds = 3

func (m *readMemo) byteBudget() int {
	if m.budget > 0 {
		return m.budget
	}
	return readMemoByteBudget
}

// removeLocked drops one entry and its bytes.
func (m *readMemo) removeLocked(key string) {
	if entry, ok := m.entries[key]; ok {
		m.bytes -= entry.value.size()
		delete(m.entries, key)
	}
}

// storeLocked holds value under key. A value larger than the byte budget is
// not stored. Otherwise the oldest entries are evicted until both the entry
// count and the byte budget fit.
func (m *readMemo) storeLocked(key string, value readMemoValue) {
	m.removeLocked(key)
	size := value.size()
	if size > m.byteBudget() {
		return
	}
	if m.entries == nil {
		m.entries = make(map[string]readMemoEntry)
	}
	for len(m.entries) > 0 && (len(m.entries) >= readMemoCapacity || m.bytes+size > m.byteBudget()) {
		var oldestKey string
		var oldest uint64
		for k, entry := range m.entries {
			if oldestKey == "" || entry.sequence < oldest {
				oldestKey, oldest = k, entry.sequence
			}
		}
		m.removeLocked(oldestKey)
	}
	m.sequence++
	m.entries[key] = readMemoEntry{value: value.clone(), sequence: m.sequence}
	m.bytes += size
}

func (m *readMemo) subscribe(hub *changes.Hub) {
	if hub == nil {
		return
	}
	for _, kind := range []changes.Kind{changes.Workspace, changes.Task, changes.Job, changes.Deadline} {
		m.notices = append(m.notices, hub.SubscribeAll(kind))
	}
}

func (m *readMemo) now() time.Time {
	if m.clock != nil {
		return m.clock().UTC()
	}
	return time.Now().UTC()
}

// drainLocked must be called on both sides of a refresh. A commit during a
// refresh invalidates its generation and prevents publication of that result.
func (m *readMemo) drainLocked() {
	changed := false
	for _, sub := range m.notices {
		for {
			select {
			case <-sub.C():
				changed = true
			default:
				goto drained
			}
		}
	drained:
	}
	if changed {
		m.generation++
		m.entries = nil
		m.bytes = 0
	}
}

// get coalesces equal misses. Build must validate and build from one request's
// already authorized scope; errors and results invalidated during a build are
// never stored. The returned bytes and headers are private to the caller.
func (m *readMemo) get(ctx context.Context, key string, build func(context.Context, time.Time) (readMemoValue, error)) (readMemoValue, error) {
	// One request shares one budget of deadline rebuilds across every memo
	// read it nests, so an enclosing build and the inbox read inside it do
	// not multiply their rebuilds.
	budget, ok := ctx.Value(readMemoBudgetKey{}).(*readMemoBudget)
	if !ok {
		budget = &readMemoBudget{}
		ctx = context.WithValue(ctx, readMemoBudgetKey{}, budget)
	}
	builds := 0
	for {
		if err := ctx.Err(); err != nil {
			return readMemoValue{}, err
		}
		m.mu.Lock()
		m.drainLocked()
		now := m.now()
		if entry, ok := m.entries[key]; ok {
			if now.Before(entry.value.validUntil) {
				value := entry.value.clone()
				m.mu.Unlock()
				return value, nil
			}
			m.removeLocked(key)
		}
		if flight := m.flights[key]; flight != nil {
			done := flight.done
			m.mu.Unlock()
			if m.waiting != nil {
				m.waiting()
			}
			select {
			case <-done:
				// A leader's failure, its cancellation included, belongs
				// to the leader: this caller looks again and builds under
				// its own context when nothing was stored.
				continue
			case <-ctx.Done():
				return readMemoValue{}, ctx.Err()
			}
		}
		if m.flights == nil {
			m.flights = make(map[string]*readMemoFlight)
		}
		flight := &readMemoFlight{done: make(chan struct{})}
		m.flights[key] = flight
		generation := m.generation
		m.mu.Unlock()

		value, err := build(ctx, now)
		builds++
		m.mu.Lock()
		m.drainLocked()
		delete(m.flights, key)
		flight.err = err
		close(flight.done)
		changed := generation != m.generation
		if value.validUntil.After(now.Add(readMemoLifetime)) {
			value.validUntil = now.Add(readMemoLifetime)
		}
		answered := m.now()
		expired := !answered.Before(value.validUntil)
		deadlinePassed := !value.deadline.IsZero() && !answered.Before(value.deadline)
		if err == nil && !changed && !expired && !value.unstored {
			m.storeLocked(key, value)
		}
		m.mu.Unlock()
		if err != nil {
			return readMemoValue{}, err
		}
		if value.unstored {
			// The build saw its inputs move and answers once, untagged.
			return value.clone(), nil
		}
		if changed && builds >= readMemoRebuilds {
			value.etag, value.unstored = "", true
			return value.clone(), nil
		}
		// A build that outlived its horizon is never stored or tagged. When
		// only the memo lifetime, the minute boundary, or a display instant
		// passed during the build, it answers its caller once, untagged,
		// projected at the build's start: an uncached render also projects
		// at the request's start, so this answer promises no freshness past
		// that instant. When a real projection deadline passed during the
		// build, such as a review lease or a stated deadline, even one later
		// than the lifetime, the memo builds again at a fresh instant. One
		// request builds again for deadlines at most readMemoRebuilds-1
		// times in all its nested memo reads, so it renders a memo read at
		// most readMemoRebuilds times for deadlines, as a per-task statement
		// read projects at most statementReadAttempts times. The last build
		// answers once, untagged. Building again after every expired build
		// repeated the whole read for as long as each build outlived the
		// lifetime (component-next-action invariant 58; DEC-140(3)).
		if expired && !changed {
			if !deadlinePassed || !budget.spend() {
				value.etag, value.unstored = "", true
				return value.clone(), nil
			}
		}
		if changed || expired {
			continue
		}
		return value.clone(), nil
	}
}
