package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBoardDiffHistoryRoundTripAndDedupe(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	cutoff := start.Add(time.Hour)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/tasks/a/history" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("unexpected %s", r.URL)
		}
		fmt.Fprintf(w, `{"events":[{"id":"3","kind":"work_order.created","at":%q,"payload":{"stage":"implement"}},{"id":"2","kind":"work_order.created","at":%q,"payload":{"stage":"review"}},{"id":"1","kind":"work_order.created","at":%q,"payload":{"stage":"implement"}}],"has_older":false}`, cutoff.Add(20*time.Minute).Format(time.RFC3339), cutoff.Add(10*time.Minute).Format(time.RFC3339), start.Format(time.RFC3339))
	}))
	defer srv.Close()
	c := &client{base: srv.URL}
	row := boardDiffActivity{}
	row.Task.ID = "a"
	row.Task.Title = "A"
	row.Task.State = "running"
	row.LastEventAt = cutoff.Add(20 * time.Minute)
	state := boardDiffState{Position: cutoff, Recent: map[string]time.Time{"a": cutoff}, Tasks: map[string]boardDiffTask{"a": {State: "running", Stage: "implement", EnteredAt: &start}}}
	feed := boardDiffFeed{Items: []boardDiffActivity{row}, Position: cutoff.Add(21 * time.Minute)}
	changes, next, incomplete, err := c.diffBoardActivities(state, feed)
	if err != nil || len(incomplete) != 0 || len(changes) != 1 {
		t.Fatalf("changes %#v incomplete %v err %v", changes, incomplete, err)
	}
	tr := changes[0].Transitions
	if len(tr) != 2 || tr[0].From != "implement" || tr[0].To != "review" || tr[0].PreviousSeconds == nil || *tr[0].PreviousSeconds != 4200 || tr[1].From != "review" || tr[1].To != "implement" || tr[1].PreviousSeconds == nil || *tr[1].PreviousSeconds != 600 {
		t.Fatalf("transitions %#v", tr)
	}
	again, _, _, err := c.diffBoardActivities(next, feed)
	if err != nil || len(again) != 0 || calls != 1 {
		t.Fatalf("dedupe %v calls %d err %v", again, calls, err)
	}
}

func TestBoardDiffHistoryPagingAndUnknown(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	t.Run("cap", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			fmt.Fprintf(w, `{"events":[{"id":%q,"kind":"other","at":%q}],"has_older":true,"older_cursor":%q}`, strconv.Itoa(calls), now.Add(time.Hour).Format(time.RFC3339), strconv.Itoa(calls))
		}))
		defer srv.Close()
		h, err := (&client{base: srv.URL}).boardDiffHistory("a", now, false)
		if err != nil || !h.Incomplete || calls != 20 {
			t.Fatalf("history %#v calls %d error %v", h, calls, err)
		}
	})
	t.Run("missing payload", func(t *testing.T) {
		row := boardDiffActivity{}
		row.Task.ID = "a"
		row.Task.State = "running"
		change, _, incomplete := reduceBoardDiff(row, boardDiffTask{Stage: "implement", State: "running"}, true, now, boardDiffHistory{Events: []boardDiffHistoryEvent{{Kind: "work_order.created", At: now.Add(time.Second)}}})
		if !incomplete || len(change.Transitions) != 1 || change.Transitions[0].To != "unknown" || change.Transitions[0].PreviousSeconds != nil {
			t.Fatalf("change %#v incomplete %v", change, incomplete)
		}
	})
	t.Run("restart", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				http.Error(w, "expired", 409)
				return
			}
			fmt.Fprint(w, `{"events":[],"has_older":false}`)
		}))
		defer srv.Close()
		h, err := (&client{base: srv.URL}).boardDiffHistory("a", now, false)
		if err != nil || h.Incomplete || calls != 2 {
			t.Fatalf("history %#v calls %d error %v", h, calls, err)
		}
	})
}

func TestBoardDiffRequestCount(t *testing.T) {
	for _, n := range []int{0, 1, 5} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/activity" {
					w.Header().Set("Date", "Fri, 02 Jan 2026 03:01:00 GMT")
					w.Header().Set("X-Conveyor-Total", strconv.Itoa(n))
					rows := make([]map[string]any, 0, n)
					for i := range n {
						rows = append(rows, map[string]any{"task": map[string]any{"id": strconv.Itoa(i), "state": "running"}, "last_event_at": now.Add(time.Second)})
					}
					_ = json.NewEncoder(w).Encode(rows)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/history") {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				fmt.Fprint(w, `{"events":[],"has_older":false}`)
			}))
			defer srv.Close()
			c := &client{base: srv.URL}
			feed, err := c.readBoardDiffFeed(now)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = c.diffBoardActivities(boardDiffState{Position: now}, feed)
			if err != nil || calls != 1+n {
				t.Fatalf("calls %d err %v", calls, err)
			}
		})
	}
}
