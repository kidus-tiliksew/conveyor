// Package dbmetrics is the driver-neutral boundary for request database
// metrics and process-wide pool statistics. It imports only the standard
// library, so the HTTP layer and the daemon can read what a backend measures
// without importing a driver (component-persistence, Request database
// metrics and pool sizing).
package dbmetrics

import (
	"context"
	"math"
	"sync/atomic"
	"time"
)

// Capability is the set of request metrics a backend can measure.
type Capability uint8

const (
	// QueryCount counts query operations.
	QueryCount Capability = 1 << iota
	// QueryDuration sums query operation lifetimes.
	QueryDuration
	// AcquireDuration sums pool acquisition latency.
	AcquireDuration
)

// RequestMeasurer is the optional interface a backend implements when it
// records into a request's Collector. It is not part of store.Backend.
type RequestMeasurer interface {
	RequestMetrics() Capability
}

// CapabilityOf returns the request metrics v measures, or zero.
func CapabilityOf(v any) Capability {
	if measurer, ok := v.(RequestMeasurer); ok {
		return measurer.RequestMetrics()
	}
	return 0
}

// Collector accumulates one request's database metrics. It holds exactly
// three fixed-size counters and no query text, argument, error, identifier,
// actor, or workspace value. All methods are safe for concurrent use and are
// no-ops on a nil collector.
type Collector struct {
	queries      atomic.Uint64
	queryNanos   atomic.Int64
	acquireNanos atomic.Int64
}

// Snapshot is a plain copy of a collector's counters.
type Snapshot struct {
	Queries         uint64
	QueryDuration   time.Duration
	AcquireDuration time.Duration
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{} }

// AddQuery records one query operation that lasted d.
func (c *Collector) AddQuery(d time.Duration) {
	if c == nil {
		return
	}
	for {
		current := c.queries.Load()
		if current == math.MaxUint64 || c.queries.CompareAndSwap(current, current+1) {
			break
		}
	}
	addSaturating(&c.queryNanos, d)
}

// AddQueryDuration adds database time that is not a query operation, such
// as statement preparation, without counting a query.
func (c *Collector) AddQueryDuration(d time.Duration) {
	if c == nil {
		return
	}
	addSaturating(&c.queryNanos, d)
}

// AddAcquire records one pool acquisition that took d.
func (c *Collector) AddAcquire(d time.Duration) {
	if c == nil {
		return
	}
	addSaturating(&c.acquireNanos, d)
}

// Snapshot returns the current counters.
func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	return Snapshot{
		Queries:         c.queries.Load(),
		QueryDuration:   time.Duration(c.queryNanos.Load()),
		AcquireDuration: time.Duration(c.acquireNanos.Load()),
	}
}

// addSaturating adds a non-negative duration, stopping at the maximum.
func addSaturating(sum *atomic.Int64, d time.Duration) {
	if d <= 0 {
		return
	}
	for {
		current := sum.Load()
		next := current + int64(d)
		if next < current {
			next = math.MaxInt64
		}
		if current == next || sum.CompareAndSwap(current, next) {
			return
		}
	}
}

type collectorKey struct{}

// WithCollector returns ctx carrying c.
func WithCollector(ctx context.Context, c *Collector) context.Context {
	return context.WithValue(ctx, collectorKey{}, c)
}

// FromContext returns the request's collector, or nil for work that no
// request started.
func FromContext(ctx context.Context) *Collector {
	c, _ := ctx.Value(collectorKey{}).(*Collector)
	return c
}

// PoolStats is a process-wide pool snapshot. The counts are cumulative over
// the pool's lifetime where noted; they are not request metrics.
type PoolStats struct {
	AcquiredConns int32
	IdleConns     int32
	MaxConns      int32
	// EmptyAcquireCount is the cumulative number of acquisitions that had to
	// wait because no idle connection was available.
	EmptyAcquireCount int64
	// AcquireDuration is the cumulative time spent acquiring connections.
	AcquireDuration time.Duration
}
