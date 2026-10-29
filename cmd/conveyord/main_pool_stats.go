package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// poolStatsInterval is how often the daemon logs a pool snapshot.
const poolStatsInterval = 30 * time.Second

// poolStatsLogger logs process-wide pool counters on every tick until it is
// stopped. Startup already logged the effective maximum, so it logs nothing
// at start (component-runtime, Reconcile tick and background loops).
type poolStatsLogger struct {
	read     func() dbmetrics.PoolStats
	logf     func(string, ...any)
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	stopTick func()
}

// startPoolStatsLogger derives from the service context; ticks come from a
// time.Ticker in production and a channel in tests.
func startPoolStatsLogger(parent context.Context, read func() dbmetrics.PoolStats, ticks <-chan time.Time, stopTick func(), logf func(string, ...any)) *poolStatsLogger {
	ctx, cancel := context.WithCancel(parent)
	logger := &poolStatsLogger{read: read, logf: logf, cancel: cancel, done: make(chan struct{}), stopTick: stopTick}
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

func (l *poolStatsLogger) log(kind string) {
	l.logf("%s", formatPoolStats(kind, l.read()))
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
