package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestLineageRebuildLongHelpDocumentsOperationalBound(t *testing.T) {
	command := lineageCmd()
	rebuild, _, err := command.Find([]string{"rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"every workspace event in memory within one transaction",
		"never deletes rows it cannot regenerate",
		"Stale identity cleanup requires a migration",
	} {
		if !strings.Contains(rebuild.Long, phrase) {
			t.Fatalf("lineage rebuild help omitted %q: %s", phrase, rebuild.Long)
		}
	}
}

func serveLineage(t *testing.T, wantPath, body string) (*client, func() url.Values) {
	t.Helper()
	var mu sync.Mutex
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != wantPath || r.Header.Get("X-Workspace-ID") != "demo" {
			t.Errorf("unexpected request %s %s workspace=%q", r.Method, r.URL.EscapedPath(), r.Header.Get("X-Workspace-ID"))
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		query = r.URL.Query()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t", workspace: "demo"}, func() url.Values {
		mu.Lock()
		defer mu.Unlock()
		return query
	}
}

// TestLineageShowSendsFiltersAndPrintsDepthAndStatus covers conveyor lineage
// show: filters and caps reach GET /v1/lineage with depth, path, and status
// included; text output indents by depth and prints status and truncation;
// --json prints the server body verbatim (component-lineage;
// req-260802-72fc68 REQ-5).
func TestLineageShowSendsFiltersAndPrintsDepthAndStatus(t *testing.T) {
	body := `{"roots":[{"type":"requirement_version","id":"req-a:v1"}],"nodes":[` +
		`{"type":"requirement_version","id":"req-a:v1","label":"Intent requirement v1","depth":0,"status":{"version_status":"superseded","current_version":2}},` +
		`{"type":"task","id":"261027-aaaaaa","label":"Deliver","depth":1,"edge_path":[{"kind":"serves"}],"status":{"state":"merged"}},` +
		`{"type":"verdict","id":"review:261027-aaaaaa-review-1-seat-2","label":"Review verdict","depth":2,"status":{"outcome":"approve","review_round":1,"review_seat":2}}],` +
		`"links":[{},{}],"truncated":true,"exhaustion_reasons":["fanout"],"omitted_nodes":3,"omitted_links":0}`
	c, query := serveLineage(t, "/v1/lineage/requirement_version/req-a:v1", body)
	var out bytes.Buffer
	options := lineageShowOptions{direction: "out", kinds: []string{"serves", "dispatches"}, nodeTypes: []string{"task"}, maxDepth: 3, maxNodes: 64}
	if err := runLineageShow(c, &out, "requirement_version", "req-a:v1", options); err != nil {
		t.Fatal(err)
	}
	got := query()
	want := url.Values{"include": {"depth,path,status"}, "direction": {"out"}, "kind": {"serves,dispatches"}, "node_type": {"task"}, "max_depth": {"3"}, "max_nodes": {"64"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query=%v want %v", got, want)
	}
	text := out.String()
	for _, line := range []string{
		`0 requirement_version req-a:v1 "Intent requirement v1" [superseded, current v2]`,
		`  1 task 261027-aaaaaa "Deliver" [merged]`,
		`    2 verdict review:261027-aaaaaa-review-1-seat-2 "Review verdict" [approve, round 1 seat 2]`,
		`3 nodes, 2 links; truncated by fanout (3 nodes and 0 links omitted)`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("output missing %q:\n%s", line, text)
		}
	}
	out.Reset()
	if err := runLineageShow(c, &out, "requirement_version", "req-a:v1", lineageShowOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("json output=%q", out.String())
	}
}

// TestLineageImpactPagesAndFiltersOutdatedWork covers conveyor lineage impact.
func TestLineageImpactPagesAndFiltersOutdatedWork(t *testing.T) {
	body := `{"root":{"type":"requirement","id":"req-a"},"current_version":2,"outdated_only":true,"items":[` +
		`{"version":1,"version_status":"superseded","relation":"serves","task":{"type":"task","id":"261027-aaaaaa","label":"Deliver"},"task_state":"merged","merged_ranges":["conveyor@a..b"],"created_by_event_id":7}],` +
		`"total":3,"limit":1,"offset":1,"next_offset":2,"truncated":false}`
	c, query := serveLineage(t, "/v1/lineage/requirement/req-a/impact", body)
	var out bytes.Buffer
	if err := runLineageImpact(c, &out, "requirement", "req-a", lineageImpactOptions{outdated: true, limit: 1, offset: 1}); err != nil {
		t.Fatal(err)
	}
	if got, want := query(), (url.Values{"outdated": {"true"}, "limit": {"1"}, "offset": {"1"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("query=%v want %v", got, want)
	}
	want := "requirement req-a current v2\n  v1 superseded serves 261027-aaaaaa \"Deliver\" merged conveyor@a..b\n1 of 3 entries from offset 1; next --offset 2\n"
	if out.String() != want {
		t.Fatalf("output=%q want %q", out.String(), want)
	}
	out.Reset()
	if err := runLineageImpact(c, &out, "requirement", "req-a", lineageImpactOptions{asJSON: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("json output=%q", out.String())
	}
}

func TestLineageReadCommandsRequireTwoArguments(t *testing.T) {
	command := lineageCmd()
	for _, name := range []string{"show", "impact"} {
		sub, _, err := command.Find([]string{name})
		if err != nil || sub.Name() != name {
			t.Fatalf("lineage %s missing: %v", name, err)
		}
		if sub.Args(sub, []string{"task"}) == nil {
			t.Fatalf("lineage %s accepted one argument", name)
		}
	}
}
