package httpapi

import (
	"context"
	"errors"
	"fmt"
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
