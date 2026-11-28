package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// mcpToolInventory lists the tools and their input property names.
func mcpToolInventory(t *testing.T, h http.Handler, token string) map[string][]string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result.Tools) == 0 {
		t.Fatalf("tools/list=%s err=%v", response.Body, err)
	}
	tools := map[string][]string{}
	for _, tool := range envelope.Result.Tools {
		names := []string{}
		for name := range tool.InputSchema.Properties {
			names = append(names, name)
		}
		slices.Sort(names)
		tools[tool.Name] = names
	}
	return tools
}

// TestMCPFeedbackReportToolIsTheOnlyReportTool proves that
// list_feedback_reports is the one MCP tool for reports, with the filters
// the design names, and that no other tool's name or input names a feedback
// report, a report link, or a fix task (component-feedback-reports invariant
// 24; req-production-feedback REQ-10; DEC-149(3)).
func TestMCPFeedbackReportToolIsTheOnlyReportTool(t *testing.T) {
	h := newReportRoutesHarness(t)
	tools := mcpToolInventory(t, h.handler, h.manager())
	inputs, ok := tools["list_feedback_reports"]
	if !ok {
		t.Fatal("tools/list lacks list_feedback_reports")
	}
	if want := []string{"crate_id", "environment_id", "limit", "offset", "snapshot", "state", "task_id", "workspace_id"}; !slices.Equal(inputs, want) {
		t.Fatalf("list_feedback_reports inputs=%v, want %v", inputs, want)
	}
	for name, inputs := range tools {
		if name == "list_feedback_reports" {
			continue
		}
		if strings.Contains(strings.ToLower(name), "feedback") {
			t.Fatalf("tool %s names feedback", name)
		}
		// submit_review_verdict's "feedback" is review feedback, not a
		// report, so an input is checked for the report's own names.
		for _, input := range inputs {
			lower := strings.ToLower(input)
			if strings.Contains(lower, "feedback_report") || slices.Contains([]string{"report", "report_id", "link_id", "cause_task_ids", "fix_task", "fix_task_id"}, lower) {
				t.Fatalf("tool %s names a feedback report through input %q", name, input)
			}
		}
	}
	if mcpCapabilities["list_feedback_reports"] != core.CapabilityViewWorkspace {
		t.Fatalf("list_feedback_reports capability=%q", mcpCapabilities["list_feedback_reports"])
	}
}

func TestMCPFeedbackReportRead(t *testing.T) {
	h := newReportRoutesHarness(t)
	crate := h.crate("Spring")
	member, outsider := h.mergedTask(crate.ID), h.mergedTask("")
	staging := h.create(`{"name":"Staging"}`)
	stagingKey := h.issue(staging.ID)
	a, b, closed := h.storeReport("a", nil), h.storeReport("b", nil), h.storeReport("closed", nil)
	h.advance(1)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: staging.ID, KeyID: stagingKey.ID, ArrivedAt: h.clock().Add(1),
		Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportMessage, Key: "s", Summary: "Staging message"}}}); err != nil {
		t.Fatal(err)
	}
	s, err := h.st.ListEnvironmentFeedbackReports(h.ctx, staging.ID)
	if err != nil || len(s) != 1 {
		t.Fatalf("staging=%v err=%v", s, err)
	}
	for _, report := range []store.FeedbackReport{a, b, closed, s[0]} {
		h.endLinking(report, nil, nil)
	}
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: a.ID, TaskID: member.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: b.ID, TaskID: outsider.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: closed.ID, Reason: "done"})
	args := map[string]any{"workspace_id": "alpha"}
	itemIDs := func(page mcpReadPage) []string {
		out := make([]string, len(page.Items))
		for i, item := range page.Items {
			var report map[string]any
			if err := json.Unmarshal(item, &report); err != nil {
				t.Fatal(err)
			}
			out[i] = report["id"].(string)
		}
		return out
	}
	for _, c := range []struct {
		args map[string]any
		want []string
	}{
		{args, []string{s[0].ID, b.ID, a.ID}},
		{withArgs(args, "state", "closed"), []string{closed.ID}},
		{withArgs(args, "environment_id", staging.ID), []string{s[0].ID}},
		{withArgs(args, "task_id", member.ID), []string{a.ID}},
		{withArgs(args, "crate_id", crate.ID), []string{a.ID}},
		{withArgs(args, "crate_id", "crate-missing"), []string{}},
	} {
		page := readAs(t, h.handler, h.tokens[core.WorkspaceRoleViewer], "list_feedback_reports", c.args)
		if got := itemIDs(page); !slices.Equal(got, c.want) || page.Total != len(c.want) {
			t.Fatalf("list_feedback_reports %v=%v total=%d, want %v", c.args, got, page.Total, c.want)
		}
	}
	// Each item is the served report that the route for people serves.
	page := readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "task_id", member.ID))
	var fromMCP, fromHTTP map[string]any
	if err := json.Unmarshal(page.Items[0], &fromMCP); err != nil {
		t.Fatal(err)
	}
	fromHTTP = h.detail(a.ID)
	if fmt.Sprint(fromMCP) != fmt.Sprint(fromHTTP) {
		t.Fatalf("MCP item=%v\nHTTP report=%v", fromMCP, fromHTTP)
	}
	requireServedFieldNames(t, fromMCP)
	// Paging through the framework's snapshot.
	first := readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 1))
	if first.Total != 3 || len(first.Items) != 1 || first.Snapshot == "" || first.NextOffset == nil || *first.NextOffset != 1 {
		t.Fatalf("first page=%+v", first)
	}
	second := readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 2, "offset", 1, "snapshot", first.Snapshot))
	if got := append(itemIDs(first), itemIDs(second)...); !slices.Equal(got, []string{s[0].ID, b.ID, a.ID}) {
		t.Fatalf("paged ids=%v", got)
	}
	// The framework's bounds refuse an argument outside them.
	for _, refused := range []map[string]any{
		withArgs(args, "limit", 101),
		withArgs(args, "limit", 0),
		withArgs(args, "offset", 1),
		withArgs(args, "state", "all"),
		withArgs(args, "no_crate", true),
		withArgs(args, "crate_id", strings.Repeat("c", 257)),
		withArgs(args, "task_id", ""),
		withArgs(args, "environment_id", 7),
	} {
		refusalAs(t, h.handler, h.manager(), "list_feedback_reports", refused)
	}
	if text := refusalAs(t, h.handler, h.foreign, "list_feedback_reports", args); !strings.Contains(text, "workspace_not_found") {
		t.Fatalf("a member of another workspace=%q", text)
	}
}

