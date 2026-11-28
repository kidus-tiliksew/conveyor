package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// reportRoutesHarness drives the report routes for people through the real
// handler on the memory backend (component-feedback-reports, Verification).
type reportRoutesHarness struct {
	*readsHarness
	userCtx context.Context
}

func newReportRoutesHarness(t *testing.T) *reportRoutesHarness {
	t.Helper()
	h := &reportRoutesHarness{readsHarness: newReadsHarness(t)}
	h.userCtx = store.WithActor(h.ctx, store.Actor{ID: "user:usr-operator", Role: core.ActorUser})
	t.Cleanup(h.settle)
	return h
}

// storeReport records one report through the store, so no linking job is
// asked for, and returns it.
func (h *reportRoutesHarness) storeReport(key string, edit func(*store.FeedbackReportEntry)) store.FeedbackReport {
	h.t.Helper()
	entry := store.FeedbackReportEntry{Kind: store.FeedbackReportError, Key: key, Summary: "Request failed for " + key}
	if edit != nil {
		edit(&entry)
	}
	h.advance(time.Second)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: h.clock(), Entries: []store.FeedbackReportEntry{entry}}); err != nil {
		h.t.Fatal(err)
	}
	return h.reportByKey(key)
}

// endLinking ends the linking of a report with the given links and notes,
// as the linking job would.
func (h *reportRoutesHarness) endLinking(report store.FeedbackReport, links []store.FeedbackReportLinkCandidate, notes []store.FeedbackReportNote) {
	h.t.Helper()
	at := h.clock()
	if _, claimed, err := h.st.ClaimFeedbackReportLinking(h.ctx, report.ID, at); err != nil || !claimed {
		h.t.Fatalf("claim linking of %s: claimed=%v err=%v", report.ID, claimed, err)
	}
	if finished, err := h.st.FinishFeedbackReportLinking(h.ctx, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: at, AttemptAt: at, Links: links, Notes: notes}); err != nil || !finished {
		h.t.Fatalf("finish linking of %s: finished=%v err=%v", report.ID, finished, err)
	}
}

// storeAct records an act through the store.
func (h *reportRoutesHarness) storeAct(act store.FeedbackReportAct) {
	h.t.Helper()
	if act.At.IsZero() {
		act.At = h.clock()
	}
	if _, err := h.st.RecordFeedbackReportAct(h.userCtx, act); err != nil {
		h.t.Fatal(err)
	}
}

func (h *reportRoutesHarness) mergedTask(crateID string) core.Task {
	h.t.Helper()
	return h.merged(h.ctx, readsCommit(fmt.Sprintf("%08x", readsTaskSequence.Load()+1)), crateID)
}

func (h *reportRoutesHarness) openTask() core.Task {
	h.t.Helper()
	id := fmt.Sprintf("%s-%d", core.NewTaskID(), readsTaskSequence.Add(1))
	task := core.Task{ID: id, Workspace: "alpha", Repo: "app", Title: "Open " + id, Body: "Open.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := h.st.CreateTask(h.ctx, task); err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *reportRoutesHarness) crate(name string) store.Crate {
	h.t.Helper()
	crate, err := h.st.CreateCrate(h.ctx, store.CrateInput{Name: name})
	if err != nil {
		h.t.Fatal(err)
	}
	return crate
}

// list reads one list page and returns the report IDs and the total header.
func (h *reportRoutesHarness) list(query string) ([]string, string) {
	h.t.Helper()
	response := h.want(http.StatusOK, http.MethodGet, "/v1/feedback-reports"+query, "")
	var reports []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &reports); err != nil {
		h.t.Fatalf("decode list %s: %v", response.Body, err)
	}
	ids := make([]string, len(reports))
	for i, report := range reports {
		ids[i] = report["id"].(string)
	}
	return ids, response.Header().Get("X-Conveyor-Total")
}

func (h *reportRoutesHarness) detail(id string) map[string]any {
	h.t.Helper()
	response := h.want(http.StatusOK, http.MethodGet, "/v1/feedback-reports/"+id, "")
	var report map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		h.t.Fatalf("decode detail %s: %v", response.Body, err)
	}
	return report
}

func requireJSONError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body, status, code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"] != code {
		t.Fatalf("body=%s, want error %s", response.Body, code)
	}
}

