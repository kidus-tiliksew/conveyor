package httpapi

import (
	"sync"
	"time"
)

const (
	// feedbackSignalWindow is how long after an accepted signal the same key
	// is refused (component-production-feedback, Refusals).
	feedbackSignalWindow = 5 * time.Second
	// feedbackLimiterMaxKeys bounds the keys held in memory.
	feedbackLimiterMaxKeys = 4096
)

// feedbackLimiterKey names one environment key by its stable ID, so the
// limiter holds no bearer value and two keys with one label stay apart.
type feedbackLimiterKey struct{ workspace, keyID string }

type feedbackLimiterEntry struct {
	// accepted is the arrival of the key's last accepted signal, and zero
	// before the first.
	accepted time.Time
	// pending marks a signal that is on its way to the store.
	pending bool
}

// feedbackArrival is one signal request from its arrival to its answer.
type feedbackArrival struct {
	at time.Time
	// key is the request's key once it is verified, and zero before that.
	key feedbackLimiterKey
}

// feedbackLimiter keeps the 5-second rule per key. Like
// passwordAttemptLimiter it is deliberately process-local and bounded: it
// adds no persistence, and each conveyord process keeps its own windows. A
// signal reserves its key before the store call, so two concurrent signals
// never both pass, and only a committed signal opens a window.
//
// A request reaches reserve after its checks, so a later one can overtake it.
// The limiter therefore holds each request in progress and forgets no window
// that one of them can still need. limit bounds the windows, and the server's
// own requests bound the ones in progress.
type feedbackLimiter struct {
	mu      sync.Mutex
	entries map[feedbackLimiterKey]feedbackLimiterEntry
	limit   int
	// waiting holds the requests in progress by key. The zero key holds those
	// whose key is not verified yet.
	waiting map[feedbackLimiterKey]map[*feedbackArrival]struct{}
}

func newFeedbackLimiter(limit int) *feedbackLimiter {
	return &feedbackLimiter{
		entries: make(map[feedbackLimiterKey]feedbackLimiterEntry), limit: limit,
		waiting: make(map[feedbackLimiterKey]map[*feedbackArrival]struct{}),
	}
}

// arrive records a request and its time of arrival. It reads the clock under
// the limiter's lock, so a request that is not held yet arrives no earlier
// than one that reserve already judged. The caller calls leave exactly once.
func (l *feedbackLimiter) arrive(clock func() time.Time) *feedbackArrival {
	l.mu.Lock()
	defer l.mu.Unlock()
	arrival := &feedbackArrival{at: clock()}
	l.hold(arrival)
	return arrival
}

// identify names the verified key of a request in progress. From then on it
// keeps only the window of that key.
func (l *feedbackLimiter) identify(arrival *feedbackArrival, key feedbackLimiterKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.release(arrival)
	arrival.key = key
	l.hold(arrival)
}

// leave ends a request.
func (l *feedbackLimiter) leave(arrival *feedbackArrival) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.release(arrival)
}

func (l *feedbackLimiter) hold(arrival *feedbackArrival) {
	held := l.waiting[arrival.key]
	if held == nil {
		held = make(map[*feedbackArrival]struct{})
		l.waiting[arrival.key] = held
	}
	held[arrival] = struct{}{}
}

func (l *feedbackLimiter) release(arrival *feedbackArrival) {
	held := l.waiting[arrival.key]
	delete(held, arrival)
	if len(held) == 0 {
		delete(l.waiting, arrival.key)
	}
}

// earliest returns the first arrival among the requests in progress of a key,
// or limit when none arrived before it.
func (l *feedbackLimiter) earliest(key feedbackLimiterKey, limit time.Time) time.Time {
	for arrival := range l.waiting[key] {
		if arrival.at.Before(limit) {
			limit = arrival.at
		}
	}
	return limit
}

// reserve admits one signal of the key that arrived at the given time. A
// refused signal gets the time to wait from now, which is more than zero. full
// reports that the limiter cannot take one more key without forgetting a
// window that is live for this signal or for a request in progress. An
// admitted caller calls settle exactly once.
func (l *feedbackLimiter) reserve(key feedbackLimiterKey, arrived, now time.Time) (wait time.Duration, full bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, held := l.entries[key]
	switch {
	case entry.pending:
		return feedbackSignalWindow, false
	case held:
		// The arrival decides, and a signal that arrived before the accepted
		// one is refused too. The wait counts from now: the window of a
		// request that took long can be over, so a retry can come at once.
		if arrived.Sub(entry.accepted) < feedbackSignalWindow {
			return min(max(feedbackSignalWindow-now.Sub(entry.accepted), 1), feedbackSignalWindow), false
		}
	case len(l.entries) >= l.limit:
		first := l.earliest(feedbackLimiterKey{}, arrived)
		for other, state := range l.entries {
			if !state.pending && l.earliest(other, first).Sub(state.accepted) >= feedbackSignalWindow {
				delete(l.entries, other)
			}
		}
		if len(l.entries) >= l.limit {
			return 0, true
		}
	}
	entry.pending = true
	l.entries[key] = entry
	return 0, false
}

// settle ends a reservation. An accepted signal opens the key's window at its
// arrival. A refused or failed one leaves the key as it was.
func (l *feedbackLimiter) settle(key feedbackLimiterKey, arrived time.Time, accepted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	entry.pending = false
	if accepted {
		entry.accepted = arrived
	}
	if entry.accepted.IsZero() {
		delete(l.entries, key)
		return
	}
	l.entries[key] = entry
}
