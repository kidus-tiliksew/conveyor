package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestSelectFactoryActsBoundsAndRole(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	events := []core.Event{
		{ID: 1, TaskID: "t", Kind: "pipeline.bounced", ActorRole: core.ActorAgent, At: now, Payload: core.JSONPayload(map[string]any{"source": "request-changes"})},
		{ID: 2, TaskID: "t", Kind: "pipeline.bounced", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"source": "mcp-review"})},
		{ID: 3, TaskID: "t", Kind: "merge.confirmed", ActorRole: core.ActorSystem, At: now},
		{ID: 4, TaskID: "t", Kind: "merge.confirmed", ActorRole: core.ActorRunner, At: now.Add(time.Second)},
		{ID: 5, TaskID: "t", Kind: "work_order.expired", ActorRole: core.ActorSystem, At: now.Add(2 * time.Second)},
		{ID: 6, TaskID: "t", Kind: "work_order.requeued", ActorRole: core.ActorSystem, At: now.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{"automatic_retry_count": 1})},
	}
	acts := SelectFactoryActs(events, now, 100)
	if len(acts) != 4 {
		t.Fatalf("acts=%+v", acts)
	}
	if acts[len(acts)-1].At.Before(now) {
		t.Fatal("since bound excluded an event at since")
	}
	one := SelectFactoryActs(events, now, 1)
	if len(one) != 1 {
		t.Fatalf("limit=%d", len(one))
	}
	outside := SelectFactoryActs([]core.Event{{ID: 9, TaskID: "t", Kind: "pipeline.bounced", ActorRole: core.ActorSystem, At: now.Add(-time.Nanosecond)}}, now, 100)
	if len(outside) != 0 {
		t.Fatalf("event before since kept: %+v", outside)
	}
}
