package httpapi

import (
	"slices"
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

// feedbackReservation is one signal of a key on its way to the store. done is
// closed when it is settled.
type feedbackReservation struct {
	at   time.Time
	done chan struct{}
}

type feedbackLimiterEntry struct {
	// accepted holds arrivals of accepted signals, oldest first: the newest
	// one, and each older one that a request in progress arrived near.
	accepted []time.Time
	pending  *feedbackReservation
}

// feedbackNear reports whether two arrivals are too close for both signals to
// be accepted, in either order.
func feedbackNear(a, b time.Time) bool { return a.Sub(b).Abs() < feedbackSignalWindow }

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
// Any two accepted signals of a key arrived at least feedbackSignalWindow
// apart, and a signal that far from each of them is not refused. A request
// reaches reserve after its checks, so a later one can overtake it. The limiter
// therefore holds each request in progress and forgets no accepted arrival that
// one of them, or a request still to come, can be near. limit bounds the keys.
// The accepted arrivals of a key are that far apart, so a request in progress
// is near at most two. A key forgets the others when a request of it ends, when
// it accepts a signal, and when the limiter needs room, so it holds at most one
// and two for each request that was in progress at that time.
type feedbackLimiter struct {
	mu      sync.Mutex
	entries map[feedbackLimiterKey]*feedbackLimiterEntry
	limit   int
	// waiting holds the requests in progress by key. The zero key holds those
	// whose key is not verified yet.
	waiting map[feedbackLimiterKey]map[*feedbackArrival]struct{}
}

func newFeedbackLimiter(limit int) *feedbackLimiter {
	return &feedbackLimiter{
		entries: make(map[feedbackLimiterKey]*feedbackLimiterEntry), limit: limit,
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
// keeps only accepted arrivals of that key.
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
	if entry := l.entries[arrival.key]; entry != nil {
		l.prune(arrival.key, entry)
	}
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

// needed reports whether a request in progress that can be of the key arrived
// near an accepted arrival.
func (l *feedbackLimiter) needed(key feedbackLimiterKey, accepted time.Time) bool {
	for _, held := range []map[*feedbackArrival]struct{}{l.waiting[key], l.waiting[feedbackLimiterKey{}]} {
		for arrival := range held {
			if feedbackNear(arrival.at, accepted) {
				return true
			}
		}
	}
	return false
}

// prune forgets the older accepted arrivals of a key that no request in
// progress needs. A request still to come arrives after the newest one, which
// is a whole window after each older one, so it can be near the newest only.
func (l *feedbackLimiter) prune(key feedbackLimiterKey, entry *feedbackLimiterEntry) {
	newest := len(entry.accepted) - 1
	kept := entry.accepted[:0]
	for i, accepted := range entry.accepted {
		if i == newest || l.needed(key, accepted) {
			kept = append(kept, accepted)
		}
	}
	entry.accepted = kept
}

// idle reports whether the limiter can forget a key at now: no signal of it is
// on its way, and no request, in progress or still to come, can need it.
func (l *feedbackLimiter) idle(key feedbackLimiterKey, entry *feedbackLimiterEntry, now time.Time) bool {
	if entry.pending != nil {
		return false
	}
	l.prune(key, entry)
	for _, accepted := range entry.accepted {
		if now.Sub(accepted) < feedbackSignalWindow || l.needed(key, accepted) {
			return false
		}
	}
	return true
}

// feedbackDecision is the limiter's answer to one signal. At most one member
// is set, and none admits the signal.
type feedbackDecision struct {
	// wait refuses the signal and says how long to wait from now. It is more
	// than zero.
	wait time.Duration
	// busy closes when the signal of the key that is on its way is settled.
	// This signal is far enough from it, so the caller asks again then.
	busy <-chan struct{}
	// full reports that the limiter cannot take one more key without
	// forgetting one that a request can still need.
	full bool
}

// reserve decides on one signal of the key that arrived at the given time. An
// admitted caller calls settle exactly once.
func (l *feedbackLimiter) reserve(key feedbackLimiterKey, arrived, now time.Time) feedbackDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if entry != nil {
		near := slices.ContainsFunc(entry.accepted, func(accepted time.Time) bool { return feedbackNear(arrived, accepted) })
		if near || (entry.pending != nil && feedbackNear(arrived, entry.pending.at)) {
			return feedbackDecision{wait: entry.wait(now)}
		}
		if entry.pending != nil {
			return feedbackDecision{busy: entry.pending.done}
		}
	} else {
		if len(l.entries) >= l.limit {
			for other, state := range l.entries {
				if l.idle(other, state, now) {
					delete(l.entries, other)
				}
			}
			if len(l.entries) >= l.limit {
				return feedbackDecision{full: true}
			}
		}
		entry = &feedbackLimiterEntry{}
		l.entries[key] = entry
	}
	entry.pending = &feedbackReservation{at: arrived, done: make(chan struct{})}
	return feedbackDecision{}
}

// wait is how long from now a new signal of the key is refused, if the signal
// on its way is accepted. The window of a request that took long can be over,
// so it can be as little as the least wait.
func (e *feedbackLimiterEntry) wait(now time.Time) time.Duration {
	var last time.Time
	if len(e.accepted) > 0 {
		last = e.accepted[len(e.accepted)-1]
	}
	if e.pending != nil && e.pending.at.After(last) {
		last = e.pending.at
	}
	return min(max(feedbackSignalWindow-now.Sub(last), 1), feedbackSignalWindow)
}

// settle ends a reservation. An accepted signal adds its arrival and replaces
// no other one. A refused or failed one leaves the key as it was.
func (l *feedbackLimiter) settle(key feedbackLimiterKey, accepted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if accepted {
		entry.accepted = append(entry.accepted, entry.pending.at)
		slices.SortFunc(entry.accepted, time.Time.Compare)
	}
	close(entry.pending.done)
	entry.pending = nil
	if len(entry.accepted) == 0 {
		delete(l.entries, key)
		return
	}
	l.prune(key, entry)
}
