// Package logtest is the eventlog conformance suite. Every driver runs it;
// passing it is what "implements eventlog.Store" means.
package logtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Factory returns a fresh store per subtest. Drivers backed by shared
// infrastructure isolate by schema, database, or unique workspace ids.
type Factory func(t *testing.T) eventlog.Store

// Run executes the conformance suite against stores produced by factory.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("append_new_stream_and_head", func(t *testing.T) { testAppendNewStream(t, factory(t)) })
	t.Run("expected_version_mismatch", func(t *testing.T) { testExpectedMismatch(t, factory(t)) })
	t.Run("expect_new_on_existing", func(t *testing.T) { testExpectNewOnExisting(t, factory(t)) })
	t.Run("expect_any", func(t *testing.T) { testExpectAny(t, factory(t)) })
	t.Run("batch_is_atomic", func(t *testing.T) { testBatchAtomic(t, factory(t)) })
	t.Run("validation", func(t *testing.T) { testValidation(t, factory(t)) })
	t.Run("normalisation", func(t *testing.T) { testNormalisation(t, factory(t)) })
	t.Run("read_after_and_limit", func(t *testing.T) { testReadAfterLimit(t, factory(t)) })
	t.Run("tail_positions", func(t *testing.T) { testTail(t, factory(t)) })
	t.Run("workspace_isolation", func(t *testing.T) { testWorkspaceIsolation(t, factory(t)) })
	t.Run("payload_is_copied", func(t *testing.T) { testPayloadCopied(t, factory(t)) })
	t.Run("concurrent_appends_serialise", func(t *testing.T) { testConcurrentAppends(t, factory(t)) })
}

func ws(t *testing.T) string {
	return fmt.Sprintf("ws-%s-%d", sanitize(t.Name()), time.Now().UnixNano())
}

func sanitize(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name) && len(out) < 40; i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func ev(kind string) eventlog.NewEvent {
	return eventlog.NewEvent{Kind: kind, ActorID: "actor-1", ActorRole: "human", Payload: json.RawMessage(`{"k":"` + kind + `"}`)}
}

func testAppendNewStream(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("task/t1")
	head, err := s.Head(ctx, w, stream)
	if err != nil || head != 0 {
		t.Fatalf("empty head=%d err=%v", head, err)
	}
	head, err = s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("task.created")})
	if err != nil || head != 1 {
		t.Fatalf("first append head=%d err=%v", head, err)
	}
	head, err = s.Append(ctx, w, stream, 1, []eventlog.NewEvent{ev("task.classified"), ev("task.state_changed")})
	if err != nil || head != 3 {
		t.Fatalf("second append head=%d err=%v", head, err)
	}
	events, err := s.Read(ctx, w, stream, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("read %d events, want 3", len(events))
	}
	for i, e := range events {
		if e.Version != eventlog.Version(i+1) {
			t.Fatalf("event %d version=%d", i, e.Version)
		}
		if e.Workspace != w || e.Stream != stream {
			t.Fatalf("event %d scope=%s/%s", i, e.Workspace, e.Stream)
		}
		if e.ActorID != "actor-1" || e.ActorRole != "human" {
			t.Fatalf("event %d actor=%s/%s", i, e.ActorID, e.ActorRole)
		}
		if i > 0 && events[i].Position <= events[i-1].Position {
			t.Fatalf("positions not increasing: %d then %d", events[i-1].Position, events[i].Position)
		}
	}
	if events[0].Kind != "task.created" || events[2].Kind != "task.state_changed" {
		t.Fatalf("kinds out of order: %+v", events)
	}
}

func testExpectedMismatch(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("work_order/wo1")
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("work_order.created"), ev("work_order.released")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append(ctx, w, stream, 1, []eventlog.NewEvent{ev("work_order.claimed")})
	var conflict *eventlog.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale append err=%v, want VersionConflictError", err)
	}
	if conflict.Expected != 1 || conflict.Actual != 2 || conflict.Stream != stream || conflict.Workspace != w {
		t.Fatalf("conflict=%+v", conflict)
	}
	if !eventlog.IsVersionConflict(err) {
		t.Fatal("IsVersionConflict false")
	}
	head, _ := s.Head(ctx, w, stream)
	if head != 2 {
		t.Fatalf("head after conflict=%d", head)
	}
	events, _ := s.Read(ctx, w, stream, 0, 0)
	if len(events) != 2 {
		t.Fatalf("conflict appended events: %d", len(events))
	}
	if _, err := s.Append(ctx, w, stream, 5, []eventlog.NewEvent{ev("x")}); !eventlog.IsVersionConflict(err) {
		t.Fatalf("future expected err=%v", err)
	}
}

func testExpectNewOnExisting(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("requirement/r1")
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("requirement.created")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("requirement.created")})
	var conflict *eventlog.VersionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != 1 {
		t.Fatalf("ExpectNew on existing err=%v", err)
	}
}

