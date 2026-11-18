package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestBoardDiffResetReasons(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Date", now.Format(http.TimeFormat))
		switch r.URL.Path {
		case "/v1/board":
			fmt.Fprint(w, `{"items":[],"total":0}`)
		case "/v1/activity":
			w.Header().Set("X-Conveyor-Total", "0")
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	c := &client{base: srv.URL}
	path, _ := boardDiffStatePath(srv.URL, "", "")
	var out bytes.Buffer
	if err := runBoardDiff(c, &out, boardDiffOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runBoardDiff(c, &out, boardDiffOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	var result boardDiffOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.FirstRun || result.ResetReason != "invalid snapshot" || result.Summary == nil {
		t.Fatalf("result %#v err %v", result, err)
	}
	out.Reset()
	if err := runBoardDiff(c, &out, boardDiffOptions{reset: true, asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.ResetReason != "reset requested" {
		t.Fatalf("result %#v err %v", result, err)
	}
	if calls != 6 {
		t.Fatalf("calls %d", calls)
	}
}