// servedFieldNames lists the members the design names for each object of a
// served report (component-feedback-reports, Public surfaces). Every served
// field name is read against this list.
var servedFieldNames = map[string][]string{
	"report":      {"id", "kind", "key", "summary", "environment", "service", "count", "first_seen", "last_seen", "file", "line", "first", "latest", "state", "closed", "reopened_at", "in_inbox", "links", "links_pending", "notes", "fix_task", "created_at"},
	"environment": {"id", "name", "listening", "last_signal_at"},
	"version":     {"repository", "commit", "version"},
	"closed":      {"at", "by", "reason"},
	"link":        {"id", "state", "task", "crate", "evidence", "confirmed_by", "confirmed_at"},
	"link_task":   {"id", "title", "crate"},
	"crate":       {"id", "name"},
	"evidence":    {"kind", "file", "line", "commit", "service", "version", "deployed_at", "source", "elapsed_minutes", "prior_signal_at"},
	"note":        {"kind", "deploy", "task_count", "rule", "reason", "file", "commit"},
	"deploy":      {"service", "version", "deployed_at", "source"},
	"fix_task":    {"id", "title"},
}

// requireServedFieldNames walks a served report and fails on any member the
// design does not name for its object. It returns every member it saw, by
// object.
func requireServedFieldNames(t *testing.T, report map[string]any) map[string]map[string]bool {
	t.Helper()
	seen := map[string]map[string]bool{}
	var walk func(kind string, value any)
	walk = func(kind string, value any) {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return
		}
		if seen[kind] == nil {
			seen[kind] = map[string]bool{}
		}
		for name, member := range object {
			if !slices.Contains(servedFieldNames[kind], name) {
				t.Fatalf("served %s holds %q, which the design does not name", kind, name)
			}
			seen[kind][name] = true
			child := map[string]string{"report.environment": "environment", "report.first": "version", "report.latest": "version", "report.closed": "closed",
				"report.fix_task": "fix_task", "link.task": "link_task", "link.crate": "crate", "link_task.crate": "crate", "evidence.version": "version",
				"note.deploy": "deploy", "deploy.version": "version"}[kind+"."+name]
			switch {
			case child != "":
				walk(child, member)
			case kind == "report" && name == "links":
				for _, link := range member.([]any) {
					walk("link", link)
				}
			case kind == "report" && name == "notes":
				for _, note := range member.([]any) {
					walk("note", note)
				}
			case kind == "link" && name == "evidence":
				for _, piece := range member.([]any) {
					walk("evidence", piece)
				}
			}
		}
	}
	walk("report", report)
	return seen
}

