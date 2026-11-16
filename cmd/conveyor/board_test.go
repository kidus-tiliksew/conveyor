package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBoardOutputAndPaging(t *testing.T) {
	body := `{"items":[{"id":"task-1","title":"A task","state":"running","next_action":{"label":"Work queued"}}],"total":4,"limit":2,"offset":2}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/board" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("offset") != "2" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "t"}
	var out bytes.Buffer
	if err := runBoard(c, &out, boardOptions{limit: 2, offset: 2, asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("JSON: %q", out.String())
	}
	out.Reset()
	if err := runBoard(c, &out, boardOptions{limit: 2, offset: 2}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task-1", "A task", "Work queued", "1 shown, 4 total"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
}
