package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBoardDiffHistoryExtraPagesAndMissing(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	t.Run("two extra pages", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			fmt.Fprintf(w, `{"events":[{"id":%q,"kind":"other","at":%q}],"has_older":true,"older_cursor":%q}`, fmt.Sprint(calls), now.Add(-time.Duration(calls)*time.Minute).Format(time.RFC3339), fmt.Sprint(calls))
		}))
		defer srv.Close()
		h, err := (&client{base: srv.URL}).boardDiffHistory("a", now, true)
		if err != nil || h.Incomplete || calls != 3 {
			t.Fatalf("result %#v calls %d err %v", h, calls, err)
		}
	})
	t.Run("missing task", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
		defer srv.Close()
		row := boardDiffActivity{}
		row.Task.ID = "a"
		row.Task.State = "running"
		row.LastEventAt = now
		state := boardDiffState{Position: now}
		_, next, incomplete, err := (&client{base: srv.URL}).diffBoardActivities(state, boardDiffFeed{Items: []boardDiffActivity{row}, Position: now.Add(time.Minute)})
		if err != nil || len(incomplete) != 1 || incomplete[0].Reason != "task no longer visible" || !next.Position.Equal(now.Add(time.Minute)) {
			t.Fatalf("incomplete %#v position %s err %v", incomplete, next.Position, err)
		}
	})
	t.Run("unknown previous duration", func(t *testing.T) {
		row := boardDiffActivity{}
		row.Task.State = "running"
		e := boardDiffHistoryEvent{Kind: "work_order.created", At: now.Add(time.Minute), Payload: map[string]json.RawMessage{"stage": json.RawMessage(`"review"`)}}
		change, _, _ := reduceBoardDiff(row, boardDiffTask{Stage: "implement"}, true, now, boardDiffHistory{Events: []boardDiffHistoryEvent{e}})
		if len(change.Transitions) != 1 || change.Transitions[0].PreviousSeconds != nil {
			t.Fatalf("transitions %#v", change.Transitions)
		}
	})
}