func TestFeedbackReportServedFields(t *testing.T) {
	h := newReportRoutesHarness(t)
	crate := h.crate("Spring")
	member, removed, fix := h.mergedTask(crate.ID), h.mergedTask(""), h.openTask()
	report := h.storeReport("crash", func(entry *store.FeedbackReportEntry) {
		entry.Service, entry.File, entry.Line, entry.Commit, entry.Version, entry.Repository = "api", "cmd/main.go", 12, "abcdef0", "1.2.3", "app"
		entry.Summary = "Crash with ghp_" + strings.Repeat("b", 36)
	})
	deployed := h.clock().Add(-time.Minute)
	prior := deployed.Add(-time.Minute)
	elapsed := int64(1)
	deploy := &store.FeedbackReportDeploy{ID: "dep-1", Service: "api", Version: store.FeedbackReportVersion{Repository: "app", Commit: readsCommit("ab"), Version: "1.2.3"}, DeployedAt: deployed, Source: "signal"}
	h.endLinking(report, []store.FeedbackReportLinkCandidate{
		{TaskID: member.ID, Evidence: []store.FeedbackReportEvidence{
			{Kind: store.FeedbackEvidenceLine, File: "cmd/main.go", Line: 12, Commit: readsCommit("cd")},
			{Kind: store.FeedbackEvidenceTiming, Deploy: deploy, ElapsedMinutes: &elapsed, PriorSignalAt: &prior},
			// A malformed piece proves nothing and is not served.
			{Kind: store.FeedbackEvidenceLine, File: "", Line: 0, Commit: "nope"},
		}},
		{TaskID: removed.ID, Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceLine, File: "a.go", Line: 1, Commit: readsCommit("ef")}}},
	}, []store.FeedbackReportNote{
		{Kind: store.FeedbackNoteTooManyTasks, Deploy: deploy, TaskCount: 11},
		{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: store.FeedbackReasonNoGitHubApp},
		{Kind: "unknown_kind"},
	})
	links, err := h.st.ListFeedbackReportLinks(h.ctx, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.TaskID == removed.ID {
			h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, ReportID: report.ID, LinkID: link.ID})
		}
	}
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: report.ID, CrateID: crate.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, ReportID: report.ID, TaskID: fix.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: report.ID, Reason: "Shipped the fix"})

	served := h.detail(report.ID)
	seen := requireServedFieldNames(t, served)
	// Every object the design names is served with each of its members.
	for kind, names := range servedFieldNames {
		for _, name := range names {
			// The two notes served name a deploy, a task count, a rule, and a
			// reason, and no file.
			if kind == "note" {
				continue
			}
			if !seen[kind][name] {
				t.Fatalf("served %s lacks %q", kind, name)
			}
		}
	}
	raw, _ := json.Marshal(served)
	for _, never := range []string{"line_progress", "timing_result", "summary_redactions", "origin", "linking_last_attempt_at", "linking_ended_at", "fix_task_set_by", "removed", removed.ID, "dep-1", "unknown_kind", "ghp_"} {
		if strings.Contains(string(raw), never) {
			t.Fatalf("served report holds %q: %s", never, raw)
		}
	}
	if got := served["links"].([]any); len(got) != 2 {
		t.Fatalf("served links=%v, want the member link and the crate link", got)
	}
	memberLink := served["links"].([]any)[0].(map[string]any)
	if memberLink["state"] != store.FeedbackReportLinkPossible || memberLink["task"].(map[string]any)["crate"].(map[string]any)["name"] != "Spring" || len(memberLink["evidence"].([]any)) != 2 {
		t.Fatalf("member link=%v", memberLink)
	}
	if _, confirmed := memberLink["confirmed_by"]; confirmed {
		t.Fatalf("a possible link carries confirmed_by: %v", memberLink)
	}
	if notes := served["notes"].([]any); len(notes) != 2 {
		t.Fatalf("notes=%v, want the two well-formed notes", notes)
	}
	if served["closed"].(map[string]any)["reason"] != "Shipped the fix" || served["in_inbox"] != false || served["links_pending"] != false || served["fix_task"].(map[string]any)["id"] != fix.ID {
		t.Fatalf("served report=%v", served)
	}
	// The line holds the stated values and nothing of another workspace.
	if served["line"] != float64(12) || served["file"] != "cmd/main.go" || served["first"].(map[string]any)["version"] != "1.2.3" {
		t.Fatalf("served entry fields=%v", served)
	}
}

