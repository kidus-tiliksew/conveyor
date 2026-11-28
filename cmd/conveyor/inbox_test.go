package main

import (
	"bytes"
	"encoding/json"
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

// TestInboxPrintsProposalIdentity pins a pending document proposal item to
// its tier, ID and version, so the operator can tell which document waits.
func TestInboxPrintsProposalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{
			name: "single proposal",
			body: `{"items":[{"kind":"document_proposal","band":"decisions","proposal":{"id":"component-mcp","title":"MCP protocol","tier":"system_design","version":3,"origin_type":"task","proposed_at":"2026-10-12T07:00:00Z"}}],"acts":[]}`,
			want: []string{"decisions\n  system_design proposal component-mcp v3: MCP protocol\nshowing 1 of 1\n"},
		},
		{
			name: "several versions pending",
			body: `{"items":[
			  {"kind":"document_proposal","band":"decisions","proposal":{"id":"req-next","title":"Next action","tier":"requirement","version":2,"origin_type":"task","proposed_at":"2026-10-12T07:00:00Z"},"sentence":"Confirming a later one dismisses the earlier ones."},
			  {"kind":"document_proposal","band":"decisions","proposal":{"id":"req-next","title":"Next action","tier":"requirement","version":4,"origin_type":"task","proposed_at":"2026-10-12T08:00:00Z"},"sentence":"Confirming a later one dismisses the earlier ones."}
			],"acts":[]}`,
			want: []string{"decisions\n" +
				"  requirement proposal req-next v2: Next action\n    Confirming a later one dismisses the earlier ones.\n" +
				"  requirement proposal req-next v4: Next action\n    Confirming a later one dismisses the earlier ones.\n" +
				"showing 2 of 2\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total := "1"
			if strings.Count(tc.body, "document_proposal") == 2 {
				total = "2"
			}
			c, _ := serveInbox(t, tc.body, total)
			var out bytes.Buffer
			if err := runInbox(c, &out, inboxOptions{}); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.HasPrefix(out.String(), want) {
					t.Fatalf("output:\n%s\nwant prefix:\n%s", out.String(), want)
				}
			}
		})
	}
}

// A feedback report prints on exactly one line: its kind, ID, environment,
// and summary, the summary labelled as untrusted data, with every text that
// could start a line or change the terminal escaped
// (feature-production-feedback-listening FR-5; component-feedback-reports
// invariant 5).
func TestInboxPrintsFeedbackReportOnOneLine(t *testing.T) {
	for _, tc := range []struct {
		name, environment, summary, want string
	}{
		{"plain", "Production", "Checkout timed out", `"Production": untrusted summary "Checkout timed out"`},
		{"markdown and html", "Prod", "**bold** <img src=x onerror=alert(1)> [link](javascript:alert(1))", `"Prod": untrusted summary "**bold** <img src=x onerror=alert(1)> [link](javascript:alert(1))"`},
		{"backticks", "Prod", "``````code``````", "\"Prod\": untrusted summary \"``````code``````\""},
		{"line breaks and tabs", "Prod\nfake line", "first\nsecond\r\nthird\tcolumn", `"Prod\nfake line": untrusted summary "first\nsecond\r\nthird\tcolumn"`},
		{"terminal escape", "\x1b[2J\x1b[31mred", "\x1b]0;title\x07bell\x1b[0m", `"\x1b[2J\x1b[31mred": untrusted summary "\x1b]0;title\abell\x1b[0m"`},
		{"other control characters", "a\x00b\x7fc", "\x08back\x0bvertical\x0cfeed\u0085next", `"a\x00b\x7fc": untrusted summary "\bback\vvertical\ffeed\u0085next"`},
		{"right-to-left and separators", "\u202eevil", "abc\u202edef\u2066x\u2069\u2028line\u2029para", `"\u202eevil": untrusted summary "abc\u202edef\u2066x\u2069\u2028line\u2029para"`},
		{"quotes and backslashes", `say "hi"`, `C:\path\"x"`, `"say \"hi\"": untrusted summary "C:\\path\\\"x\""`},
		{"very long word", "Prod", strings.Repeat("w", 1000), `"Prod": untrusted summary "` + strings.Repeat("w", 1000) + `"`},
		{"non-ASCII", "Zürich", "Fehler beim Bezahlen – 支付失败", `"Z\u00fcrich": untrusted summary "Fehler beim Bezahlen \u2013 \u652f\u4ed8\u5931\u8d25"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"kind": "feedback_report", "band": "decisions", "report": map[string]any{
				"id": "rpt-261128-abc123", "summary": tc.summary, "environment": map[string]any{"id": "env-1", "name": tc.environment},
			}}
			body, err := json.Marshal(map[string]any{"items": []any{item}, "acts": []any{}})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := serveInbox(t, string(body), "1")
			var out bytes.Buffer
			if err := runInbox(c, &out, inboxOptions{}); err != nil {
				t.Fatal(err)
			}
			want := "decisions\n  feedback_report rpt-261128-abc123 environment " + tc.want + "\nshowing 1 of 1\nfactory acts: 0\n"
			if out.String() != want {
				t.Fatalf("stdout =\n%q\nwant\n%q", out.String(), want)
			}
			for _, r := range out.String() {
				if r != '\n' && (r < 0x20 || r == 0x7f || r > 0x7e) {
					t.Fatalf("stdout holds the character %U", r)
				}
			}
		})
	}
	// An ID that is not a plain identifier is quoted too.
	if got := inboxReportLine("feedback_report", inboxReportView{ID: "rpt\nx", Summary: "s", Environment: inboxReportEnvironmentView{ID: "env-1"}}); got != `  feedback_report "rpt\nx" environment "env-1": untrusted summary "s"` {
		t.Fatalf("line = %q", got)
	}
}
