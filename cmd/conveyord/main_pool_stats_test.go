package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var testPoolStats = dbmetrics.PoolStats{AcquiredConns: 3, IdleConns: 5, MaxConns: 32, EmptyAcquireCount: 7, AcquireDuration: 1234567 * time.Nanosecond}

const testPoolStatsFields = "acquired_conns=3 idle_conns=5 max_conns=32 empty_acquire_count=7 acquire_duration_ms=1.234"

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
	logger := startPoolStatsLogger(context.Background(), func() dbmetrics.PoolStats { return testPoolStats }, ticks, nil, logf)
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
}

func TestPoolStatsLoggerStopJoinsWorker(t *testing.T) {
	logf, lines := lineSink()
	ticks := make(chan time.Time, 1)
	tickerStops := 0
	logger := startPoolStatsLogger(context.Background(), func() dbmetrics.PoolStats { return testPoolStats }, ticks, func() { tickerStops++ }, logf)
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
	logger := startPoolStatsLogger(ctx, func() dbmetrics.PoolStats { return testPoolStats }, make(chan time.Time), nil, logf)
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
	logger := startPoolStatsLogger(serviceCtx, func() dbmetrics.PoolStats { return testPoolStats }, ticks, func() { record("ticker-stop") }, func(format string, args ...any) {
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
