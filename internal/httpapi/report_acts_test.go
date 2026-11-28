package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
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

	act := func(method, path, body string) map[string]any {
		t.Helper()
		response := h.do(h.manager(), method, base+path, body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d body=%s", method, path, response.Code, response.Body)
		}
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
		before, _ := h.reportActState(report.ID)
		requireJSONError(t, h.do(h.manager(), refused.method, base+refused.path, refused.body), http.StatusConflict, "report_closed")
		if after, _ := h.reportActState(report.ID); after != before {
			t.Fatalf("a refused %s %s recorded something", refused.method, refused.path)
		}
	}
	served = act(http.MethodPost, "/reopen", "")
	if served["state"] != store.FeedbackReportOpen || served["reopened_at"] == nil || served["closed"] != nil || len(served["links"].([]any)) != 3 {
		t.Fatalf("after reopening=%v", served)
	}
	unchanged(http.MethodPost, "/reopen", "{}")
	// No act changed a task, a work order, or a next action.
	if _, deliveryAfter := h.reportActState(report.ID); deliveryAfter != deliveryBefore {
		t.Fatalf("an act changed delivery:\nbefore %s\nafter  %s", deliveryBefore, deliveryAfter)
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
