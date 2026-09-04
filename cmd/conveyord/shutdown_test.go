package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

type shutdownRecorder struct {
	mu        sync.Mutex
	steps     []string
	deadlines map[string]time.Time
}

func (r *shutdownRecorder) record(step string, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
	if deadline, ok := ctx.Deadline(); ok {
		r.deadlines[step] = deadline
	}
}

type fakeShutdownHTTP struct{ recorder *shutdownRecorder }

func (f fakeShutdownHTTP) Shutdown(ctx context.Context) error {
	f.recorder.record("http", ctx)
	return nil
}

type fakeShutdownQueue struct{ recorder *shutdownRecorder }

func (f fakeShutdownQueue) Stop(ctx context.Context) error {
	f.recorder.record("queue-soft", ctx)
	return nil
}

func (f fakeShutdownQueue) StopAndCancel(ctx context.Context) error {
	f.recorder.record("queue-hard", ctx)
	return nil
}

func TestConveyordShutdownOrdersPhasesAndSharesBudget(t *testing.T) {
	recorder := &shutdownRecorder{deadlines: map[string]time.Time{}}
	shutdown := conveyordShutdown{
		Timeout: 50 * time.Millisecond,
		HTTP:    fakeShutdownHTTP{recorder}, Queue: fakeShutdownQueue{recorder},
		CancelService: func() { recorder.record("service-cancel", context.Background()) },
		CancelHTTP:    func() { recorder.record("http-cancel", context.Background()) },
		CloseStore:    func() { recorder.record("store-close", context.Background()) },
	}
	shutdown.Run()

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	position := func(step string) int {
		for index, candidate := range recorder.steps {
			if candidate == step {
				return index
			}
		}
		return -1
	}
	for _, step := range []string{"service-cancel", "queue-soft", "http-cancel", "http", "queue-hard", "store-close"} {
		if position(step) < 0 {
			t.Fatalf("shutdown steps=%v, missing %s", recorder.steps, step)
		}
	}
	if position("service-cancel") > position("queue-soft") || position("http-cancel") > position("http") ||
		position("queue-hard") < position("queue-soft") || position("queue-hard") < position("http") ||
		position("store-close") != len(recorder.steps)-1 {
		t.Fatalf("shutdown steps=%v", recorder.steps)
	}
	if !recorder.deadlines["http"].Equal(recorder.deadlines["queue-hard"]) {
		t.Fatalf("HTTP deadline=%s hard-stop deadline=%s, want one overall budget", recorder.deadlines["http"], recorder.deadlines["queue-hard"])
	}
	if !recorder.deadlines["queue-soft"].Before(recorder.deadlines["queue-hard"]) {
		t.Fatalf("soft-stop deadline=%s hard-stop deadline=%s, want hard-stop reserve", recorder.deadlines["queue-soft"], recorder.deadlines["queue-hard"])
	}
}

func TestShutdownDrainWindowReservesHardStopTime(t *testing.T) {
	if got := shutdownDrainWindow(25 * time.Second); got != 20*time.Second {
		t.Fatalf("default drain window=%s, want 20s", got)
	}
	if got := shutdownDrainWindow(8 * time.Second); got != 4*time.Second {
		t.Fatalf("short drain window=%s, want 4s", got)
	}
}
