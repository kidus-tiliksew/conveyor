package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoardDiffReviewStageRecovery(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	event := func(id, kind, stage string, minutes int) boardDiffHistoryEvent {
		return boardDiffHistoryEvent{ID: id, Kind: kind, At: now.Add(time.Duration(minutes) * time.Minute), Payload: map[string]json.RawMessage{"stage": json.RawMessage(fmt.Sprintf("%q", stage))}}
	}
	row := boardDiffActivity{}
	row.Task.State = "running"
	history := boardDiffHistory{Events: []boardDiffHistoryEvent{event("1", "work_order.created", "spec", -30), event("2", "work_order.created", "implement", -20), event("3", "work_order.created", "review", 10)}}
	change, _, _ := reduceBoardDiff(row, boardDiffTask{}, false, now, history)
	if len(change.Transitions) != 1 || change.Transitions[0].From != "implement" || change.Transitions[0].To != "review" || change.Transitions[0].PreviousSeconds == nil || *change.Transitions[0].PreviousSeconds != 1800 {
		t.Fatalf("transitions %#v", change.Transitions)
	}
	history.Events[1] = event("2", "work_order.created", "review", -20)
	history.Events[2] = event("3", "work_order.claimed", "review", 10)
	change, _, _ = reduceBoardDiff(row, boardDiffTask{}, false, now, history)
	if len(change.Transitions) != 0 {
		t.Fatalf("duplicate transition %#v", change.Transitions)
	}
	history.Events = []boardDiffHistoryEvent{event("1", "work_order.claimed", "implement", -20), event("2", "work_order.created", "review", 10)}
	change, _, _ = reduceBoardDiff(row, boardDiffTask{Stage: "implement"}, true, now, history)
	if len(change.Transitions) != 1 || change.Transitions[0].PreviousSeconds != nil {
		t.Fatalf("unproven start %#v", change.Transitions)
	}
	row.Task.State = "closed"
	change, _, _ = reduceBoardDiff(row, boardDiffTask{}, false, now, boardDiffHistory{})
	if change.Terminal {
		t.Fatal("reported already-terminal task")
	}
}

func TestBoardDiffReviewIncompleteUpdates(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	for _, terminal := range []bool{true, false} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if terminal {
					http.Error(w, "restart", http.StatusConflict)
					return
				}
				fmt.Fprintf(w, `{"events":[{"id":%q,"kind":"other","at":%q}],"has_older":true,"older_cursor":%q}`, fmt.Sprint(calls), now.Add(time.Minute).Format(time.RFC3339), fmt.Sprint(calls))
			}))
			defer srv.Close()
			row := boardDiffActivity{}
			row.Task.ID = "a"
			row.Task.Title = "Updated"
			row.Task.State = "running"
			if terminal {
				row.Task.State = "merged"
			}
			row.LastEventAt = now.Add(time.Minute)
			state := boardDiffState{Position: now, Recent: map[string]time.Time{"a": now}, Tasks: map[string]boardDiffTask{"a": {Title: "Old", State: "queued", Stage: "implement", EnteredAt: &now}}}
			changes, next, incomplete, err := (&client{base: srv.URL}).diffBoardActivities(state, boardDiffFeed{Items: []boardDiffActivity{row}, Position: now.Add(time.Minute)})
			if err != nil || len(incomplete) != 1 || !strings.Contains(incomplete[0].Reason, "conveyor task show a") {
				t.Fatalf("incomplete %#v err %v", incomplete, err)
			}
			if terminal {
				if len(changes) != 1 || !changes[0].Terminal {
					t.Fatalf("terminal %#v", changes)
				}
				if _, ok := next.Tasks["a"]; ok {
					t.Fatal("terminal task retained")
				}
			} else {
				task := next.Tasks["a"]
				if task.Title != "Updated" || task.State != "running" || task.Stage != "unknown" || task.EnteredAt != nil || calls != 20 {
					t.Fatalf("task %#v calls %d", task, calls)
				}
			}
		})
	}
}

func TestBoardDiffReviewOutputText(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	s := int64(4320)
	second := int64(600)
	result := boardDiffOutput{Position: now.Format(time.RFC3339), Changes: []boardDiffChange{{ID: "a", Title: "Task", Transitions: []boardDiffTransition{{Kind: "stage", From: "implement", To: "review", At: now, PreviousSeconds: &s}, {Kind: "stage", From: "review", To: "implement", At: now.Add(10 * time.Minute), PreviousSeconds: &second}}}}}
	var text bytes.Buffer
	if err := writeBoardDiffText(&text, result); err != nil {
		t.Fatal(err)
	}
	want := "a  Task\n  implement → review  03:00  (implement 1h12m)\n  review → implement  03:10  (review 10m)\n"
	if text.String() != want {
		t.Fatalf("text %q", text.String())
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded boardDiffOutput
	if err = json.Unmarshal(data, &decoded); err != nil || len(decoded.Changes) != 1 || len(decoded.Changes[0].Transitions) != 2 || decoded.Changes[0].Transitions[1].To != "implement" {
		t.Fatalf("JSON %s err %v", data, err)
	}
	text.Reset()
	if err := writeBoardDiffText(&text, boardDiffOutput{Changes: []boardDiffChange{}}); err != nil || text.Len() != 0 || text.Len() >= 1024 {
		t.Fatalf("unchanged %q %v", text.String(), err)
	}
}
