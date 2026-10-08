package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TestVerificationChunkExpiryGrouping pins the bounded expiry helpers: only
// the internal reconciler in an explicit workspace is admitted, candidates are
// ordered by expiry then ID and capped at the per-run limit, groups follow
// first expiry, the recheck drops rows that changed under the lock, and the
// audit event carries bounded metadata (component-verification-evidence).
func TestVerificationChunkExpiryGrouping(t *testing.T) {
	reconciler := WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "verification-reconciler", Role: core.ActorSystem})
	if _, _, err := AuthorizeVerificationChunkExpiry(reconciler); err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{
		"no workspace":    WithActor(context.Background(), Actor{ID: "verification-reconciler", Role: core.ActorSystem}),
		"other system":    WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "dispatcher", Role: core.ActorSystem}),
		"worker":          WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "verification-reconciler", Role: core.ActorWorker}),
		"no actor":        WithWorkspace(context.Background(), "demo"),
		"empty workspace": WithActor(WithWorkspace(context.Background(), ""), Actor{ID: "verification-reconciler", Role: core.ActorSystem}),
	} {
		if _, _, err := AuthorizeVerificationChunkExpiry(ctx); !errors.Is(err, ErrVerificationAccess) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	fixed := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, cutoff, _ := AuthorizeVerificationChunkExpiry(WithVerificationClockForTest(reconciler, func() time.Time { return fixed })); !cutoff.Equal(fixed) {
		t.Fatalf("cutoff ignored the verification clock: %s", cutoff)
	}
	for limit, want := range map[int]int{-1: 100, 0: 100, 1: 1, 100: 100, 101: 100} {
		if got := VerificationChunkExpiryLimitOrDefault(limit); got != want {
			t.Errorf("limit %d clamped to %d, want %d", limit, got, want)
		}
	}
	at := func(minutes int) time.Time { return fixed.Add(time.Duration(minutes) * time.Minute) }
	candidates := []VerificationChunkCandidate{
		{ID: "u2:0", TaskID: "task-b", UploadID: "u2", ExpiresAt: at(2)},
		{ID: "u1:1", TaskID: "task-a", UploadID: "u1", ExpiresAt: at(1)},
		{ID: "u1:0", TaskID: "task-a", UploadID: "u1", ExpiresAt: at(1)},
		{ID: "u3:0", TaskID: "task-a", UploadID: "u3", ExpiresAt: at(3)},
		{ID: "u4:0", TaskID: "task-c", UploadID: "u4", ExpiresAt: at(4)},
	}
	groups := GroupVerificationChunkCandidates(candidates, 4)
	var got [][]string
	for _, g := range groups {
		var ids []string
		for _, c := range g {
			ids = append(ids, c.TaskID+"/"+c.ID)
		}
		got = append(got, ids)
	}
	want := [][]string{{"task-a/u1:0", "task-a/u1:1", "task-a/u3:0"}, {"task-b/u2:0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	expires := at(1)
	later := at(90)
	current := map[string]VerificationRow{
		"u1:0": {Table: "verification_upload_chunks", ID: "u1:0", TaskID: "task-a", LogicalKey: "u1", State: "staging", ExpiresAt: &expires},
		"u1:1": {Table: "verification_upload_chunks", ID: "u1:1", TaskID: "task-a", LogicalKey: "u1", State: "staging", ExpiresAt: &later},
		"u3:0": {Table: "verification_upload_chunks", ID: "u3:0", TaskID: "task-b", LogicalKey: "u3", State: "staging", ExpiresAt: &expires},
	}
	kept := VerificationChunkExpiryRecheck("task-a", at(30), groups[0], current)
	if len(kept) != 1 || kept[0].ID != "u1:0" {
		t.Fatalf("recheck kept %+v", kept)
	}
	event := VerificationChunkExpiryEvent("task-a", fixed, []VerificationChunkCandidate{{ID: "u1:1", UploadID: "u1"}, {ID: "u1:0", UploadID: "u1"}})
	var payload struct {
		Cutoff       string   `json:"cutoff"`
		DeletedCount int      `json:"deleted_count"`
		ChunkIDs     []string `json:"chunk_ids"`
		UploadIDs    []string `json:"upload_ids"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "verification.chunk.expire" || event.ActorID != "verification-reconciler" || event.ActorRole != core.ActorSystem || event.TaskID != "task-a" || payload.DeletedCount != 2 || !reflect.DeepEqual(payload.ChunkIDs, []string{"u1:0", "u1:1"}) || !reflect.DeepEqual(payload.UploadIDs, []string{"u1"}) || payload.Cutoff != fixed.Format(time.RFC3339Nano) {
		t.Fatalf("event = %+v payload = %+v", event, payload)
	}
}

// TestVerificationChunkExpirySelectionSeam keeps the conformance seam
// context-scoped: without the test value it does nothing, and with it the
// callback sees every selected row before any group runs.
func TestVerificationChunkExpirySelectionSeam(t *testing.T) {
	backend := NewVolatileBackend().(*volatileMemory)
	groups := [][]VerificationChunkCandidate{{{ID: "a:0", TaskID: "task-a"}, {ID: "a:1", TaskID: "task-a"}}, {{ID: "b:0", TaskID: "task-b"}}}
	var order []string
	run := func(ctx context.Context) {
		n, err := ExecuteVerificationChunkExpiry(ctx, backend, groups, func(_ taskops.TaskLease, group []VerificationChunkCandidate) (int, error) {
			order = append(order, "group:"+group[0].TaskID)
			return len(group), nil
		})
		if err != nil || n != 3 {
			t.Fatalf("expiry = %d, %v", n, err)
		}
	}
	run(context.Background())
	if !reflect.DeepEqual(order, []string{"group:task-a", "group:task-b"}) {
		t.Fatalf("production order = %v", order)
	}
	order = nil
	run(WithVerificationChunkExpirySelectedForTest(context.Background(), func(rows []VerificationChunkCandidate) {
		order = append(order, "selected:"+strconv.Itoa(len(rows)))
	}))
	if !reflect.DeepEqual(order, []string{"selected:3", "group:task-a", "group:task-b"}) {
		t.Fatalf("seam order = %v", order)
	}
}
