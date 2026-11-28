package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// reportActState renders everything an act can change and the lists an act
// must never change: the report and its links, the report events, and the
// task, work-order, and next-action reads (component-feedback-reports
// invariant 1; req-production-feedback AC-11.1).
func (h *reportRoutesHarness) reportActState(reportID string) (report string, delivery string) {
	h.t.Helper()
	stored, err := h.st.GetFeedbackReport(h.ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	links, err := h.st.ListFeedbackReportLinks(h.ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	events, err := h.st.ListEvents(h.ctx, "")
	if err != nil {
		h.t.Fatal(err)
	}
	reportEvents := []core.Event{}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "feedback_report.") {
			reportEvents = append(reportEvents, event)
		}
	}
	raw, _ := json.Marshal([]any{stored, links, reportEvents})
	parts := []string{}
	for _, path := range []string{"/v1/tasks", "/v1/tasks?state=merged", "/v1/work-orders", "/v1/next-actions"} {
		parts = append(parts, h.want(http.StatusOK, http.MethodGet, path, "").Body.String())
	}
	return string(raw), strings.Join(parts, "\n")
}

func TestFeedbackReportActRoutes(t *testing.T) {
	h := newReportRoutesHarness(t)
	crate := h.crate("Spring")
	cause, other := h.mergedTask(""), h.mergedTask("")
	fix, open := h.openTask(), h.openTask()
	report := h.storeReport("crash", nil)
	h.endLinking(report, []store.FeedbackReportLinkCandidate{{TaskID: cause.ID, Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceLine, File: "a.go", Line: 1, Commit: readsCommit("ab")}}}}, nil)
	links, err := h.st.ListFeedbackReportLinks(h.ctx, report.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links=%v err=%v", links, err)
	}
	base := "/v1/feedback-reports/" + report.ID
	_, deliveryBefore := h.reportActState(report.ID)

	// Each act is bracketed by its own read of the task, work-order, and
	// next-action lists, so no act can hide a delivery change behind a later
	// one (component-feedback-reports invariant 1; req-production-feedback
	// AC-11.1).
	acts := 0
	act := func(method, path, body string) map[string]any {
		t.Helper()
		_, deliveryBeforeAct := h.reportActState(report.ID)
		response := h.do(h.manager(), method, base+path, body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d body=%s", method, path, response.Code, response.Body)
		}
		if _, deliveryAfterAct := h.reportActState(report.ID); deliveryAfterAct != deliveryBeforeAct {
			t.Fatalf("%s %s changed delivery:\nbefore %s\nafter  %s", method, path, deliveryBeforeAct, deliveryAfterAct)
		}
		acts++
		var served map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &served); err != nil {
			t.Fatal(err)
		}
		requireServedFieldNames(t, served)
		return served
	}
	unchanged := func(method, path, body string) {
		t.Helper()
		before, _ := h.reportActState(report.ID)
		act(method, path, body)
		if after, _ := h.reportActState(report.ID); after != before {
			t.Fatalf("%s %s changed nothing yet recorded something", method, path)
		}
	}

	// Confirming the possible link answers the served report with the link
	// confirmed, and takes the report out of the Inbox.
	served := act(http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q}`, cause.ID))
	link := served["links"].([]any)[0].(map[string]any)
	if link["state"] != store.FeedbackReportLinkConfirmed || link["confirmed_by"] == nil || served["in_inbox"] != false {
		t.Fatalf("after confirming=%v", served)
	}
	unchanged(http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q}`, cause.ID))
	served = act(http.MethodPost, "/links", fmt.Sprintf(`{"crate_id":%q}`, crate.ID))
	if len(served["links"].([]any)) != 2 {
		t.Fatalf("after adding a crate link=%v", served)
	}
	served = act(http.MethodDelete, "/links/"+links[0].ID, "")
	if len(served["links"].([]any)) != 1 || served["state"] != store.FeedbackReportOpen {
		t.Fatalf("after removing=%v, want the removed link gone and the report open", served)
	}
	unchanged(http.MethodDelete, "/links/"+links[0].ID, "{}")
	served = act(http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[%q,%q]}`, fix.ID, cause.ID, other.ID))
	if served["fix_task"].(map[string]any)["id"] != fix.ID || len(served["links"].([]any)) != 3 {
		t.Fatalf("after the fix task=%v", served)
	}
	unchanged(http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q}`, fix.ID))
	served = act(http.MethodPost, "/close", `{"reason":"Shipped"}`)
	if served["state"] != store.FeedbackReportClosed || served["closed"].(map[string]any)["reason"] != "Shipped" {
		t.Fatalf("after closing=%v", served)
	}
	// A closed report refuses every act but reopening, and records nothing.
	for _, refused := range []struct{ method, path, body string }{
		{http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q}`, other.ID)},
		{http.MethodDelete, "/links/" + links[0].ID, ""},
		{http.MethodPost, "/close", `{"reason":"again"}`},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q}`, open.ID)},
	} {
		before, deliveryBeforeAct := h.reportActState(report.ID)
		requireJSONError(t, h.do(h.manager(), refused.method, base+refused.path, refused.body), http.StatusConflict, "report_closed")
		after, deliveryAfterAct := h.reportActState(report.ID)
		if after != before {
			t.Fatalf("a refused %s %s recorded something", refused.method, refused.path)
		}
		if deliveryAfterAct != deliveryBeforeAct {
			t.Fatalf("a refused %s %s changed delivery", refused.method, refused.path)
		}
	}
	served = act(http.MethodPost, "/reopen", "")
	if served["state"] != store.FeedbackReportOpen || served["reopened_at"] == nil || served["closed"] != nil || len(served["links"].([]any)) != 3 {
		t.Fatalf("after reopening=%v", served)
	}
	unchanged(http.MethodPost, "/reopen", "{}")
	// No act changed a task, a work order, or a next action.
	if _, deliveryAfter := h.reportActState(report.ID); deliveryAfter != deliveryBefore {
		t.Fatalf("the acts changed delivery:\nbefore %s\nafter  %s", deliveryBefore, deliveryAfter)
	}
	// Six acts that change the report and four that change nothing.
	if acts != 10 {
		t.Fatalf("bracketed %d acts, want 10", acts)
	}
}

