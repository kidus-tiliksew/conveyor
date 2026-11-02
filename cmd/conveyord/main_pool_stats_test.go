package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var testPoolStats = dbmetrics.PoolStats{AcquiredConns: 3, IdleConns: 5, MaxConns: 32, EmptyAcquireCount: 7, AcquireDuration: 1234567 * time.Nanosecond}

const testPoolStatsFields = "acquired_conns=3 idle_conns=5 max_conns=32 empty_acquire_count=7 acquire_duration_ms=1.234"

// poolOnly reports pool statistics and no read statistics.
var poolOnly = backend.Stats{Pool: func() dbmetrics.PoolStats { return testPoolStats }}

var testReadInterval = dbmetrics.ReadInterval{Bytes: 1200, Rows: 7, Queries: 4, Labels: 2, Top: []dbmetrics.ReadTotal{
	{Label: "ListWorkOrders", Bytes: 1000, Rows: 6, Queries: 2},
	{Label: "unlabeled", Bytes: 200, Rows: 1, Queries: 2},
}}

const testReadStatsFields = "bytes=1200 rows=7 queries=4 labels=2 top=[ListWorkOrders bytes=1000 rows=6 queries=2] [unlabeled bytes=200 rows=1 queries=2]"

// drainingReads returns a read statistics function that records each
// requested top count and returns testReadInterval.
func drainingReads(tops chan<- int) backend.ReadStatsFunc {
	return func(top int) dbmetrics.ReadInterval {
		tops <- top
		return testReadInterval
	}
}

func TestFormatReadStats(t *testing.T) {
	if got := formatReadStats("periodic", testReadInterval); got != "database read stats (periodic): "+testReadStatsFields {
		t.Fatalf("line = %q", got)
	}
	if got := formatReadStats("final", dbmetrics.ReadInterval{}); got != "database read stats (final): bytes=0 rows=0 queries=0 labels=0 top=[]" {
		t.Fatalf("line = %q", got)
	}
}

func TestFormatPoolStats(t *testing.T) {
	if got := formatPoolStats("periodic", testPoolStats); got != "database pool stats (periodic): "+testPoolStatsFields {
		t.Fatalf("line = %q", got)
	}
	if got := formatPoolStats("final", dbmetrics.PoolStats{MaxConns: 4, AcquireDuration: 2*time.Second + 5*time.Microsecond}); got != "database pool stats (final): acquired_conns=0 idle_conns=0 max_conns=4 empty_acquire_count=0 acquire_duration_ms=2000.005" {
		t.Fatalf("line = %q", got)
	}
}

// lineSink delivers every log line on a channel.
func lineSink() (func(string, ...any), chan string) {
	lines := make(chan string, 16)
	return func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }, lines
}

func TestPoolStatsLoggerLogsEachTick(t *testing.T) {
	logf, lines := lineSink()
	ticks := make(chan time.Time)
	logger := startPoolStatsLogger(context.Background(), poolOnly, ticks, nil, logf)
	defer logger.Stop()
	for range 2 {
		ticks <- time.Now()
		if line := <-lines; line != "database pool stats (periodic): "+testPoolStatsFields {
			t.Fatalf("line = %q", line)
		}
	}
	select {
	case line := <-lines:
		t.Fatalf("unexpected line %q", line)
	default:
	}

	// With both functions, as on PostgreSQL, each tick logs the pool line
	// and then the read line, draining the read counters once.
	tops := make(chan int, 4)
	both := backend.Stats{Pool: poolOnly.Pool, Reads: drainingReads(tops)}
	bothTicks := make(chan time.Time)
	bothLogger := startPoolStatsLogger(context.Background(), both, bothTicks, nil, logf)
	defer bothLogger.Stop()
	for range 2 {
		bothTicks <- time.Now()
		if line := <-lines; line != "database pool stats (periodic): "+testPoolStatsFields {
			t.Fatalf("pool line = %q", line)
		}
		if line := <-lines; line != "database read stats (periodic): "+testReadStatsFields {
			t.Fatalf("read line = %q", line)
		}
		if top := <-tops; top != readStatsTopLabels {
			t.Fatalf("drained top %d", top)
		}
	}
	select {
	case line := <-lines:
		t.Fatalf("unexpected line %q", line)
	default:
	}
}

