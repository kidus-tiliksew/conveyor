package store_test

import (
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestBoardNextActionMatchesTaskDetail(t *testing.T) {
	f := newBatchFixture(t)
	f.add(t, true)
	f.add(t, false)
	opts := store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities}
	board, err := store.ReadBoard(f.ctx, f.st, store.BoardOptions{NextAction: opts})
	if err != nil {
		t.Fatal(err)
	}
	lines := make(map[string]nextaction.Compact, len(board.Items))
	for _, item := range board.Items {
		lines[item.ID] = item.NextAction
	}
	for _, task := range f.requested {
		got, ok := lines[task.ID]
		if !ok {
			t.Fatalf("task %s missing from board", task.ID)
		}
		detail, err := store.ReadNextAction(f.ctx, f.st, task.ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		want := nextaction.CompactFrom(detail)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("task %s board action %+v, detail %+v", task.ID, got, want)
		}
	}
}