func TestFeedbackReportActRefusals(t *testing.T) {
	h := newReportRoutesHarness(t)
	crate := h.crate("Spring")
	cause := h.mergedTask("")
	open := h.openTask()
	report := h.storeReport("crash", nil)
	h.endLinking(report, nil, nil)
	base := "/v1/feedback-reports/" + report.ID
	foreignCrate, err := h.st.CreateCrate(h.betaCtx, store.CrateInput{Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/links", fmt.Sprintf(`{"crate_id":%q}`, crate.ID)},
		{http.MethodDelete, "/links/lnk:x", ""},
		{http.MethodPost, "/close", `{"reason":"done"}`},
		{http.MethodPost, "/reopen", ""},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q}`, open.ID)},
	}
	before, _ := h.reportActState(report.ID)
	requireNothing := func(what string) {
		t.Helper()
		if after, _ := h.reportActState(report.ID); after != before {
			t.Fatalf("%s recorded something", what)
		}
	}
	// The credential, the workspace, and the capability to create tasks are
	// decided first: a viewer and a contributor get the uniform 404, as a
	// member of another workspace does.
	for _, route := range routes {
		if response := h.do("not-a-token", route.method, base+route.path, route.body); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with an unknown token=%d", route.method, route.path, response.Code)
		}
		for _, token := range []string{h.tokens[core.WorkspaceRoleViewer], h.tokens[core.WorkspaceRoleContributor], h.foreign} {
			requireJSONError(t, h.do(token, route.method, base+route.path, route.body), http.StatusNotFound, "workspace_not_found")
		}
		// A body over the provisional 16 KiB bound answers 413.
		huge := `{"reason":"` + strings.Repeat("a", feedbackReportActBodyLimit) + `"}`
		if response := h.do(h.manager(), route.method, base+route.path, huge); response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s %s with a body over the bound=%d %s", route.method, route.path, response.Code, response.Body)
		}
	}
	requireNothing("a refused credential, capability, or body")
	marker := "SECRET-REASON-MARKER"
	for _, refused := range []struct {
		method, path, body, field string
	}{
		{http.MethodPost, "/links", `not json`, "body"},
		{http.MethodPost, "/links", `[]`, "body"},
		{http.MethodPost, "/links", `{}`, "body"},
		{http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q,"crate_id":%q}`, cause.ID, crate.ID), "body"},
		{http.MethodPost, "/links", `{"task_id":null}`, "task_id"},
		{http.MethodPost, "/links", `{"task_id":""}`, "task_id"},
		{http.MethodPost, "/links", `{"task_id":7}`, "task_id"},
		{http.MethodPost, "/links", `{"task_id":"a","task_id":"b"}`, "body"},
		{http.MethodPost, "/links", `{"link_id":"x"}`, "body"},
		{http.MethodDelete, "/links/lnk:x", `{"reason":"x"}`, "body"},
		{http.MethodPost, "/close", `{}`, "reason"},
		{http.MethodPost, "/close", `{"reason":""}`, "reason"},
		{http.MethodPost, "/close", `{"reason":null}`, "reason"},
		{http.MethodPost, "/close", `{"reason":"` + marker + strings.Repeat("é", store.FeedbackReportCloseReasonMax) + `"}`, "reason"},
		{http.MethodPost, "/close", `{"reason":"` + marker + `\u0000"}`, "reason"},
		{http.MethodPost, "/close", `{"reason":"` + marker + `\ud800"}`, "reason"},
		{http.MethodPost, "/close", `{"reason":"done","extra":1}`, "body"},
		{http.MethodPost, "/reopen", `{"reason":"x"}`, "body"},
		{http.MethodPut, "/fix-task", `{}`, "task_id"},
		{http.MethodPut, "/fix-task", `{"cause_task_ids":[]}`, "task_id"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":null}`, open.ID), "cause_task_ids"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":"x"}`, open.ID), "cause_task_ids"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[""]}`, open.ID), "cause_task_ids"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[1]}`, open.ID), "cause_task_ids"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[%q,%q]}`, open.ID, cause.ID, cause.ID), "cause_task_ids"},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[%s]}`, open.ID, strings.TrimSuffix(strings.Repeat(`"t",`, store.MaxFeedbackReportCauseTasks+1), ",")), "cause_task_ids"},
	} {
		response := h.do(h.manager(), refused.method, base+refused.path, refused.body)
		requireJSONError(t, response, http.StatusBadRequest, "invalid_report_act")
		if !strings.Contains(response.Body.String(), `"field":"`+refused.field+`"`) || strings.Contains(response.Body.String(), marker) {
			t.Fatalf("%s %s %s refusal=%s, want field %s and no submitted value", refused.method, refused.path, refused.body, response.Body, refused.field)
		}
	}
	requireNothing("a refused body")
	// The targets are decided in the store transaction before any write.
	for _, refused := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{http.MethodPost, "/links", `{"task_id":"task-missing"}`, http.StatusNotFound, store.FeedbackActTaskNotFound},
		{http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q}`, open.ID), http.StatusConflict, store.FeedbackActTaskNotMerged},
		{http.MethodPost, "/links", `{"crate_id":"crate-missing"}`, http.StatusNotFound, store.FeedbackActCrateNotFound},
		{http.MethodPost, "/links", fmt.Sprintf(`{"crate_id":%q}`, foreignCrate.ID), http.StatusNotFound, store.FeedbackActCrateNotFound},
		{http.MethodDelete, "/links/lnk:missing", "", http.StatusNotFound, store.FeedbackActLinkNotFound},
		{http.MethodPut, "/fix-task", `{"task_id":"task-missing"}`, http.StatusNotFound, store.FeedbackActTaskNotFound},
		{http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[%q,%q]}`, open.ID, cause.ID, open.ID), http.StatusConflict, store.FeedbackActTaskNotMerged},
	} {
		requireJSONError(t, h.do(h.manager(), refused.method, base+refused.path, refused.body), refused.status, refused.code)
	}
	requireNothing("a refused target")
	// A report the workspace does not hold answers 404 on every act.
	for _, route := range routes {
		requireJSONError(t, h.do(h.manager(), route.method, "/v1/feedback-reports/rpt-261125-000000"+route.path, route.body), http.StatusNotFound, "report_not_found")
	}
	// Exactly 50 distinct merged causes are accepted.
	causes := make([]string, store.MaxFeedbackReportCauseTasks)
	for i := range causes {
		causes[i] = fmt.Sprintf("%q", h.mergedTask("").ID)
	}
	if response := h.do(h.manager(), http.MethodPut, base+"/fix-task", fmt.Sprintf(`{"task_id":%q,"cause_task_ids":[%s]}`, open.ID, strings.Join(causes, ","))); response.Code != http.StatusOK {
		t.Fatalf("50 causes=%d %s", response.Code, response.Body)
	}
	// A close reason of 500 characters of two bytes each is accepted and
	// served redacted.
	secret := "ghp_" + strings.Repeat("c", 36)
	reason := "Leaked " + secret + " " + strings.Repeat("é", store.FeedbackReportCloseReasonMax-len("Leaked  ")-len(secret))
	response := h.do(h.manager(), http.MethodPost, base+"/close", fmt.Sprintf(`{"reason":%q}`, reason))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("close with a secret=%d %s", response.Code, response.Body)
	}
	if stored := h.reportByKey("crash"); strings.Contains(stored.CloseReason, secret) {
		t.Fatalf("stored reason holds the secret: %q", stored.CloseReason)
	}
}

// actFaultStore wraps the harness store so a test can drive each step of an
// act route: it fails every report read, it fails the act before anything is
// written, or it runs a step right after the act commits.
type actFaultStore struct {
	store.Backend
	readErr  error
	actErr   error
	afterAct func()
}

func (f *actFaultStore) ReadFeedbackReport(ctx context.Context, id string) (store.FeedbackReport, store.FeedbackReportDetails, error) {
	if f.readErr != nil {
		return store.FeedbackReport{}, store.FeedbackReportDetails{}, f.readErr
	}
	return f.Backend.ReadFeedbackReport(ctx, id)
}

func (f *actFaultStore) ReadFeedbackReports(ctx context.Context, ids []string) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	if f.readErr != nil {
		return nil, store.FeedbackReportDetails{}, f.readErr
	}
	return f.Backend.ReadFeedbackReports(ctx, ids)
}

func (f *actFaultStore) ListFeedbackReports(ctx context.Context, query store.FeedbackReportQuery) (store.FeedbackReportPage, error) {
	if f.readErr != nil {
		return store.FeedbackReportPage{}, f.readErr
	}
	return f.Backend.ListFeedbackReports(ctx, query)
}

func (f *actFaultStore) RecordFeedbackReportAct(ctx context.Context, act store.FeedbackReportAct) (store.FeedbackReportActResult, error) {
	if f.actErr != nil {
		return store.FeedbackReportActResult{}, f.actErr
	}
	result, err := f.Backend.RecordFeedbackReportAct(ctx, act)
	if err == nil && f.afterAct != nil {
		f.afterAct()
	}
	return result, err
}

// failingWriter is a response writer whose connection is gone: every write
// fails.
type failingWriter struct {
	header http.Header
	status int
}

func (w *failingWriter) Header() http.Header       { return w.header }
func (w *failingWriter) WriteHeader(status int)    { w.status = status }
func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection closed") }

// actRoute sets up a fresh report for one act route and answers its request.
type actRoute struct {
	name string
	// setup stores a report in the state the act needs and answers the
	// request; repeat is a request that changes nothing or is refused once
	// the act is recorded.
	setup func(h *reportRoutesHarness, key string) (reportID, method, path, body string)
}

func feedbackActRoutes() []actRoute {
	return []actRoute{
		{"confirm a link", func(h *reportRoutesHarness, key string) (string, string, string, string) {
			task := h.mergedTask("")
			report := h.storeReport(key, nil)
			return report.ID, http.MethodPost, "/links", fmt.Sprintf(`{"task_id":%q}`, task.ID)
		}},
		{"remove a link", func(h *reportRoutesHarness, key string) (string, string, string, string) {
			task := h.mergedTask("")
			report := h.storeReport(key, nil)
			h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: report.ID, TaskID: task.ID})
			links, err := h.st.ListFeedbackReportLinks(h.ctx, report.ID)
			if err != nil || len(links) != 1 {
				h.t.Fatalf("links=%v err=%v", links, err)
			}
			return report.ID, http.MethodDelete, "/links/" + links[0].ID, ""
		}},
		{"close", func(h *reportRoutesHarness, key string) (string, string, string, string) {
			return h.storeReport(key, nil).ID, http.MethodPost, "/close", `{"reason":"fixed"}`
		}},
		{"reopen", func(h *reportRoutesHarness, key string) (string, string, string, string) {
			report := h.storeReport(key, nil)
			h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: report.ID, Reason: "done"})
			return report.ID, http.MethodPost, "/reopen", ""
		}},
		{"fix task", func(h *reportRoutesHarness, key string) (string, string, string, string) {
			task := h.openTask()
			return h.storeReport(key, nil).ID, http.MethodPut, "/fix-task", fmt.Sprintf(`{"task_id":%q}`, task.ID)
		}},
	}
}

// TestFeedbackReportActAnswersMatchTheStore proves, for each of the five act
// routes, that an answer says "send it again" only when nothing was
// written, and that once an act has committed the caller always learns that
// it succeeded (component-persistence invariant 8; component-feedback-reports
// v4 invariants 17 to 20). The store reads the answer in the act's own
// transaction, so each step after the commit is driven here with a fault:
// a report read that answers retryable or fails, a request cancelled right
// after the commit, and a connection that is gone when the answer is
// written. The act still answers 200 with the served report as it stands
// after the act, or, with no connection, is recorded and still asks the
// linking job. A failure before the commit (retryable, any other store
// error, a cancelled request) writes nothing and answers 503 retryable or a
// generic 500. An act that changes nothing, and a refused act, answer from
// the act alone while every read fails.
func TestFeedbackReportActAnswersMatchTheStore(t *testing.T) {
	h := newReportRoutesHarness(t)
	base := h.server.Store.(store.Backend)
	n := 0
	for _, route := range feedbackActRoutes() {
		// Background linking work from an earlier case settles first, so a
		// before-and-after comparison sees only the act under test.
		fresh := func() (string, string, string, string) {
			h.settle()
			n++
			return route.setup(h, fmt.Sprintf("act-%02d", n))
		}
		request := func(reportID, method, path, body string) *httptest.ResponseRecorder {
			return h.do(h.manager(), method, "/v1/feedback-reports/"+reportID+path, body)
		}
		committed := func(name, reportID, before string, response *httptest.ResponseRecorder) {
			t.Helper()
			if response.Code != http.StatusOK {
				t.Fatalf("%s, %s: status=%d body=%s, want 200 for a recorded act", route.name, name, response.Code, response.Body)
			}
			var served map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &served); err != nil || served["id"] != reportID {
				t.Fatalf("%s, %s: body=%s err=%v, want the served report", route.name, name, response.Body, err)
			}
			requireServedFieldNames(t, served)
			if after, _ := h.reportActState(reportID); after == before {
				t.Fatalf("%s, %s: the store holds no act", route.name, name)
			}
		}
		for _, fault := range []struct {
			name    string
			install func(f *actFaultStore, cancel context.CancelFunc)
		}{
			{"the report read answers retryable", func(f *actFaultStore, _ context.CancelFunc) {
				f.readErr = fmt.Errorf("%w: report view moved", store.ErrRetryable)
			}},
			{"the report read fails", func(f *actFaultStore, _ context.CancelFunc) { f.readErr = errors.New("read failed") }},
			{"the request is cancelled after the commit", func(f *actFaultStore, cancel context.CancelFunc) {
				f.readErr, f.afterAct = context.Canceled, cancel
			}},
		} {
			reportID, method, path, body := fresh()
			before, _ := h.reportActState(reportID)
			faulty := &actFaultStore{Backend: base}
			ctx, cancel := context.WithCancel(context.Background())
			fault.install(faulty, cancel)
			h.server.Store = faulty
			req := httptest.NewRequest(method, "/v1/feedback-reports/"+reportID+path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+h.manager())
			req.Header.Set("X-Workspace-ID", "alpha")
			response := h.serve(req)
			cancel()
			h.server.Store = base
			committed(fault.name, reportID, before, response)
		}

		// The connection is gone when the answer is written: the act is
		// recorded, and the linking job is still asked for the pending report.
		{
			reportID, method, path, body := fresh()
			before, _ := h.reportActState(reportID)
			asked := []string{}
			h.server.feedbackLinkingAsked = func(_ string, id string) { asked = append(asked, id) }
			req := httptest.NewRequest(method, "/v1/feedback-reports/"+reportID+path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+h.manager())
			req.Header.Set("X-Workspace-ID", "alpha")
			writer := &failingWriter{header: http.Header{}}
			h.handler.ServeHTTP(writer, req)
			h.server.feedbackLinkingAsked = nil
			if writer.status != http.StatusOK {
				t.Fatalf("%s, the connection is gone: status=%d, want 200", route.name, writer.status)
			}
			if after, _ := h.reportActState(reportID); after == before {
				t.Fatalf("%s, the connection is gone: the store holds no act", route.name)
			}
			if !slices.Contains(asked, reportID) {
				t.Fatalf("%s, the connection is gone: the linking job was not asked, asked=%v", route.name, asked)
			}
		}

		// A failure before the commit writes nothing; only a retryable one
		// asks for the request again.
		for _, fault := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{"the act answers retryable", fmt.Errorf("%w: lock wait", store.ErrRetryable), http.StatusServiceUnavailable, "retryable"},
			{"the act fails", errors.New("write failed"), http.StatusInternalServerError, ""},
			{"the request is cancelled before the commit", context.Canceled, http.StatusInternalServerError, ""},
		} {
			reportID, method, path, body := fresh()
			before, _ := h.reportActState(reportID)
			h.server.Store = &actFaultStore{Backend: base, actErr: fault.err}
			response := request(reportID, method, path, body)
			h.server.Store = base
			if response.Code != fault.status {
				t.Fatalf("%s, %s: status=%d body=%s, want %d", route.name, fault.name, response.Code, response.Body, fault.status)
			}
			if fault.code != "" {
				requireJSONError(t, response, fault.status, fault.code)
			} else if strings.Contains(response.Body.String(), "retryable") {
				t.Fatalf("%s, %s: a failure that is not retryable asks for the request again: %s", route.name, fault.name, response.Body)
			}
			if after, _ := h.reportActState(reportID); after != before {
				t.Fatalf("%s, %s: a refused write recorded something", route.name, fault.name)
			}
		}

		// The same request again, after the act is recorded, changes nothing
		// or is refused, and its answer comes from the act alone while every
		// read fails.
		{
			reportID, method, path, body := fresh()
			if response := request(reportID, method, path, body); response.Code != http.StatusOK {
				t.Fatalf("%s: first act status=%d body=%s", route.name, response.Code, response.Body)
			}
			h.settle()
			before, _ := h.reportActState(reportID)
			h.server.Store = &actFaultStore{Backend: base, readErr: fmt.Errorf("%w: report view moved", store.ErrRetryable)}
			response := request(reportID, method, path, body)
			h.server.Store = base
			switch {
			case route.name == "close":
				requireJSONError(t, response, http.StatusConflict, "report_closed")
			case route.name == "fix task":
				// The same fix task again changes nothing.
				if response.Code != http.StatusOK {
					t.Fatalf("%s repeated: status=%d body=%s", route.name, response.Code, response.Body)
				}
			default:
				if response.Code != http.StatusOK {
					t.Fatalf("%s repeated: status=%d body=%s, want 200 for an act that changes nothing", route.name, response.Code, response.Body)
				}
			}
			if after, _ := h.reportActState(reportID); after != before {
				t.Fatalf("%s repeated: recorded something", route.name)
			}
		}
	}

	// A refusal answers its refusal while every read fails.
	reportID := h.storeReport("refused", nil).ID
	h.server.Store = &actFaultStore{Backend: base, readErr: fmt.Errorf("%w: report view moved", store.ErrRetryable)}
	requireJSONError(t, h.do(h.manager(), http.MethodPost, "/v1/feedback-reports/"+reportID+"/links", `{"task_id":"task-missing"}`), http.StatusNotFound, "task_not_found")
	requireJSONError(t, h.do(h.manager(), http.MethodDelete, "/v1/feedback-reports/"+reportID+"/links/lnk:missing", ""), http.StatusNotFound, "link_not_found")
	h.server.Store = base
}
