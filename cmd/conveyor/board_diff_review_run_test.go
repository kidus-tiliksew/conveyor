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

func TestBoardDiffReviewChangedRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	pos := now.Add(-10 * time.Minute)
	entered := pos.Add(-62 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", now.Format(http.TimeFormat))
		switch r.URL.Path {
		case "/v1/activity":
			w.Header().Set("X-Conveyor-Total", "3")
			fmt.Fprintf(w, `[{"task":{"id":"a","title":"Task A","state":"running"},"last_event_at":%q,"next_action":{"judgment":true,"label":"Approve"}},{"task":{"id":"b","title":"Task B","state":"running"},"last_event_at":%q},{"task":{"id":"c","title":"Task C","state":"merged"},"last_event_at":%q}]`, now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))
		case "/v1/tasks/a/history":
			fmt.Fprintf(w, `{"events":[{"id":"3","kind":"work_order.created","at":%q,"payload":{"stage":"implement"}},{"id":"2","kind":"pull_request.opened","at":%q,"payload":{"number":42}},{"id":"1","kind":"work_order.created","at":%q,"payload":{"stage":"review"}}]}`, now.Format(time.RFC3339), pos.Add(5*time.Minute).Format(time.RFC3339), pos.Add(10*time.Minute).Add(-time.Second).Format(time.RFC3339))
		case "/v1/tasks/b/history":
			fmt.Fprintf(w, `{"events":[{"id":"1","kind":"task.created","at":%q}]}`, now.Format(time.RFC3339))
		case "/v1/tasks/c/history":
			fmt.Fprintf(w, `{"events":[{"id":"1","kind":"task.state_changed","at":%q,"payload":{"from":"running","to":"merged"}}]}`, now.Format(time.RFC3339))
		default:
			t.Errorf("unexpected %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &client{base: srv.URL}
	state := boardDiffState{Position: pos, SavedAt: now, Recent: map[string]time.Time{"a": pos, "c": pos}, Tasks: map[string]boardDiffTask{"a": {Title: "Task A", State: "running", Stage: "implement", EnteredAt: &entered}, "c": {Title: "Task C", State: "running", Stage: "review", EnteredAt: &entered, Decision: "Decide"}}}
	var out bytes.Buffer
	for _, asJSON := range []bool{false, true} {
		if err := saveBoardDiffState(srv.URL, "", state); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := runBoardDiff(c, &out, boardDiffOptions{asJSON: asJSON}); err != nil {
			t.Fatal(err)
		}
		if !asJSON {
			text := out.String()
			firstLine := fmt.Sprintf("  implement → review  %s  (implement 1h11m59s)\n", now.Add(-time.Second).Format("15:04"))
			secondLine := fmt.Sprintf("  review → implement  %s  (review 1s)\n", now.Format("15:04"))
			if !strings.Contains(text, firstLine+secondLine) {
				t.Fatalf("missing exact transition lines: %q", text)
			}
			for _, part := range []string{"a  Task A", "implement → review", "review → implement", "PR #42 opened", "decision opened: Approve", "b  Task B", "  created", "c  Task C", "decision cleared: Decide", "  terminal"} {
				if strings.Count(text, part) != 1 {
					t.Fatalf("expected one %q in %s", part, text)
				}
			}
			continue
		}
		var got boardDiffOutput
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Changes) != 3 {
			t.Fatalf("json %s error %v", out.String(), err)
		}
		a, b, c := got.Changes[0], got.Changes[1], got.Changes[2]
		if len(a.Transitions) != 2 || a.Transitions[0].From != "implement" || a.Transitions[0].To != "review" || a.Transitions[1].To != "implement" || a.PullRequest == nil || a.PullRequest.Number != 42 || a.Decision == nil || !a.Decision.Opened || !b.Created || !c.Terminal || c.Decision == nil || !c.Decision.Cleared {
			t.Fatalf("changes %#v", got.Changes)
		}
	}
}
