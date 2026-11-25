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

// feedbackLimiter keeps the 5-second rule per key. Like
// passwordAttemptLimiter it is deliberately process-local and bounded: it
// adds no persistence, and each conveyord process keeps its own windows. A
// signal reserves its key before the store call, so two concurrent signals
// never both pass, and only a committed signal opens a window.
type feedbackLimiter struct {
	mu      sync.Mutex
	entries map[feedbackLimiterKey]feedbackLimiterEntry
	limit   int
}

func newFeedbackLimiter(limit int) *feedbackLimiter {
	return &feedbackLimiter{entries: make(map[feedbackLimiterKey]feedbackLimiterEntry), limit: limit}
}

// reserve admits one signal of the key that arrived at the given time. A
// refused signal gets the time to wait. full reports that every held key is
// inside its window, so the limiter cannot take one more without forgetting a
// live window. An admitted caller calls settle exactly once.
func (l *feedbackLimiter) reserve(key feedbackLimiterKey, arrived time.Time) (wait time.Duration, full bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, held := l.entries[key]
	switch {
	case entry.pending:
		return feedbackSignalWindow, false
	case held:
		// A signal that arrived before the accepted one waits no longer than
		// the window.
		if wait := feedbackSignalWindow - arrived.Sub(entry.accepted); wait > 0 {
			return min(wait, feedbackSignalWindow), false
		}
	case len(l.entries) >= l.limit:
		for other, state := range l.entries {
			if !state.pending && arrived.Sub(state.accepted) >= feedbackSignalWindow {
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
