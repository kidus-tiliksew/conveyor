package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
)

func TestReadMemoExpiryInvalidationAndIsolation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &readMemo{clock: func() time.Time { return now }}
	hub := changes.NewHub()
	m.subscribe(hub)
	calls := 0
	build := func(_ context.Context, at time.Time) (readMemoValue, error) {
		calls++
		return readMemoValue{body: []byte(fmt.Sprint(calls)), header: map[string]string{"test": "original"}, validUntil: at.Add(time.Minute)}, nil
	}
	get := func(key string) readMemoValue {
		t.Helper()
		v, err := m.get(context.Background(), key, build)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := get("workspace-a")
	first.body[0] = 'x'
	first.header["test"] = "changed"
	warm := get("workspace-a")
	if string(warm.body) != "1" || warm.header["test"] != "original" || calls != 1 {
		t.Fatalf("warm value mutated: %q %v calls=%d", warm.body, warm.header, calls)
	}
	get("workspace-b")
	hub.Publish(changes.Notice{Kind: changes.Task, Workspace: "workspace-a"})
	hub.Publish(changes.Notice{Kind: changes.Task, Workspace: "workspace-b"})
	get("workspace-b")
	if calls != 3 {
		t.Fatalf("coalesced notices did not invalidate: calls=%d", calls)
	}
	now = now.Add(5 * time.Second)
	get("workspace-b")
	if calls != 4 {
		t.Fatalf("entry survived TTL: calls=%d", calls)
	}
}

func TestReadMemoDeadlineFailureAndEviction(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &readMemo{clock: func() time.Time { return now }}
	calls := 0
	fail := false
	build := func(_ context.Context, at time.Time) (readMemoValue, error) {
		calls++
		if fail {
			return readMemoValue{}, errors.New("refresh failed")
		}
		return readMemoValue{body: []byte("ok"), validUntil: at.Add(time.Second)}, nil
	}
	ctx := context.Background()
	if _, err := m.get(ctx, "deadline", build); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	fail = true
	if _, err := m.get(ctx, "deadline", build); err == nil {
		t.Fatal("expired entry served after failed refresh")
	}
	fail = false
	if _, err := m.get(ctx, "deadline", build); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("failure cached: calls=%d", calls)
	}
	for i := range readMemoCapacity {
		if _, err := m.get(ctx, fmt.Sprint(i), build); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.entries) != readMemoCapacity {
		t.Fatalf("entries=%d", len(m.entries))
	}
	before := calls
	if _, err := m.get(ctx, "deadline", build); err != nil {
		t.Fatal(err)
	}
	if calls != before+1 {
		t.Fatal("oldest entry was not evicted")
	}
}