func TestPoolStatsLoggerStopJoinsWorker(t *testing.T) {
	logf, lines := lineSink()
	ticks := make(chan time.Time, 1)
	tickerStops := 0
	logger := startPoolStatsLogger(context.Background(), poolOnly, ticks, func() { tickerStops++ }, logf)
	logger.Stop()
	select {
	case <-logger.done:
	default:
		t.Fatal("Stop returned before the worker exited")
	}
	// The worker has exited, so a later tick is never read or logged.
	ticks <- time.Now()
	logger.Stop()
	if tickerStops != 1 {
		t.Fatalf("ticker stopped %d times", tickerStops)
	}
	select {
	case line := <-lines:
		t.Fatalf("logged after Stop: %q", line)
	default:
	}
	if len(ticks) != 1 {
		t.Fatal("the tick after Stop was consumed")
	}
}

func TestPoolStatsLoggerExitsOnServiceCancellation(t *testing.T) {
	logf, _ := lineSink()
	ctx, cancel := context.WithCancel(context.Background())
	logger := startPoolStatsLogger(ctx, poolOnly, make(chan time.Time), nil, logf)
	cancel()
	<-logger.done
	logger.Stop()
}

func TestPoolStatsCloseLogsFinalSnapshotBeforeSingleClose(t *testing.T) {
	var mu sync.Mutex
	var events []string
	logged := make(chan struct{}, 4)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	recorder := &shutdownRecorder{deadlines: map[string]time.Time{}}
	ticks := make(chan time.Time)
	serviceCtx, cancelService := context.WithCancel(context.Background())
	logger := startPoolStatsLogger(serviceCtx, poolOnly, ticks, func() { record("ticker-stop") }, func(format string, args ...any) {
		record(fmt.Sprintf(format, args...))
		logged <- struct{}{}
	})
	// One periodic tick before shutdown, observed through the worker.
	ticks <- time.Now()
	<-logged
	closeStore := sync.OnceFunc(logger.closeWith(func() { record("store-close") }))
	shutdown := conveyordShutdown{
		Timeout: 10 * time.Second,
		HTTP:    fakeShutdownHTTP{recorder}, Queue: fakeShutdownQueue{recorder},
		CancelService: func() { record("service-cancel"); cancelService() },
		CancelHTTP:    func() {},
		CloseStore:    closeStore,
		Logf: func(format string, args ...any) {
			if strings.HasPrefix(format, "shutdown phase=") && len(args) > 0 && args[0] == "store-closed" {
				record("store-closed-phase")
			}
		},
	}
	shutdown.Run()
	closeStore()

	mu.Lock()
	defer mu.Unlock()
	periodic := 0
	for _, event := range events {
		if event == "database pool stats (periodic): "+testPoolStatsFields {
			periodic++
		}
	}
	tail := events[len(events)-4:]
	want := []string{"ticker-stop", "database pool stats (final): " + testPoolStatsFields, "store-close", "store-closed-phase"}
	if strings.Join(tail, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %q", events)
	}
	closes := 0
	for _, event := range events {
		if event == "store-close" {
			closes++
		}
	}
	if closes != 1 || periodic < 1 {
		t.Fatalf("closes %d periodic %d events %q", closes, periodic, events)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if strings.Join(recorder.steps, ",") != "queue-soft,http,queue-hard" && strings.Join(recorder.steps, ",") != "http,queue-soft,queue-hard" {
		t.Fatalf("drain steps = %v", recorder.steps)
	}
}

// TestPoolStatsLoggerLogsReadsWithoutPool runs the worker as SingleStore
// does: read statistics only, one read line per tick, and a final read line
// through the wrapped close.
func TestPoolStatsLoggerLogsReadsWithoutPool(t *testing.T) {
	logf, lines := lineSink()
	tops := make(chan int, 4)
	ticks := make(chan time.Time)
	logger := startPoolStatsLogger(context.Background(), backend.Stats{Reads: drainingReads(tops)}, ticks, nil, logf)
	for range 2 {
		ticks <- time.Now()
		if line := <-lines; line != "database read stats (periodic): "+testReadStatsFields {
			t.Fatalf("line = %q", line)
		}
		if top := <-tops; top != readStatsTopLabels {
			t.Fatalf("drained top %d", top)
		}
	}
	closed := 0
	closeStore := sync.OnceFunc(logger.closeWith(func() { closed++ }))
	closeStore()
	closeStore()
	if line := <-lines; line != "database read stats (final): "+testReadStatsFields {
		t.Fatalf("final line = %q", line)
	}
	if closed != 1 || len(tops) != 1 {
		t.Fatalf("closes %d drains after ticks %d", closed, len(tops))
	}
	select {
	case line := <-lines:
		t.Fatalf("unexpected line %q", line)
	default:
	}
}
