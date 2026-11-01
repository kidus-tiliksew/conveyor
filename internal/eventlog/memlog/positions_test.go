package memlog

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

func TestPositionsReportEachWorkspaceHead(t *testing.T) {
	s := New()
	if got := s.Positions(); len(got) != 0 {
		t.Fatalf("empty log positions=%v", got)
	}
	ctx := context.Background()
	for _, ws := range []string{"a", "a", "b"} {
		if _, err := s.Append(ctx, ws, eventlog.StreamID("job/"+ws), eventlog.ExpectAny, []eventlog.NewEvent{{Kind: "job.enqueued", Payload: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Positions()
	if len(got) != 2 || got["a"] != 2 || got["b"] != 1 {
		t.Fatalf("positions=%v, want a=2 b=1", got)
	}
}