func TestFeedbackReportListRoute(t *testing.T) {
	h := newReportRoutesHarness(t)
	staging := h.create(`{"name":"Staging"}`)
	stagingKey := h.issue(staging.ID)
	crate := h.crate("Spring")
	member, outsider := h.mergedTask(crate.ID), h.mergedTask("")
	a := h.storeReport("a", nil)
	b := h.storeReport("b", nil)
	c := h.storeReport("c", nil)
	closed := h.storeReport("closed", nil)
	h.advance(time.Second)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: staging.ID, KeyID: stagingKey.ID, ArrivedAt: h.clock(),
		Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportAlert, Key: "s", Summary: "Staging alert"}}}); err != nil {
		t.Fatal(err)
	}
	s, err := h.st.ListEnvironmentFeedbackReports(h.ctx, staging.ID)
	if err != nil || len(s) != 1 {
		t.Fatalf("staging reports=%v err=%v", s, err)
	}
	for _, report := range []store.FeedbackReport{a, b, c, closed, s[0]} {
		h.endLinking(report, nil, nil)
	}
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: a.ID, TaskID: member.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: a.ID, CrateID: crate.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: b.ID, TaskID: outsider.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: closed.ID, Reason: "done"})
	newestFirst := []string{s[0].ID, c.ID, b.ID, a.ID}
	for _, c2 := range []struct {
		query string
		want  []string
	}{
		{"", newestFirst},
		{"?state=open", newestFirst},
		{"?state=closed", []string{closed.ID}},
		{"?environment_id=" + staging.ID, []string{s[0].ID}},
		{"?task_id=" + member.ID, []string{a.ID}},
		{"?task_id=" + outsider.ID, []string{b.ID}},
		{"?crate_id=" + crate.ID, []string{a.ID}},
		{"?no_crate=true", []string{s[0].ID, c.ID, b.ID}},
		{"?no_crate=false", newestFirst},
		{"?crate_id=crate-missing", []string{}},
		{"?limit=2", newestFirst[:2]},
		{"?limit=2&offset=2", newestFirst[2:]},
		{"?limit=2&offset=9", []string{}},
	} {
		ids, total := h.list(c2.query)
		wantTotal := len(c2.want)
		if strings.Contains(c2.query, "limit") {
			wantTotal = len(newestFirst)
		}
		if !slices.Equal(ids, c2.want) || total != fmt.Sprint(wantTotal) {
			t.Fatalf("list%s ids=%v total=%s, want %v of %d", c2.query, ids, total, c2.want, wantTotal)
		}
	}
	// The default page carries the list headers.
	response := h.want(http.StatusOK, http.MethodGet, "/v1/feedback-reports", "")
	if response.Header().Get("X-Conveyor-Limit") != fmt.Sprint(defaultFeedbackReportPageLimit) || response.Header().Get("X-Conveyor-Offset") != "0" {
		t.Fatalf("headers=%v", response.Header())
	}
	for _, refused := range []struct{ query, field string }{
		{"?state=all", "state"}, {"?state=open&state=closed", "state"}, {"?state=", "state"},
		{"?environment_id=", "environment_id"}, {"?task_id=a&task_id=b", "task_id"}, {"?crate_id=", "crate_id"},
		{"?no_crate=yes", "no_crate"}, {"?no_crate=true&no_crate=true", "no_crate"},
	} {
		response := h.do(h.manager(), http.MethodGet, "/v1/feedback-reports"+refused.query, "")
		requireJSONError(t, response, http.StatusBadRequest, "invalid_report_query")
		if !strings.Contains(response.Body.String(), `"field":"`+refused.field+`"`) {
			t.Fatalf("refusal of %s=%s", refused.query, response.Body)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?offset=1", "?limit=1&offset=-1"} {
		if response := h.do(h.manager(), http.MethodGet, "/v1/feedback-reports"+query, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("list%s status=%d", query, response.Code)
		}
	}
}

func TestFeedbackReportReadAccess(t *testing.T) {
	h := newReportRoutesHarness(t)
	report := h.storeReport("crash", nil)
	h.endLinking(report, nil, nil)
	foreignEnvironment, err := h.st.CreateEnvironment(h.betaCtx, store.EnvironmentInput{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	foreignKey, err := h.st.IssueEnvironmentKey(h.betaCtx, foreignEnvironment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordFeedbackReports(h.betaCtx, store.FeedbackReportBatch{EnvironmentID: foreignEnvironment.ID, KeyID: foreignKey.ID, ArrivedAt: h.clock(),
		Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportError, Key: "beta-only", Summary: "Beta secret text"}}}); err != nil {
		t.Fatal(err)
	}
	foreignReports, err := h.st.ListEnvironmentFeedbackReports(h.betaCtx, foreignEnvironment.ID)
	if err != nil || len(foreignReports) != 1 {
		t.Fatalf("beta reports=%v err=%v", foreignReports, err)
	}
	// Every role of the workspace reads.
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		if response := h.do(h.tokens[role], http.MethodGet, "/v1/feedback-reports/"+report.ID, ""); response.Code != http.StatusOK {
			t.Fatalf("%s read status=%d", role, response.Code)
		}
	}
	// A member of another workspace gets the uniform 404 before any lookup.
	for _, path := range []string{"/v1/feedback-reports", "/v1/feedback-reports/" + report.ID} {
		requireJSONError(t, h.do(h.foreign, http.MethodGet, path, ""), http.StatusNotFound, "workspace_not_found")
	}
	if response := h.do("not-a-token", http.MethodGet, "/v1/feedback-reports", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown token status=%d", response.Code)
	}
	// A report of another workspace answers exactly as a missing one, and no
	// list serves its text.
	missing := h.do(h.manager(), http.MethodGet, "/v1/feedback-reports/rpt-261125-000000", "")
	foreign := h.do(h.manager(), http.MethodGet, "/v1/feedback-reports/"+foreignReports[0].ID, "")
	requireJSONError(t, missing, http.StatusNotFound, "report_not_found")
	if foreign.Code != missing.Code || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("a report of another workspace=%d %s, a missing one=%d %s", foreign.Code, foreign.Body, missing.Code, missing.Body)
	}
	for _, query := range []string{"", "?state=closed", "?environment_id=" + foreignEnvironment.ID} {
		if body := h.want(http.StatusOK, http.MethodGet, "/v1/feedback-reports"+query, "").Body.String(); strings.Contains(body, "Beta secret text") || strings.Contains(body, foreignReports[0].ID) {
			t.Fatalf("list%s served another workspace: %s", query, body)
		}
	}
}