func TestReadMemoDiscardsInvalidatedRefresh(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &readMemo{clock: func() time.Time { return now }}
	hub := changes.NewHub()
	m.subscribe(hub)
	calls := 0
	value, err := m.get(context.Background(), "key", func(_ context.Context, at time.Time) (readMemoValue, error) {
		calls++
		if calls == 1 {
			hub.Publish(changes.Notice{Kind: changes.Workspace, Workspace: "workspace"})
		}
		return readMemoValue{body: []byte(fmt.Sprint(calls)), validUntil: at.Add(time.Second)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(value.body) != "2" {
		t.Fatalf("invalidated refresh was served: calls=%d body=%q", calls, value.body)
	}
}

// TestReadMemoWaitersRetryAfterTheLeaderFails proves a leader's failure,
// its cancellation included, is never handed to a waiter: each live waiter
// builds again under its own context, and only its own failure answers it.
func TestReadMemoWaitersRetryAfterTheLeaderFails(t *testing.T) {
	for _, leaderFailure := range []string{"cancelled", "failed"} {
		t.Run(leaderFailure, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			const waiters = 4
			joined := make(chan struct{}, waiters)
			m := &readMemo{clock: func() time.Time { return now }, waiting: func() { joined <- struct{}{} }}
			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			defer cancelLeader()
			entered := make(chan struct{})
			release := make(chan struct{})
			var builds atomic.Int32
			build := func(ctx context.Context, at time.Time) (readMemoValue, error) {
				if builds.Add(1) == 1 {
					close(entered)
					<-release
					if leaderFailure == "cancelled" {
						cancelLeader()
						return readMemoValue{}, ctx.Err()
					}
					return readMemoValue{}, errors.New("leader read failed")
				}
				if err := ctx.Err(); err != nil {
					return readMemoValue{}, err
				}
				return readMemoValue{body: []byte("waiter"), validUntil: at.Add(time.Second)}, nil
			}
			leaderErr := make(chan error, 1)
			go func() {
				_, err := m.get(leaderCtx, "key", build)
				leaderErr <- err
			}()
			<-entered
			results := make(chan error, waiters)
			bodies := make(chan string, waiters)
			for range waiters {
				go func() {
					value, err := m.get(context.Background(), "key", build)
					bodies <- string(value.body)
					results <- err
				}()
			}
			// Every waiter joins the leader's flight before the leader fails.
			for range waiters {
				<-joined
			}
			close(release)
			if err := <-leaderErr; err == nil {
				t.Fatal("the leader's own failure was not returned to it")
			}
			for range waiters {
				if err := <-results; err != nil {
					t.Fatalf("a waiter received the leader's failure: %v", err)
				}
				if body := <-bodies; body != "waiter" {
					t.Fatalf("waiter body=%q", body)
				}
			}
			// One waiter rebuilt; the others waited on its flight and read
			// its stored entry.
			if got := builds.Load(); got != 2 {
				t.Fatalf("builds=%d, want the leader's and one waiter's", got)
			}
		})
	}
	t.Run("own cancellation", func(t *testing.T) {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		m := &readMemo{clock: func() time.Time { return now }}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := m.get(ctx, "key", func(context.Context, time.Time) (readMemoValue, error) {
			t.Fatal("a cancelled caller built")
			return readMemoValue{}, nil
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("own cancellation err=%v", err)
		}
	})
}

// TestReadMemoByteBudget proves the memo bounds its held encoded bytes:
// oldest entries are evicted first until a new entry fits, an entry larger
// than the budget is served and never stored, and the held total stays exact
// across replacement, expiry, and invalidation. The entry count and the
// five-second lifetime still apply.
func TestReadMemoByteBudget(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &readMemo{clock: func() time.Time { return now }, budget: 100}
	hub := changes.NewHub()
	m.subscribe(hub)
	builds := map[string]int{}
	sizes := map[string]int{}
	build := func(key string) func(context.Context, time.Time) (readMemoValue, error) {
		return func(_ context.Context, at time.Time) (readMemoValue, error) {
			builds[key]++
			return readMemoValue{body: make([]byte, sizes[key]), validUntil: at.Add(time.Minute)}, nil
		}
	}
	get := func(key string, size int) {
		t.Helper()
		sizes[key] = size
		value, err := m.get(context.Background(), key, build(key))
		if err != nil || len(value.body) != size {
			t.Fatalf("%s: body=%d err=%v", key, len(value.body), err)
		}
	}
	held := func() int {
		total := 0
		for _, entry := range m.entries {
			total += entry.value.size()
		}
		if total != m.bytes {
			t.Fatalf("held bytes=%d, entries hold %d", m.bytes, total)
		}
		return total
	}
	get("a", 40)
	get("b", 40)
	if held() != 80 {
		t.Fatalf("held=%d", held())
	}
	// c does not fit beside a and b: the oldest, a, is evicted.
	get("c", 40)
	if _, ok := m.entries["a"]; ok || held() != 80 {
		t.Fatalf("oldest entry kept: held=%d entries=%d", held(), len(m.entries))
	}
	get("b", 40)
	if builds["b"] != 1 {
		t.Fatal("b was evicted instead of the oldest entry")
	}
	// An entry larger than the budget is answered and never stored.
	get("huge", 101)
	get("huge", 101)
	if builds["huge"] != 2 || held() != 80 {
		t.Fatalf("oversized entry stored: builds=%d held=%d", builds["huge"], held())
	}
	// Replacement recounts the key once.
	m.mu.Lock()
	m.storeLocked("b", readMemoValue{body: make([]byte, 10), validUntil: now.Add(time.Minute)})
	m.mu.Unlock()
	if held() != 50 {
		t.Fatalf("replacement held=%d, want 50", held())
	}
	// An expired entry is dropped when read and rebuilt.
	now = now.Add(readMemoLifetime)
	get("c", 20)
	if builds["c"] != 2 || held() != 30 {
		t.Fatalf("expiry held=%d builds=%d", held(), builds["c"])
	}
	// A notice drops every entry and its bytes.
	hub.Publish(changes.Notice{Kind: changes.Workspace, Workspace: "w"})
	get("d", 5)
	if held() != 5 || len(m.entries) != 1 {
		t.Fatalf("invalidation held=%d entries=%d", held(), len(m.entries))
	}
	if readMemoByteBudget != 8<<20 || readMemoCapacity != 128 || readMemoLifetime != 5*time.Second {
		t.Fatal("memo bounds changed")
	}
}

// TestReadMemoBoundsRebuildsAfterUnrelatedNotices proves a build that a
// notice discards is retried at most readMemoRebuilds times: a steady stream
// of notices cannot keep a caller rebuilding until its context ends. The
// last build answers its caller once, untagged and unstored.
func TestReadMemoBoundsRebuildsAfterUnrelatedNotices(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &readMemo{clock: func() time.Time { return now }}
	hub := changes.NewHub()
	m.subscribe(hub)
	builds := 0
	build := func(_ context.Context, at time.Time) (readMemoValue, error) {
		builds++
		hub.Publish(changes.Notice{Kind: changes.Workspace, Workspace: "another-workspace"})
		return readMemoValue{body: []byte("body"), etag: `"tag"`, validUntil: at.Add(time.Second)}, nil
	}
	value, err := m.get(context.Background(), "key", build)
	if err != nil {
		t.Fatal(err)
	}
	if builds != readMemoRebuilds || string(value.body) != "body" || value.etag != "" {
		t.Fatalf("builds=%d body=%q etag=%q", builds, value.body, value.etag)
	}
	if len(m.entries) != 0 {
		t.Fatal("a build a notice discarded was stored")
	}
}

// memoDeadlineBuild returns a build that takes step on the memo clock and
// reports as its deadline the first of deadlines after its start, or no
// deadline when none is left. It records each build's start.
func memoDeadlineBuild(now *time.Time, step time.Duration, deadlines []time.Time, starts *[]time.Time) func(context.Context, time.Time) (readMemoValue, error) {
	return func(_ context.Context, at time.Time) (readMemoValue, error) {
		*starts = append(*starts, at)
		*now = now.Add(step)
		value := readMemoValue{body: []byte(at.Format(time.RFC3339)), etag: `"tag"`, validUntil: at.Add(readMemoLifetime)}
		for _, deadline := range deadlines {
			if deadline.After(at) {
				value.deadline = deadline
				if deadline.Before(value.validUntil) {
					value.validUntil = deadline
				}
				break
			}
		}
		return value, nil
	}
}

// TestReadMemoAnswersAnExpiredBuildOnce proves a build that outlives its
// horizon is never stored or tagged, that it is not built again when only
// the memo lifetime passed during it, and that it is built again at a fresh
// instant when a real projection deadline passed during it, also a deadline
// later than the lifetime, at most readMemoRebuilds times in all.
func TestReadMemoAnswersAnExpiredBuildOnce(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		step       time.Duration
		deadlines  []time.Duration
		wantBuilds int
	}{
		{"only the lifetime passed", readMemoLifetime + time.Second, []time.Duration{time.Hour}, 1},
		{"no deadline was read", readMemoLifetime + time.Second, nil, 1},
		{"a deadline inside the lifetime passed", readMemoLifetime + time.Second, []time.Duration{2 * time.Second, time.Hour}, 2},
		{"a deadline after the lifetime passed", 7 * time.Second, []time.Duration{6 * time.Second, time.Hour}, 2},
		{"deadlines kept passing", 7 * time.Second, []time.Duration{6 * time.Second, 13 * time.Second, 20 * time.Second, 27 * time.Second}, readMemoRebuilds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := start
			m := &readMemo{clock: func() time.Time { return now }}
			var deadlines, starts []time.Time
			for _, d := range tc.deadlines {
				deadlines = append(deadlines, start.Add(d))
			}
			value, err := m.get(context.Background(), "key", memoDeadlineBuild(&now, tc.step, deadlines, &starts))
			if err != nil {
				t.Fatal(err)
			}
			if len(starts) != tc.wantBuilds || value.etag != "" || !value.unstored || len(m.entries) != 0 {
				t.Fatalf("builds=%d etag=%q unstored=%v entries=%d", len(starts), value.etag, value.unstored, len(m.entries))
			}
			// The answer is the last build, projected at that build's start.
			last := starts[len(starts)-1]
			if string(value.body) != last.Format(time.RFC3339) {
				t.Fatalf("answered %s, want the build at %s", value.body, last)
			}
			// Each build after the first starts past the deadline the
			// previous build crossed.
			for i := 1; i < len(starts); i++ {
				if !starts[i].After(deadlines[i-1]) {
					t.Fatalf("build %d started at %s, not past the deadline %s", i+1, starts[i], deadlines[i-1])
				}
			}
			// A build that finishes inside its horizon is still stored and
			// tagged.
			fast := func(_ context.Context, at time.Time) (readMemoValue, error) {
				return readMemoValue{body: []byte("fresh"), etag: `"fresh"`, validUntil: at.Add(readMemoLifetime), deadline: at.Add(time.Hour)}, nil
			}
			if value, err = m.get(context.Background(), "key", fast); err != nil || value.etag != `"fresh"` || len(m.entries) != 1 {
				t.Fatalf("fast build etag=%q entries=%d err=%v", value.etag, len(m.entries), err)
			}
		})
	}
}

// TestReadMemoRetriesADeadlineExpiryAfterANotice proves a notice-driven
// rebuild does not use up the builds that project past a deadline: a notice
// discards the first build, the second build crosses a deadline, and the
// third build projects past that deadline and answers untagged.
func TestReadMemoRetriesADeadlineExpiryAfterANotice(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	now := start
	m := &readMemo{clock: func() time.Time { return now }}
	hub := changes.NewHub()
	m.subscribe(hub)
	deadline := start.Add(2 * time.Second)
	var starts []time.Time
	timed := memoDeadlineBuild(&now, readMemoLifetime+time.Second, []time.Time{deadline, start.Add(time.Hour)}, &starts)
	build := func(ctx context.Context, at time.Time) (readMemoValue, error) {
		if len(starts) == 0 {
			starts = append(starts, at)
			hub.Publish(changes.Notice{Kind: changes.Workspace, Workspace: "another-workspace"})
			return readMemoValue{body: []byte("discarded"), etag: `"tag"`, validUntil: at.Add(time.Second), deadline: deadline}, nil
		}
		return timed(ctx, at)
	}
	value, err := m.get(context.Background(), "key", build)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 || value.etag != "" || !value.unstored || len(m.entries) != 0 {
		t.Fatalf("builds=%d etag=%q unstored=%v entries=%d", len(starts), value.etag, value.unstored, len(m.entries))
	}
	if !starts[2].After(deadline) || string(value.body) != starts[2].Format(time.RFC3339) {
		t.Fatalf("answered %s from the build at %s, want a build past %s", value.body, starts[2], deadline)
	}
}
