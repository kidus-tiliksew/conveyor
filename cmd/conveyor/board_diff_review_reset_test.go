package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBoardDiffReviewChangesAfterReset(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	reads := 0
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		w.Header().Set("Date", now.Format(http.TimeFormat))
		switch r.URL.Path {
		case "/v1/activity":
			reads++
			w.Header().Set("X-Conveyor-Total", "1")
			at := now.Add(-time.Minute)
			if reads > 1 {
				at = now
			}
			fmt.Fprintf(w, `[{"task":{"id":"a","title":"Task A","state":"running"},"last_event_at":%q}]`, at.Format(time.RFC3339))
		case "/v1/board":
			fmt.Fprint(w, `{"total":1,"items":[{"id":"a","title":"Task A","state":"running","stage":"implement"}]}`)
		case "/v1/tasks/a/history":
			fmt.Fprintf(w, `{"events":[{"id":"2","kind":"work_order.created","at":%q,"payload":{"stage":"review"}},{"id":"1","kind":"work_order.created","at":%q,"payload":{"stage":"implement"}}]}`, now.Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	c := &client{base: srv.URL}
	path, _ := boardDiffStatePath(srv.URL, "")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runBoardDiff(c, &out, boardDiffOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Reset: invalid snapshot") || len(order) != 2 || order[0] != "/v1/activity" || order[1] != "/v1/board" {
		t.Fatalf("reset %q order %v", out.String(), order)
	}
	out.Reset()
	if err := runBoardDiff(c, &out, boardDiffOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Open tasks:") || strings.Contains(out.String(), "Reset:") || !strings.Contains(out.String(), "implement → review") {
		t.Fatalf("after reset %q", out.String())
	}
}
