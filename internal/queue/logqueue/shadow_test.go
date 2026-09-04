package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
)

func kindsOf(t *testing.T, log *memlog.Store, key string) []string {
	t.Helper()
	events, err := log.Read(context.Background(), "ws", StreamFor("demo", key), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func TestShadowVerdictsAndMirroring(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	now := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	clock := now
	shadow := NewShadow(log, ShadowOptions{Workspaces: []string{"ws"}, PollInterval: 10 * time.Millisecond, Now: func() time.Time { return clock }, Logf: t.Logf})
	if err := shadow.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shadow.Stop(stopCtx)
	}()
	args := func(key string) json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"key":%q}`, key)) }
	enqueue := func(key string) {
		t.Helper()
		if _, err := Enqueue(ctx, log, "ws", "demo", key, demoArgs{Key: key}, 3, clock); err != nil {
			t.Fatal(err)
		}
	}

	// Agreement: River claims the earliest enqueued job.
	enqueue("k0")
	enqueue("k1")
	shadow.Claimed(ctx, "ws", "demo", "k0", args("k0"), 1, 3)
	shadow.Outcome(ctx, "ws", "demo", "k0", 1, 3, nil, time.Time{})
	if got := kindsOf(t, log, "k0"); fmt.Sprint(got) != fmt.Sprint([]string{KindEnqueued, KindClaimed, KindCompleted}) {
		t.Fatalf("k0 kinds=%v", got)
	}

	// Order: River claims k3 while k1 and k2 are still ahead of it.
	enqueue("k2")
	enqueue("k3")
	shadow.Claimed(ctx, "ws", "demo", "k3", args("k3"), 1, 3)
	shadow.Outcome(ctx, "ws", "demo", "k3", 1, 3, errors.New("boom"), clock.Add(time.Minute))
	if got := kindsOf(t, log, "k3"); fmt.Sprint(got) != fmt.Sprint([]string{KindEnqueued, KindClaimed, KindFailed}) {
		t.Fatalf("k3 kinds=%v", got)
	}

	// Not claimable: k3 is scheduled a minute out; River retries early.
	shadow.Claimed(ctx, "ws", "demo", "k3", args("k3"), 2, 3)
	shadow.Outcome(ctx, "ws", "demo", "k3", 2, 3, queueargs.Snooze(time.Second), time.Time{})
	job, _ := Load(ctx, log, "ws", StreamFor("demo", "k3"))
	if job.State != StateScheduled || job.Attempt != 1 {
		t.Fatalf("k3 after snooze=%+v", job)
	}

	// Unknown: River runs a job the log never saw; the mirror creates it.
	shadow.Claimed(ctx, "ws", "demo", "k9", args("k9"), 1, 3)
	shadow.Outcome(ctx, "ws", "demo", "k9", 3, 3, errors.New("final"), time.Time{})
	if got := kindsOf(t, log, "k9"); fmt.Sprint(got) != fmt.Sprint([]string{KindEnqueued, KindClaimed, KindDiscarded}) {
		t.Fatalf("k9 kinds=%v", got)
	}

	// An outcome with no open claim is a mirror error, not a crash.
	shadow.Outcome(ctx, "ws", "demo", "k1", 1, 3, nil, time.Time{})

	report := shadow.Report()
	if len(report.Counts) != 1 {
		t.Fatalf("counts=%+v", report.Counts)
	}
	c := report.Counts[0]
	if c.Claims != 4 || c.Agree != 1 || c.Order != 1 || c.NotClaimable != 1 || c.Unknown != 1 || c.Mirrored != 8 || c.MirrorErrors != 1 {
		t.Fatalf("counts=%+v", c)
	}
	if report.Clean() {
		t.Fatal("report clean despite unknown and not-claimable verdicts")
	}
	if len(report.Recent) != 3 || report.Recent[0].Verdict != VerdictOrder || report.Recent[0].Expected != "k1" || report.Recent[1].Verdict != VerdictNotClaimable || report.Recent[2].Verdict != VerdictUnknown {
		t.Fatalf("recent=%+v", report.Recent)
	}
	if lines := shadow.Summary(); len(lines) != 1 || lines[0] == "" {
		t.Fatalf("summary=%v", lines)
	}

	// Clean run: only agreements.
	clean := NewShadow(memlog.New(), ShadowOptions{Workspaces: []string{"ws"}, PollInterval: 10 * time.Millisecond})
	if err := clean.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer clean.Stop(context.Background())
	if _, err := Enqueue(ctx, clean.log, "ws", "demo", "a", demoArgs{Key: "a"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	clean.Claimed(ctx, "ws", "demo", "a", args("a"), 1, 3)
	clean.Outcome(ctx, "ws", "demo", "a", 1, 3, nil, time.Time{})
	if r := clean.Report(); !r.Clean() || r.Counts[0].Agree != 1 {
		t.Fatalf("clean report=%+v", r)
	}
	if lines := NewShadow(memlog.New(), ShadowOptions{}).Summary(); len(lines) != 1 {
		t.Fatalf("empty summary=%v", lines)
	}
}