func TestFeedbackReportListening(t *testing.T) {
	h := newReportRoutesHarness(t)
	report := h.storeReport("crash", nil)
	h.endLinking(report, nil, nil)
	environment := h.detail(report.ID)["environment"].(map[string]any)
	if environment["listening"] != false || environment["last_signal_at"] != nil || environment["name"] != "Production" {
		t.Fatalf("environment with no signal=%v", environment)
	}
	h.accept(feedbackMinimalSignal)
	signalAt := store.EnvironmentTime(h.clock())
	environment = h.detail(report.ID)["environment"].(map[string]any)
	if environment["listening"] != true || environment["last_signal_at"] != signalAt.Format(time.RFC3339Nano) {
		t.Fatalf("environment while listening=%v, want last_signal_at %s", environment, signalAt.Format(time.RFC3339Nano))
	}
	// Twice the signal's 60-second interval later, no watcher is listening,
	// and the last signal time is kept.
	h.advance(2 * time.Minute)
	environment = h.detail(report.ID)["environment"].(map[string]any)
	if environment["listening"] != false || environment["last_signal_at"] != signalAt.Format(time.RFC3339Nano) {
		t.Fatalf("environment after listening lapsed=%v", environment)
	}
}

func TestFeedbackReportReadAsksLinkingAgain(t *testing.T) {
	h := newReportRoutesHarness(t)
	pending := h.storeReport("pending", nil)
	linked := h.storeReport("linked", nil)
	h.endLinking(linked, nil, nil)
	linkedBefore := h.reportByKey("linked")
	if served := h.detail(pending.ID); served["links_pending"] != true {
		t.Fatalf("pending report=%v", served)
	}
	h.settle()
	// The read asked the linking job, which claimed an attempt and, with no
	// rule that applies, ended linking. The read itself linked nothing.
	if after := h.reportByKey("pending"); after.LinkingLastAttemptAt == nil || after.LinkingEndedAt == nil {
		t.Fatalf("pending report after a read=%+v, want a claimed attempt and linking ended", after)
	}
	h.list("")
	h.settle()
	if after := h.reportByKey("linked"); !after.LinkingLastAttemptAt.Equal(*linkedBefore.LinkingLastAttemptAt) {
		t.Fatalf("a read of a linked report claimed another attempt: %+v", after)
	}
	if served := h.detail(pending.ID); served["links_pending"] != false {
		t.Fatalf("report after linking ended=%v", served)
	}
}

// faultyReportStore fails chosen report calls, as a store failure would.
type faultyReportStore struct {
	store.Backend
	err error
}

func (f *faultyReportStore) ListFeedbackReports(context.Context, store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	return store.FeedbackReportPage{}, f.err
}

func (f *faultyReportStore) GetFeedbackReport(context.Context, string) (store.FeedbackReport, error) {
	return store.FeedbackReport{}, f.err
}

func (f *faultyReportStore) RecordFeedbackReportAct(context.Context, store.FeedbackReportAct) (store.FeedbackReportActResult, error) {
	return store.FeedbackReportActResult{}, f.err
}

func (f *faultyReportStore) ReadFeedbackReportDetails(context.Context, []store.FeedbackReport) (store.FeedbackReportDetails, error) {
	return store.FeedbackReportDetails{}, f.err
}

