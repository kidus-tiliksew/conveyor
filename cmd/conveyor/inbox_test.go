package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Tests for conveyor inbox (req-operator-next-action AC-5.2, AC-2.4).

func serveInbox(t *testing.T, body, total string) (*client, func() url.Values) {
	t.Helper()
	var mu sync.Mutex
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/next-actions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		query = r.URL.Query()
		mu.Unlock()
		w.Header().Set("X-Conveyor-Total", total)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t"}, func() url.Values {
		mu.Lock()
		defer mu.Unlock()
		return query
	}
}

func TestInboxJSONVerbatim(t *testing.T) {
	body := `{"items":[{"band":"decisions","task":{"id":"z"},"future":true}],"acts":[],"unknown_envelope":{"x":1}}`
	c, _ := serveInbox(t, body+"\n", "1")
	var out bytes.Buffer
	if err := runInbox(c, &out, inboxOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), body+"\n")
	}
}

func TestInboxServerOrder(t *testing.T) {
	// Server order is deliberately not alphabetical by task ID or title.
	body := `{"items":[
	  {"band":"decisions","task":{"id":"261011-zzzzzz","title":"Zeta"},"next_action":{"label":"Merge approval pending","actor":"operator","kind":"merge_approval","judgment":true,"summary":"PR #9 awaits approval.","deadline":null}},
	  {"band":"decisions","task":{"id":"261011-aaaaaa","title":"Alpha"},"next_action":{"label":"Plan approval pending","actor":"operator","kind":"plan_approval","judgment":true,"summary":"Plan v1 awaits.","deadline":null}},
	  {"band":"blocked","task":{"id":"261011-mmmmmm","title":"Mu"},"next_action":{"label":"Blocked by dependency","actor":"operator","kind":"blocked","judgment":false,"summary":"Waiting on a dependency.","deadline":null},"blockers":[{"task_id":"261011-dddddd","title":"Delta schema","state":"running"}]},
	  {"band":"automatic","task":{"id":"261011-bbbbbb","title":"Beta"},"next_action":{"label":"Running","actor":"agent","kind":"running","judgment":false,"summary":"Implementing.","deadline":null}}
	],"acts":[{"id":7,"task_id":"261011-eeeeee","kind":"auto_recovered","at":"2026-10-12T07:00:00Z","automatic":true,"summary":"lease expired"}]}`
	c, query := serveInbox(t, body, "9")
	var out bytes.Buffer
	if err := runInbox(c, &out, inboxOptions{}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"decisions",
		"  261011-zzzzzz Zeta: Merge approval pending: PR #9 awaits approval.",
		"  261011-aaaaaa Alpha: Plan approval pending: Plan v1 awaits.",
		"blocked",
		"  261011-mmmmmm Mu: Blocked by dependency: Waiting on a dependency.",
		"    blocked by 261011-dddddd Delta schema",
		"automatic",
		"  261011-bbbbbb Beta: Running: Implementing.",
		"showing 4 of 9",
		"factory acts: 1",
		"",
	}, "\n")
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if got := query(); got.Has("band") {
		t.Fatalf("unfiltered inbox sent band=%q", got.Get("band"))
	}

	out.Reset()
	if err := runInbox(c, &out, inboxOptions{band: "blocked", limit: 5, offset: 2}); err != nil {
		t.Fatal(err)
	}
	if got := query(); got.Get("band") != "blocked" || got.Get("limit") != "5" || got.Get("offset") != "2" {
		t.Fatalf("query = %v", got)
	}
	// The CLI never filters client-side: every served item still prints.
	if !strings.Contains(out.String(), "261011-zzzzzz") || !strings.Contains(out.String(), "261011-bbbbbb") {
		t.Fatalf("client-side filtering dropped items:\n%s", out.String())
	}

	out.Reset()
	if err := runInbox(c, &out, inboxOptions{band: "automatic"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  2026-10-12T07:00:00Z 261011-eeeeee auto_recovered: lease expired\n") {
		t.Fatalf("--band automatic did not list acts:\n%s", out.String())
	}
}
