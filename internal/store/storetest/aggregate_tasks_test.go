package storetest

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fixtureTaskIDShape is core.NewTaskID()'s <yymmdd>-<6 hex> followed by the
// fixture's process-wide sequence number.
var fixtureTaskIDShape = regexp.MustCompile(`^[0-9]{6}-[0-9a-f]{6}-([1-9][0-9]*)$`)

// TestFixtureTaskIDsAreDistinct proves that newAggregateTask's ID source never
// repeats within one process. The proof rests on the sequence suffix, not on
// the random part: every draw must carry a sequence number no other draw
// carries, so the result holds for any random bytes and any scheduling
// (component-task-lifecycle).
func TestFixtureTaskIDsAreDistinct(t *testing.T) {
	const (
		sequential = 50_000
		workers    = 10
		perWorker  = 5_000
		draws      = sequential + workers*perWorker
	)
	ids := make([]string, 0, draws)
	for range sequential {
		ids = append(ids, fixtureTaskID())
	}
	concurrent := make([][]string, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drawn := make([]string, perWorker)
			for i := range drawn {
				drawn[i] = fixtureTaskID()
			}
			concurrent[worker] = drawn
		}()
	}
	wg.Wait()
	for _, drawn := range concurrent {
		ids = append(ids, drawn...)
	}
	if len(ids) != draws || draws < 100_000 {
		t.Fatalf("drew %d IDs, want %d (at least 100,000)", len(ids), draws)
	}

	seenIDs := make(map[string]bool, draws)
	seenSequences := make(map[uint64]bool, draws)
	for _, id := range ids {
		match := fixtureTaskIDShape.FindStringSubmatch(id)
		if match == nil {
			t.Fatalf("fixture task ID %q is not <yymmdd>-<6 hex>-<n>", id)
		}
		// The store bounds task IDs on verification records at 128 characters,
		// its narrowest task_id column (VARCHAR(128) in SingleStore).
		if len(id) > 128 || strings.ContainsRune(id, 0) {
			t.Fatalf("fixture task ID %q exceeds the store's task ID bound", id)
		}
		sequence, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			t.Fatalf("fixture task ID %q sequence: %v", id, err)
		}
		if seenSequences[sequence] {
			t.Fatalf("sequence %d handed out twice (latest %q)", sequence, id)
		}
		if seenIDs[id] {
			t.Fatalf("fixture task ID %q handed out twice", id)
		}
		seenSequences[sequence], seenIDs[id] = true, true
	}
	if len(seenIDs) != draws {
		t.Fatalf("%d distinct IDs across %d draws", len(seenIDs), draws)
	}
}
