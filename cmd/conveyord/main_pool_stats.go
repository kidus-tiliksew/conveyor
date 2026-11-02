package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// poolStatsInterval is how often the daemon logs a pool snapshot.
const poolStatsInterval = 30 * time.Second

// readStatsTopLabels is how many labels the read statistics line names.
const readStatsTopLabels = 10

// poolStatsLogger logs process-wide pool counters and the interval's
// database read statistics on every tick until it is stopped. Startup
// already logged the effective maximum, so it logs nothing at start
// (component-runtime, Start-up checks and background loops).
type poolStatsLogger struct {
	stats    backend.Stats
	logf     func(string, ...any)
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	stopTick func()
}

// startPoolStatsLogger derives from the service context; ticks come from a
// time.Ticker in production and a channel in tests.
func startPoolStatsLogger(parent context.Context, stats backend.Stats, ticks <-chan time.Time, stopTick func(), logf func(string, ...any)) *poolStatsLogger {
	ctx, cancel := context.WithCancel(parent)
	logger := &poolStatsLogger{stats: stats, logf: logf, cancel: cancel, done: make(chan struct{}), stopTick: stopTick}
	go func() {
		defer close(logger.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				// A tick that races cancellation must not log after Stop.
				if ctx.Err() != nil {
					return
				}
				logger.log("periodic")
			}
		}
	}()
	return logger
}

// Stop cancels the worker, waits for its goroutine to return, and stops the
// ticker. It never waits for a tick, and repeated calls are safe.
func (l *poolStatsLogger) Stop() {
	l.stopOnce.Do(func() {
		l.cancel()
		<-l.done
		if l.stopTick != nil {
			l.stopTick()
		}
	})
}

// log writes the pool line when the backend reports pool statistics, then
// the read line, which drains the interval's read counters.
func (l *poolStatsLogger) log(kind string) {
	if l.stats.Pool != nil {
		l.logf("%s", formatPoolStats(kind, l.stats.Pool()))
	}
	if l.stats.Reads != nil {
		l.logf("%s", formatReadStats(kind, l.stats.Reads(readStatsTopLabels)))
	}
}

// closeWith wraps the store's close: stop the worker, log the final
// snapshot, then close. The caller keeps sync.OnceFunc around the result.
func (l *poolStatsLogger) closeWith(closeStore func()) func() {
	return func() {
		l.Stop()
		l.log("final")
		closeStore()
	}
}

func formatPoolStats(kind string, stats dbmetrics.PoolStats) string {
	micros := int64(stats.AcquireDuration / time.Microsecond)
	return fmt.Sprintf("database pool stats (%s): acquired_conns=%d idle_conns=%d max_conns=%d empty_acquire_count=%d acquire_duration_ms=%d.%03d",
		kind, stats.AcquiredConns, stats.IdleConns, stats.MaxConns, stats.EmptyAcquireCount, micros/1000, micros%1000)
}

// formatReadStats renders one drained interval: its totals, the number of
// labels that received bytes, and the top labels by bytes received
// (component-persistence, Database read accounting).
func formatReadStats(kind string, interval dbmetrics.ReadInterval) string {
	var line strings.Builder
	fmt.Fprintf(&line, "database read stats (%s): bytes=%d rows=%d queries=%d labels=%d top=",
		kind, interval.Bytes, interval.Rows, interval.Queries, interval.Labels)
	if len(interval.Top) == 0 {
		line.WriteString("[]")
	}
	for i, total := range interval.Top {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "[%s bytes=%d rows=%d queries=%d]", total.Label, total.Bytes, total.Rows, total.Queries)
	}
	return line.String()
}