// TestFeedbackReportRouteStoreFailures sends every route through a store that
// asks to be sent again and through one that fails with driver text: each
// path answers 503 retryable with Retry-After, or a generic 500 that carries
// no store text (DEC-105(1); component-persistence invariant 8).
func TestFeedbackReportRouteStoreFailures(t *testing.T) {
	h := newReportRoutesHarness(t)
	report := h.storeReport("crash", nil)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/feedback-reports", ""},
		{http.MethodGet, "/v1/feedback-reports/" + report.ID, ""},
		{http.MethodPost, "/v1/feedback-reports/" + report.ID + "/links", `{"crate_id":"crate-x"}`},
		{http.MethodDelete, "/v1/feedback-reports/" + report.ID + "/links/lnk:x", ""},
		{http.MethodPost, "/v1/feedback-reports/" + report.ID + "/close", `{"reason":"done"}`},
		{http.MethodPost, "/v1/feedback-reports/" + report.ID + "/reopen", ""},
		{http.MethodPut, "/v1/feedback-reports/" + report.ID + "/fix-task", `{"task_id":"task-x"}`},
	}
	original := h.server.Store
	defer func() { h.server.Store = original }()
	h.server.Store = &faultyReportStore{Backend: h.st, err: fmt.Errorf("deadlock: %w", store.ErrRetryable)}
	for _, route := range routes {
		response := h.do(h.manager(), route.method, route.path, route.body)
		requireJSONError(t, response, http.StatusServiceUnavailable, "retryable")
		if response.Header().Get("Retry-After") != "1" || strings.Contains(response.Body.String(), "deadlock") {
			t.Fatalf("%s %s retryable answer=%v %s", route.method, route.path, response.Header(), response.Body)
		}
	}
	h.server.Store = &faultyReportStore{Backend: h.st, err: errors.New("pq: relation feedback_reports SECRET-DRIVER-TEXT")}
	for _, route := range routes {
		response := h.do(h.manager(), route.method, route.path, route.body)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "SECRET-DRIVER-TEXT") || strings.Contains(response.Body.String(), "feedback_reports") {
			t.Fatalf("%s %s store failure=%d %s", route.method, route.path, response.Code, response.Body)
		}
	}
}

func TestFeedbackReportCrateOpenCounts(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+h.environment.ID, `{"tracked_branches":{"app":"main"}}`)
	staging := h.create(`{"name":"Staging"}`)
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+staging.ID, `{"tracked_branches":{"app":"main"}}`)
	crate := h.crate("Spring")
	member := h.mergedTask(crate.ID)
	direct, viaMember, both, closedReport, removed, unlinked := h.storeReport("direct", nil), h.storeReport("member", nil), h.storeReport("both", nil), h.storeReport("closed", nil), h.storeReport("removed", nil), h.storeReport("none", nil)
	for _, report := range []store.FeedbackReport{direct, viaMember, both, closedReport, removed, unlinked} {
		h.endLinking(report, nil, nil)
	}
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: direct.ID, CrateID: crate.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: viaMember.ID, TaskID: member.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: both.ID, TaskID: member.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: both.ID, CrateID: crate.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: closedReport.ID, CrateID: crate.ID})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: closedReport.ID, Reason: "done"})
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: removed.ID, CrateID: crate.ID})
	links, err := h.st.ListFeedbackReportLinks(h.ctx, removed.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, ReportID: removed.ID, LinkID: links[0].ID})
	counts := func(raw json.RawMessage) map[string]int {
		t.Helper()
		var lines []feedback.CrateLine
		if err := json.Unmarshal(raw, &lines); err != nil {
			t.Fatalf("decode lines %s: %v", raw, err)
		}
		out := map[string]int{}
		for _, line := range lines {
			out[line.Environment.ID] = line.OpenReportCount
		}
		return out
	}
	httpCounts := counts(decodeFields(t, h.want(http.StatusOK, http.MethodGet, "/v1/crates/"+crate.ID, "").Body.Bytes())["environments"])
	page := readAs(t, h.handler, h.manager(), "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})
	mcpCounts := counts(decodeFields(t, page.Items[0])["environments"])
	want := map[string]int{h.environment.ID: 3, staging.ID: 0}
	if fmt.Sprint(httpCounts) != fmt.Sprint(want) || fmt.Sprint(mcpCounts) != fmt.Sprint(want) {
		t.Fatalf("crate read counts=%v, get_crate counts=%v, want %v", httpCounts, mcpCounts, want)
	}
	// The count and the crate's report list agree.
	if ids, total := h.list("?crate_id=" + crate.ID + "&environment_id=" + h.environment.ID); total != "3" {
		t.Fatalf("crate list=%v total=%s", ids, total)
	}
	raw := h.want(http.StatusOK, http.MethodGet, "/v1/crates/"+crate.ID, "").Body.String()
	if !strings.Contains(raw, `"open_report_count":0`) {
		t.Fatalf("a line with no report lacks a zero count: %s", raw)
	}
	keys := []string{}
	for key := range decodeFields(t, page.Items[0]) {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if slices.Contains(keys, "open_report_count") {
		t.Fatalf("get_crate carries the count outside its lines: %v", keys)
	}
}