func testExpectAny(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("decision/DEC-1")
	for i := 1; i <= 3; i++ {
		head, err := s.Append(ctx, w, stream, eventlog.ExpectAny, []eventlog.NewEvent{ev("decision.proposed")})
		if err != nil || head != eventlog.Version(i) {
			t.Fatalf("ExpectAny append %d head=%d err=%v", i, head, err)
		}
	}
}

func testBatchAtomic(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("task/atomic")
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, w, stream, 0, []eventlog.NewEvent{ev("b"), ev("c"), ev("d")}); !eventlog.IsVersionConflict(err) {
		t.Fatalf("err=%v", err)
	}
	events, _ := s.Read(ctx, w, stream, 0, 0)
	if len(events) != 1 {
		t.Fatalf("partial batch persisted: %d events", len(events))
	}
	tail, _ := s.Tail(ctx, w, 0, 0)
	if len(tail) != 1 {
		t.Fatalf("partial batch visible in tail: %d", len(tail))
	}
}

func testValidation(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	cases := []struct {
		name      string
		workspace string
		stream    eventlog.StreamID
		events    []eventlog.NewEvent
		want      error
	}{
		{"empty workspace", "", eventlog.StreamID("task/x"), []eventlog.NewEvent{ev("a")}, eventlog.ErrEmptyWorkspace},
		{"bad stream", w, eventlog.StreamID("task"), []eventlog.NewEvent{ev("a")}, eventlog.ErrInvalidStream},
		{"bad stream slash", w, eventlog.StreamID("task/"), []eventlog.NewEvent{ev("a")}, eventlog.ErrInvalidStream},
		{"no events", w, eventlog.StreamID("task/x"), nil, eventlog.ErrEmptyAppend},
		{"empty kind", w, eventlog.StreamID("task/x"), []eventlog.NewEvent{{Kind: " "}}, eventlog.ErrEmptyKind},
	}
	for _, tc := range cases {
		if _, err := s.Append(ctx, tc.workspace, tc.stream, eventlog.ExpectAny, tc.events); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", tc.name, err, tc.want)
		}
	}
	if head, err := s.Head(ctx, w, eventlog.StreamID("task/x")); err != nil || head != 0 {
		t.Fatalf("invalid appends changed head=%d err=%v", head, err)
	}
}

