package httpapi

import (
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// attentionPollComposition names the reads a poll composes its body from. A
// 304 must perform none of them (component-http-api, Activity conditional
// admission).
func assertNoComposition(t *testing.T, before, after map[string]int) {
	t.Helper()
	for name, count := range after {
		// The validation read is the tag's own input, so every poll makes it.
		if name == "ReadActivityValidation" {
			continue
		}
		if count != before[name] {
			t.Fatalf("a 304 poll performed %s (%d -> %d)", name, before[name], count)
		}
	}
}

// TestTaskObservationPollAnswersUnchangedWith304 proves the narrow wait poll
// answers an unchanged task with 304 before any composition, and a changed task
// with 200 and a new tag (req-agent-skills AC-4.3; component-http-api, Activity
// conditional admission).
func TestTaskObservationPollAnswersUnchangedWith304(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-obs", core.TaskAwaiting)
	path := "/v1/tasks/task-obs/observation?workspace_id=demo"

	cold := f.read(path, "")
	if cold.Code != http.StatusOK {
		t.Fatalf("first poll status=%d body=%s", cold.Code, cold.Body.String())
	}
	etag := cold.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the first poll states no ETag")
	}
	for _, want := range []string{`"task_id":"task-obs"`, `"state":"awaiting_human"`, `"merge_approval":false`, `"claimable_orders":[]`} {
		if !strings.Contains(cold.Body.String(), want) {
			t.Fatalf("observation body=%s lacks %s", cold.Body.String(), want)
		}
	}
	if f.counted.calls["ListWorkOrders"] != 0 || f.counted.calls["ListWorkOrdersForTasks"] != 0 {
		t.Fatalf("the observation poll read a workspace order listing: %v", f.counted.calls)
	}

	before := maps.Clone(f.counted.calls)
	warm := f.read(path, etag)
	if warm.Code != http.StatusNotModified {
		t.Fatalf("unchanged poll status=%d body=%s, want 304", warm.Code, warm.Body.String())
	}
	if warm.Body.Len() != 0 {
		t.Fatalf("304 body=%q, want empty", warm.Body.String())
	}
	assertNoComposition(t, before, f.counted.calls)

	// A new claimable order changes the observation, so the next poll answers
	// 200 with a tag that no longer admits the old one.
	f.order(core.WorkOrder{ID: "task-obs-implement-1", TaskID: "task-obs", JobID: "job-obs", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
	changed := f.read(path, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("changed poll status=%d body=%s, want 200", changed.Code, changed.Body.String())
	}
	if changed.Header().Get("ETag") == etag {
		t.Fatal("the changed observation kept the old tag")
	}
	if !strings.Contains(changed.Body.String(), `"id":"task-obs-implement-1"`) {
		t.Fatalf("changed observation=%s lacks the claimable order", changed.Body.String())
	}
}

func TestTaskObservationAndNextActionValidatorsAreRouteScoped(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-scope", core.TaskRunning)
	observation := "/v1/tasks/task-scope/observation?workspace_id=demo"
	nextAction := "/v1/tasks/task-scope/next-action?workspace_id=demo"
	for _, pair := range [][2]string{{observation, nextAction}, {nextAction, observation}} {
		first := f.read(pair[0], "")
		if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
			t.Fatalf("first read: status=%d tag=%q", first.Code, first.Header().Get("ETag"))
		}
		second := f.read(pair[1], first.Header().Get("ETag"))
		if second.Code != http.StatusOK {
			t.Fatalf("cross-route read: status=%d body=%s", second.Code, second.Body.String())
		}
	}
}

// TestNextActionPollAnswersUnchangedWith304 proves the next-action read keeps
// its body while an unchanged poll costs no projection, and a changed task
// answers 200 with a new tag (req-agent-skills AC-4.3).
func TestNextActionPollAnswersUnchangedWith304(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-next", core.TaskRunning)
	path := "/v1/tasks/task-next/next-action?workspace_id=demo"

	cold := f.read(path, "")
	if cold.Code != http.StatusOK {
		t.Fatalf("first read status=%d body=%s", cold.Code, cold.Body.String())
	}
	etag := cold.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the first read states no ETag")
	}
	if !strings.Contains(cold.Body.String(), `"actor"`) {
		t.Fatalf("next action body=%s", cold.Body.String())
	}

	before := maps.Clone(f.counted.calls)
	warm := f.read(path, etag)
	if warm.Code != http.StatusNotModified {
		t.Fatalf("unchanged read status=%d body=%s, want 304", warm.Code, warm.Body.String())
	}
	assertNoComposition(t, before, f.counted.calls)

	f.order(core.WorkOrder{ID: "task-next-implement-1", TaskID: "task-next", JobID: "job-next", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
	changed := f.read(path, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("changed read status=%d body=%s, want 200", changed.Code, changed.Body.String())
	}
	if changed.Header().Get("ETag") == etag {
		t.Fatal("the changed next action kept the old tag")
	}
}