// TestMCPFeedbackReportReadChangesNothing reads reports whose linking ended
// and proves that the read changed no report, link, fix task, or event.
func TestMCPFeedbackReportReadChangesNothing(t *testing.T) {
	h := newReportRoutesHarness(t)
	cause := h.mergedTask("")
	report := h.storeReport("crash", nil)
	h.endLinking(report, []store.FeedbackReportLinkCandidate{{TaskID: cause.ID}}, nil)
	state := func() string {
		t.Helper()
		stored, err := h.st.GetFeedbackReport(h.ctx, report.ID)
		if err != nil {
			t.Fatal(err)
		}
		links, err := h.st.ListFeedbackReportLinks(h.ctx, report.ID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := h.st.ListEvents(h.ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		_ = json.NewEncoder(&buffer).Encode([]any{stored, links, events})
		return buffer.String()
	}
	before := state()
	for range 3 {
		readAs(t, h.handler, h.manager(), "list_feedback_reports", map[string]any{"workspace_id": "alpha"})
	}
	h.settle()
	if after := state(); after != before {
		t.Fatalf("list_feedback_reports changed a record:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMCPFeedbackReportReadOverTheItemLimit proves that a selection over the
// framework's 1,000 items is refused before the store loads any report or
// details: the store list carries the bound and answers the count alone. A
// narrower filter is read. The store conformance case
// ReadWorkBoundedByThePage counts the store's work for such a list on every
// backend.
func TestMCPFeedbackReportReadOverTheItemLimit(t *testing.T) {
	h := newReportRoutesHarness(t)
	environments := []store.EnvironmentRecord{h.environment}
	keys := []store.IssuedEnvironmentKey{h.key}
	for i := range 5 {
		environment := h.create(fmt.Sprintf(`{"name":"Region %d"}`, i))
		environments, keys = append(environments, environment), append(keys, h.issue(environment.ID))
	}
	stored := 0
	for i, environment := range environments {
		for batch := 0; batch < 4 && stored < mcpReadMaxItems+1; batch++ {
			entries := []store.FeedbackReportEntry{}
			for n := 0; n < store.MaxFeedbackReportEntries && stored < mcpReadMaxItems+1; n++ {
				entries = append(entries, store.FeedbackReportEntry{Kind: store.FeedbackReportError, Key: fmt.Sprintf("k-%d-%d-%d", i, batch, n), Summary: "Many"})
				stored++
			}
			h.advance(1)
			if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: environment.ID, KeyID: keys[i].ID, ArrivedAt: h.clock(), Entries: entries}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if stored != mcpReadMaxItems+1 {
		t.Fatalf("stored %d reports", stored)
	}
	type listed struct {
		query store.FeedbackReportQuery
		page  store.FeedbackReportPage
	}
	lists := []listed{}
	h.server.feedbackReportsListed = func(query store.FeedbackReportQuery, page store.FeedbackReportPage) {
		lists = append(lists, listed{query, page})
	}
	if text := refusalAs(t, h.handler, h.manager(), "list_feedback_reports", map[string]any{"workspace_id": "alpha"}); !strings.Contains(text, "exceeds 1000 items") {
		t.Fatalf("refusal=%q", text)
	}
	if len(lists) != 1 {
		t.Fatalf("the refused read made %d store lists, want 1", len(lists))
	}
	if got := lists[0]; got.query.RefuseAbove != mcpReadMaxItems || got.page.Total != mcpReadMaxItems+1 || len(got.page.Reports) != 0 ||
		len(got.page.Details.Links) != 0 || len(got.page.Details.Notes) != 0 || len(got.page.Details.Tasks) != 0 || len(got.page.Details.Crates) != 0 {
		t.Fatalf("the refused read's store list=%+v with %d reports and details %+v, want the count alone", got.query, len(got.page.Reports), got.page.Details)
	}
	h.server.feedbackReportsListed = nil
	page := readAs(t, h.handler, h.manager(), "list_feedback_reports", map[string]any{"workspace_id": "alpha", "environment_id": environments[0].ID})
	if page.Total != 200 {
		t.Fatalf("narrowed read total=%d, want 200", page.Total)
	}
	h.settle()
}

// TestMCPFeedbackReportRefusedReadAsksNothing proves that a read the
// framework refuses after it loaded the reports asks the linking job for
// nothing, and an admitted read asks for its pending reports
// (component-feedback-reports invariant 15).
func TestMCPFeedbackReportRefusedReadAsksNothing(t *testing.T) {
	h := newReportRoutesHarness(t)
	for i := range 60 {
		h.storeReport(fmt.Sprintf("pending-%02d", i), func(entry *store.FeedbackReportEntry) {
			entry.Summary = strings.Repeat("s", 1000)
		})
	}
	claimed := func() int {
		t.Helper()
		count := 0
		for _, report := range h.reports() {
			if report.LinkingLastAttemptAt != nil {
				count++
			}
		}
		return count
	}
	// One hundred served reports of 1,000-character summaries pass the
	// 65,536-byte output budget, so the framework refuses the read.
	if text := refusalAs(t, h.handler, h.manager(), "list_feedback_reports", map[string]any{"workspace_id": "alpha", "limit": 100}); !strings.Contains(text, "output budget") {
		t.Fatalf("refusal=%q", text)
	}
	h.settle()
	if n := claimed(); n != 0 {
		t.Fatalf("a refused read asked for linking: %d reports claimed", n)
	}
	readAs(t, h.handler, h.manager(), "list_feedback_reports", map[string]any{"workspace_id": "alpha", "limit": 5})
	h.settle()
	if n := claimed(); n == 0 {
		t.Fatal("an admitted read asked for no linking")
	}
}

// TestMCPFeedbackReportReadAsksForItsPage proves that list_feedback_reports
// asks the linking job for exactly the pending reports of the page it
// serves: a fresh first page of one, a snapshot follow-up page, a snapshot
// page with no pending report, the same snapshot page again after the
// one-minute window, and a fresh read of every report
// (component-feedback-reports invariants 15 and 24).
func TestMCPFeedbackReportReadAsksForItsPage(t *testing.T) {
	h := newReportRoutesHarness(t)
	asks := &linkingAsks{}
	asks.observe(h)
	p2, p1, _ := pendingReadFixture(h)
	args := map[string]any{"workspace_id": "alpha"}
	first := readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 1))
	if got := asks.take(); !slices.Equal(got, []string{p2.ID}) || first.Snapshot == "" {
		t.Fatalf("fresh first page asked for %v, want only %s", got, p2.ID)
	}
	readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 1, "offset", 1, "snapshot", first.Snapshot))
	if got := asks.take(); !slices.Equal(got, []string{p1.ID}) {
		t.Fatalf("snapshot second page asked for %v, want only %s", got, p1.ID)
	}
	readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 1, "offset", 2, "snapshot", first.Snapshot))
	if got := asks.take(); len(got) != 0 {
		t.Fatalf("snapshot page with no pending report asked for %v", got)
	}
	h.advance(61 * time.Second)
	readAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "limit", 1, "offset", 1, "snapshot", first.Snapshot))
	if got := asks.take(); !slices.Equal(got, []string{p1.ID}) {
		t.Fatalf("snapshot page after the window asked for %v, want only %s", got, p1.ID)
	}
	readAs(t, h.handler, h.manager(), "list_feedback_reports", args)
	if got := asks.take(); !slices.Equal(got, []string{p2.ID, p1.ID}) {
		t.Fatalf("fresh read of every report asked for %v", got)
	}
	// A read the framework refuses asks for nothing.
	refusalAs(t, h.handler, h.manager(), "list_feedback_reports", withArgs(args, "offset", 1))
	if got := asks.take(); len(got) != 0 {
		t.Fatalf("a refused read asked for %v", got)
	}
}