func testNormalisation(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("task/norm")
	fixed := time.Date(2026, 9, 4, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	before := time.Now().Add(-time.Second)
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{
		{Kind: "stamped", ActorID: "a", ActorRole: "human"},
		{Kind: "fixed", ActorID: "a", ActorRole: "human", At: fixed, Payload: json.RawMessage(`{"n":1}`)},
	}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Read(ctx, w, stream, 0, 0)
	if len(events) != 2 {
		t.Fatalf("events=%d", len(events))
	}
	if events[0].At.Before(before) || events[0].At.After(time.Now().Add(time.Second)) {
		t.Fatalf("zero At not stamped: %v", events[0].At)
	}
	if string(events[0].Payload) != `{}` {
		t.Fatalf("nil payload stored as %q", events[0].Payload)
	}
	if !events[1].At.Equal(fixed.Truncate(time.Microsecond)) {
		t.Fatalf("At not kept to the microsecond: %v vs %v", events[1].At, fixed)
	}
	if events[1].At.Location() != time.UTC {
		t.Fatalf("At not UTC: %v", events[1].At.Location())
	}
	var payload map[string]int
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil || payload["n"] != 1 {
		t.Fatalf("payload=%s err=%v", events[1].Payload, err)
	}
}

func testReadAfterLimit(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("task/read")
	batch := make([]eventlog.NewEvent, 5)
	for i := range batch {
		batch[i] = ev(fmt.Sprintf("k%d", i+1))
	}
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, batch); err != nil {
		t.Fatal(err)
	}
	events, err := s.Read(ctx, w, stream, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Version != 3 || events[1].Version != 4 {
		t.Fatalf("read after=2 limit=2: %+v", versions(events))
	}
	events, _ = s.Read(ctx, w, stream, 5, 0)
	if len(events) != 0 {
		t.Fatalf("read past head returned %d", len(events))
	}
	events, _ = s.Read(ctx, w, stream, 99, 0)
	if len(events) != 0 {
		t.Fatalf("read far past head returned %d", len(events))
	}
	events, _ = s.Read(ctx, w, eventlog.StreamID("task/missing"), 0, 0)
	if len(events) != 0 {
		t.Fatalf("missing stream returned %d", len(events))
	}
}

func testTail(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	a, b := eventlog.StreamID("task/a"), eventlog.StreamID("requirement/b")
	if _, err := s.Append(ctx, w, a, eventlog.ExpectNew, []eventlog.NewEvent{ev("a1"), ev("a2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, w, b, eventlog.ExpectNew, []eventlog.NewEvent{ev("b1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, w, a, 2, []eventlog.NewEvent{ev("a3")}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Tail(ctx, w, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(all); fmt.Sprint(got) != "[a1 a2 b1 a3]" {
		t.Fatalf("tail order=%v", got)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Position <= all[i-1].Position {
			t.Fatalf("positions not strictly increasing: %v", positions(all))
		}
	}
	page, _ := s.Tail(ctx, w, all[1].Position, 1)
	if len(page) != 1 || page[0].Kind != "b1" {
		t.Fatalf("tail after=%d limit=1: %v", all[1].Position, kinds(page))
	}
	rest, _ := s.Tail(ctx, w, all[3].Position, 0)
	if len(rest) != 0 {
		t.Fatalf("tail past end=%v", kinds(rest))
	}
}

func testWorkspaceIsolation(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w1, w2 := ws(t)+"-1", ws(t)+"-2"
	stream := eventlog.StreamID("task/shared-id")
	if _, err := s.Append(ctx, w1, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("w1")}); err != nil {
		t.Fatal(err)
	}
	head, err := s.Append(ctx, w2, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("w2a"), ev("w2b")})
	if err != nil || head != 2 {
		t.Fatalf("same stream id in second workspace head=%d err=%v", head, err)
	}
	if h, _ := s.Head(ctx, w1, stream); h != 1 {
		t.Fatalf("w1 head=%d", h)
	}
	t1, _ := s.Tail(ctx, w1, 0, 0)
	t2, _ := s.Tail(ctx, w2, 0, 0)
	if fmt.Sprint(kinds(t1)) != "[w1]" || fmt.Sprint(kinds(t2)) != "[w2a w2b]" {
		t.Fatalf("tails leaked: %v / %v", kinds(t1), kinds(t2))
	}
}

func testPayloadCopied(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("task/copy")
	payload := []byte(`{"v":1}`)
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "k", Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	payload[5] = '2'
	events, _ := s.Read(ctx, w, stream, 0, 0)
	if !jsonEqual(events[0].Payload, `{"v":1}`) {
		t.Fatalf("stored payload aliased caller buffer: %s", events[0].Payload)
	}
	for i := range events[0].Payload {
		events[0].Payload[i] = 'x'
	}
	again, _ := s.Read(ctx, w, stream, 0, 0)
	if !jsonEqual(again[0].Payload, `{"v":1}`) {
		t.Fatalf("returned payload aliased store buffer: %s", again[0].Payload)
	}
}

// jsonEqual compares payloads as JSON values. Drivers may re-serialise a
// payload (jsonb does); the contract is value equality, not byte equality.
func jsonEqual(got json.RawMessage, want string) bool {
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		return false
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return bytes.Equal(ga, gb)
}

func testConcurrentAppends(t *testing.T, s eventlog.Store) {
	ctx := context.Background()
	w := ws(t)
	stream := eventlog.StreamID("work_order/race")
	if _, err := s.Append(ctx, w, stream, eventlog.ExpectNew, []eventlog.NewEvent{ev("work_order.released")}); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	wins := make(chan int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Append(ctx, w, stream, 1, []eventlog.NewEvent{{Kind: "work_order.claimed", ActorID: fmt.Sprintf("worker-%d", i), ActorRole: "worker"}})
			if err == nil {
				wins <- i
				return
			}
			if !eventlog.IsVersionConflict(err) {
				t.Errorf("writer %d: unexpected err=%v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d writers won the claim, want exactly 1", n)
	}
	head, _ := s.Head(ctx, w, stream)
	if head != 2 {
		t.Fatalf("head after race=%d", head)
	}
	// ExpectAny writers all succeed and versions stay contiguous.
	var wg2 sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, err := s.Append(ctx, w, stream, eventlog.ExpectAny, []eventlog.NewEvent{ev("work_order.lease_renewed")}); err != nil {
				t.Errorf("ExpectAny append err=%v", err)
			}
		}()
	}
	wg2.Wait()
	events, _ := s.Read(ctx, w, stream, 0, 0)
	if len(events) != 2+writers {
		t.Fatalf("events=%d want %d", len(events), 2+writers)
	}
	for i, e := range events {
		if e.Version != eventlog.Version(i+1) {
			t.Fatalf("versions not contiguous: %v", versions(events))
		}
	}
}

func kinds(events []eventlog.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func versions(events []eventlog.Event) []eventlog.Version {
	out := make([]eventlog.Version, len(events))
	for i, e := range events {
		out[i] = e.Version
	}
	return out
}

func positions(events []eventlog.Event) []eventlog.Position {
	out := make([]eventlog.Position, len(events))
	for i, e := range events {
		out[i] = e.Position
	}
	return out
}
