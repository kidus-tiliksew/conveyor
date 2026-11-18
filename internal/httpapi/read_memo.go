package httpapi

import (
	"context"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
)

const readMemoCapacity = 128
const readMemoLifetime = 5 * time.Second

type requestConfigurationKey struct{}

type loadedRequestConfiguration struct {
	config *config.Config
}

func (s *Server) requestConfiguration(ctx context.Context) (context.Context, *config.Config, error) {
	if loaded, ok := ctx.Value(requestConfigurationKey{}).(loadedRequestConfiguration); ok {
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
	return context.WithValue(ctx, requestConfigurationKey{}, loadedRequestConfiguration{config: cfg}), cfg, nil
}

type readMemoHorizonKey struct{}

// readMemoHorizon belongs to one refresh, never to a cached response.
type readMemoHorizon struct {
	now   time.Time
	until time.Time
}

func (h *readMemoHorizon) include(until time.Time) {
	if until.After(h.now) && (h.until.IsZero() || until.Before(h.until)) {
		h.until = until
	}
}

func memoHorizon(ctx context.Context, until time.Time) {
	if h, ok := ctx.Value(readMemoHorizonKey{}).(*readMemoHorizon); ok {
		h.include(until)
	}
}

type readMemoValue struct {
	body       []byte
	etag       string
	validUntil time.Time
	header     map[string]string
}

func (v readMemoValue) clone() readMemoValue {
	v.body = append([]byte(nil), v.body...)
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
	notices    []*changes.Subscription
	clock      func() time.Time
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
	}
}

// get coalesces equal misses. Build must validate and build from one request's
// already authorized scope; errors and results invalidated during a build are
// never stored. The returned bytes and headers are private to the caller.
func (m *readMemo) get(ctx context.Context, key string, build func(context.Context, time.Time) (readMemoValue, error)) (readMemoValue, error) {
	for {
		if err := ctx.Err(); err != nil {
			return readMemoValue{}, err
		}
		m.mu.Lock()
		m.drainLocked()
		now := m.now()
		if entry, ok := m.entries[key]; ok && now.Before(entry.value.validUntil) {
			value := entry.value.clone()
			m.mu.Unlock()
			return value, nil
		}
		if flight := m.flights[key]; flight != nil {
			done := flight.done
			m.mu.Unlock()
			select {
			case <-done:
				if flight.err != nil {
					return readMemoValue{}, flight.err
				}
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
		m.mu.Lock()
		m.drainLocked()
		delete(m.flights, key)
		flight.err = err
		close(flight.done)
		changed := generation != m.generation
		if value.validUntil.After(now.Add(readMemoLifetime)) {
			value.validUntil = now.Add(readMemoLifetime)
		}
		expired := !m.now().Before(value.validUntil)
		if err == nil && !changed && !expired {
			if m.entries == nil {
				m.entries = make(map[string]readMemoEntry)
			}
			delete(m.entries, key)
			if len(m.entries) >= readMemoCapacity {
				var oldestKey string
				var oldest uint64
				for k, entry := range m.entries {
					if oldestKey == "" || entry.sequence < oldest {
						oldestKey, oldest = k, entry.sequence
					}
				}
				delete(m.entries, oldestKey)
			}
			m.sequence++
			m.entries[key] = readMemoEntry{value: value.clone(), sequence: m.sequence}
		}
		m.mu.Unlock()
		if err != nil {
			return readMemoValue{}, err
		}
		if changed || expired {
			continue
		}
		return value.clone(), nil
	}
}
