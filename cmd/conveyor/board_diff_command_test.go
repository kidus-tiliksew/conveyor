package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBoardDiffCommandEndToEnd(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	changed := 0
	boardCalls, activityCalls, historyCalls := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", now.Format(http.TimeFormat))
		switch {
		case r.URL.Path == "/v1/board":
			boardCalls++
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("board limit %s", r.URL.RawQuery)
			}
			items := make([]map[string]any, 0, 100)
			for i := offset; i < min(offset+100, 500); i++ {
				items = append(items, map[string]any{"id": fmt.Sprint(i), "title": "Task", "state": "running", "stage": "implement", "next_action": map[string]any{"judgment": i == 0, "label": "Approve"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": 500})
		case r.URL.Path == "/v1/activity":
			activityCalls++
			w.Header().Set("X-Conveyor-Total", strconv.Itoa(changed))
			rows := make([]map[string]any, 0, changed)
			for i := range changed {
				rows = append(rows, map[string]any{"task": map[string]any{"id": fmt.Sprint(i), "title": "Task", "state": "running"}, "last_event_at": now.Add(time.Second), "next_action": map[string]any{"judgment": i == 0, "label": "Approve"}})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case strings.HasSuffix(r.URL.Path, "/history"):
			historyCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "has_older": false})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "do-not-persist"}
	var out bytes.Buffer
	if err := runBoardDiff(c, &out, boardDiffOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	var first boardDiffOutput
	if err := json.Unmarshal(out.Bytes(), &first); err != nil || !first.FirstRun || first.Summary == nil || first.Summary.Open != 500 || len(first.Summary.Decisions) != 1 || boardCalls != 5 || activityCalls != 1 {
		t.Fatalf("first %#v board %d activity %d err %v", first, boardCalls, activityCalls, err)
	}
	path, _ := boardDiffStatePath(srv.URL, "", "")
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte(c.token)) {
		t.Fatalf("snapshot credential or error %v", err)
	}
	for _, n := range []int{0, 1, 5} {
		changed = n
		boardCalls, activityCalls, historyCalls = 0, 0, 0
		out.Reset()
		if err := runBoardDiff(c, &out, boardDiffOptions{asJSON: true}); err != nil {
			t.Fatal(err)
		}
		if boardCalls != 0 || activityCalls != 1 || historyCalls != n {
			t.Fatalf("changed %d: board=%d activity=%d history=%d", n, boardCalls, activityCalls, historyCalls)
		}
		if n == 0 && (out.Len() >= 1024 || strings.TrimSpace(out.String()) != fmt.Sprintf(`{"position":%q,"changes":[]}`, now.Format(time.RFC3339))) {
			t.Fatalf("unchanged output %s", out.String())
		}
		if n == 0 {
			out.Reset()
			if err := runBoardDiff(c, &out, boardDiffOptions{}); err != nil || out.Len() != 0 || out.Len() >= 1024 {
				t.Fatalf("unchanged text %q: %v", out.String(), err)
			}
		}
		// Keep every task new to the overlap in the next case.
		if n > 0 {
			state, _, e := loadBoardDiffState(srv.URL, "", "", now)
			if e != nil {
				t.Fatal(e)
			}
			state.Recent = map[string]time.Time{}
			if e = saveBoardDiffState(srv.URL, "", "", state); e != nil {
				t.Fatal(e)
			}
		}
	}
}

func TestBoardDiffCommandStatelessAndFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/activity" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Date", now.Format(http.TimeFormat))
		w.Header().Set("X-Conveyor-Total", "0")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &client{base: srv.URL}
	path, _ := boardDiffStatePath(srv.URL, "", "")
	var out bytes.Buffer
	if err := runBoardDiff(c, &out, boardDiffOptions{since: now.Format(time.RFC3339), asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stateless wrote snapshot: %v", err)
	}
	if err := runBoardDiff(c, &out, boardDiffOptions{since: now.Format(time.RFC3339), reset: true}); err == nil {
		t.Fatal("expected mutually exclusive flags")
	}
	if err := runBoardDiff(c, &out, boardDiffOptions{since: now.Format(time.RFC3339Nano) + ".1"}); err == nil {
		t.Fatal("expected invalid position")
	}
}
