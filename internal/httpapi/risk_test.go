package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// riskHTTPFixture serves workspace "alpha" tasks to an operator, a
// contributor, an operator of another workspace, and a retired agent
// credential: risk-http and risk-review freeze mode record, and risk-off
// freezes no risk policy. risk-review has a review seat claimed by the
// operator's session.
func riskHTTPFixture(t *testing.T) (http.Handler, store.Store) {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	for _, id := range []string{"risk-http", "risk-off", "risk-review"} {
		task := core.Task{ID: id, Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview,
			PolicyVersion: 1, ReviewedHeadSHA: "head-" + id, CreatedAt: time.Now().UTC()}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}
		if id != "risk-off" {
			policy := record
			task.SetupContract.Risk = &policy
		}
		requireOKHTTP(t, st.CreateTask(ctx, task))
	}
	now := time.Now().UTC()
	jobID := "risk-review-review-1-seat-1"
	requireOKHTTP(t, storetest.CreateReviewRound(ctx, st, "risk-review", []core.Job{{ID: jobID, TaskID: "risk-review", Stage: core.StageReview, State: core.JobPending}},
		[]core.WorkOrder{{ID: jobID, JobID: jobID, TaskID: "risk-review", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: "head", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}}))
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, jobID, core.WorkOrderClaim{SessionID: "risk-seat", ClientToken: "risk-token", ClaimantID: core.TaskRunClaimantID("operator"), OwnerUserID: "operator", Agent: "codex", Model: "reviewer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}, {ID: "beta"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator":    {"alpha": core.WorkspaceRoleOperator},
			"contributor": {"alpha": core.WorkspaceRoleContributor},
			"outsider":    {"beta": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = fixture, fixture
	server.WorkOrders = &workorder.Service{Store: st}
	server.Credentials = staticCredentialVerifier{
		"operator-token":    {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"contributor-token": {ID: "pat_contributor", OwnerUserID: "contributor", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"outsider-token":    {ID: "pat_outsider", OwnerUserID: "outsider", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-token":       {ID: "agt_operator", OwnerUserID: "operator", Kind: retiredAgentKind, Scope: core.CredentialScopeUser},
	}
	return server.Handler(), st
}

func riskRequest(handler http.Handler, method, token, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func postRiskTier(handler http.Handler, token, taskID, body string) *httptest.ResponseRecorder {
	return riskRequest(handler, http.MethodPost, token, "/v1/tasks/"+taskID+"/risk-tier?workspace_id=alpha", body)
}

func riskRatingCount(t *testing.T, st store.Store, taskID string) int {
	t.Helper()
	ratings, err := st.ListTaskRiskRatings(store.WithWorkspace(t.Context(), "alpha"), taskID)
	requireOKHTTP(t, err)
	return len(ratings)
}

// TestRaiseRiskTierHTTPAuthorization: only a user credential holding
// operate_gates in the task's workspace may raise a tier, and the raise
// records the authenticated person, the old and new tier, and the reason
// (req-risk-based-review REQ-6, AC-6.1; feature-risk-based-review RR-8).
func TestRaiseRiskTierHTTPAuthorization(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	body := `{"tier":"medium","reason":"touches billing"}`
	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"Unauthenticated", "", http.StatusUnauthorized},
		{"AgentCredential", "agent-token", http.StatusUnauthorized},
		{"OtherWorkspaceOperator", "outsider-token", http.StatusNotFound},
		{"ContributorWithoutOperateGates", "contributor-token", http.StatusNotFound},
	} {
		if response := postRiskTier(handler, tc.token, "risk-http", body); response.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body)
		}
	}
	if count := riskRatingCount(t, st, "risk-http"); count != 0 {
		t.Fatalf("refused callers recorded %d ratings", count)
	}
	response := postRiskTier(handler, "operator-token", "risk-http", body)
	if response.Code != http.StatusOK {
		t.Fatalf("operator status=%d body=%s", response.Code, response.Body)
	}
	var result struct {
		TaskID string        `json:"task_id"`
		Risk   core.TaskRisk `json:"risk"`
	}
	requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &result))
	if result.TaskID != "risk-http" || result.Risk.Tier != riskpolicy.TierMedium || result.Risk.Source != core.RiskSourcePerson || result.Risk.Reason != "touches billing" || len(result.Risk.Ratings) != 1 {
		t.Fatalf("result=%+v", result)
	}
	rating := result.Risk.Ratings[0]
	if rating.ActorID != store.UserActorID("operator") || rating.PreviousTier != riskpolicy.TierLow || rating.HeadSHA != "head-risk-http" {
		t.Fatalf("raise rating=%+v", rating)
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "alpha"), "risk-http")
	requireOKHTTP(t, err)
	last := events[len(events)-1]
	if last.Kind != core.RiskRatedEvent || last.ActorID != store.UserActorID("operator") || last.ActorRole != core.ActorUser {
		t.Fatalf("last event=%+v", last)
	}
	var payload map[string]any
	requireOKHTTP(t, json.Unmarshal(last.Payload, &payload))
	if payload["previous_tier"] != "low" || payload["new_tier"] != "medium" || payload["reason"] != "touches billing" || payload["actor_id"] != store.UserActorID("operator") {
		t.Fatalf("raise event payload=%v", payload)
	}
}

// TestRaiseRiskTierHTTPStrictBody: every malformed body is 400
// invalid_risk_tier and records nothing, so no value is truncated, coerced,
// or read as absent.
func TestRaiseRiskTierHTTPStrictBody(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	for _, body := range []string{
		``, `[]`, `null`, `"high"`, `{`,
		`{"tier":"high"}`,
		`{"reason":"no tier"}`,
		`{"tier":"high","reason":""}`,
		`{"tier":"high","reason":"   "}`,
		`{"tier":"high","reason":"two\nlines"}`,
		`{"tier":"high","reason":"carriage\rreturn"}`,
		`{"tier":"critical","reason":"unknown tier"}`,
		`{"tier":"HIGH","reason":"wrong case"}`,
		`{"tier":3,"reason":"number tier"}`,
		`{"tier":2.5,"reason":"fraction tier"}`,
		`{"tier":null,"reason":"null tier"}`,
		`{"tier":true,"reason":"boolean tier"}`,
		`{"tier":["high"],"reason":"list tier"}`,
		`{"tier":"high","reason":null}`,
		`{"tier":"high","reason":7}`,
		`{"tier":"high","reason":"ok","actor":"someone else"}`,
		`{"tier":"high","reason":"ok"} {"tier":"high"}`,
		`{"tier":"high","reason":"` + strings.Repeat("x", core.MaxRiskReasonRunes+1) + `"}`,
	} {
		response := postRiskTier(handler, "operator-token", "risk-http", body)
		if response.Code != http.StatusBadRequest || seatErrorCode(t, response) != "invalid_risk_tier" {
			t.Fatalf("body %q: status=%d body=%s", body, response.Code, response.Body)
		}
	}
	if count := riskRatingCount(t, st, "risk-http"); count != 0 {
		t.Fatalf("malformed bodies recorded %d ratings", count)
	}
}

// TestRaiseRiskTierHTTPRefusesAtOrBelow: a request at or below the current
// tier is refused, names the current tier, and changes nothing; an off task
// is never rated (req-risk-based-review AC-6.2, AC-1.1).
func TestRaiseRiskTierHTTPRefusesAtOrBelow(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	ctx := store.WithWorkspace(t.Context(), "alpha")
	if response := postRiskTier(handler, "operator-token", "risk-http", `{"tier":"low","reason":"unrated is low"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"current_tier":"low"`) {
		t.Fatalf("raise to low on an unrated task: status=%d body=%s", response.Code, response.Body)
	}
	if response := postRiskTier(handler, "operator-token", "risk-http", `{"tier":"high","reason":"payments"}`); response.Code != http.StatusOK {
		t.Fatalf("raise to high: status=%d body=%s", response.Code, response.Body)
	}
	before, err := st.GetTask(ctx, "risk-http")
	requireOKHTTP(t, err)
	eventsBefore, err := st.ListEvents(ctx, "risk-http")
	requireOKHTTP(t, err)
	for _, tier := range []string{"high", "medium", "low"} {
		response := postRiskTier(handler, "operator-token", "risk-http", `{"tier":"`+tier+`","reason":"lower"}`)
		if response.Code != http.StatusConflict || seatErrorCode(t, response) != "risk_tier_not_raised" || !strings.Contains(response.Body.String(), `"current_tier":"high"`) {
			t.Fatalf("raise to %s: status=%d body=%s", tier, response.Code, response.Body)
		}
	}
	after, err := st.GetTask(ctx, "risk-http")
	requireOKHTTP(t, err)
	eventsAfter, err := st.ListEvents(ctx, "risk-http")
	requireOKHTTP(t, err)
	if len(eventsAfter) != len(eventsBefore) || riskRatingCount(t, st, "risk-http") != 1 || after.State != before.State || after.NextStage != before.NextStage || after.PlanState != before.PlanState {
		t.Fatalf("refused raises changed the task: events %d->%d", len(eventsBefore), len(eventsAfter))
	}
	if response := postRiskTier(handler, "operator-token", "risk-off", `{"tier":"high","reason":"off task"}`); response.Code != http.StatusConflict || seatErrorCode(t, response) != "risk_not_enabled" {
		t.Fatalf("off task raise: status=%d body=%s", response.Code, response.Body)
	}
	if count := riskRatingCount(t, st, "risk-off"); count != 0 {
		t.Fatalf("off task recorded %d ratings", count)
	}
}

// TestTaskReadsShareRisk: the REST task read, the task list, the task detail
// read, and MCP get_task return the same risk field, and an off task has
// none (req-risk-based-review REQ-11, AC-11.2).
func TestTaskReadsShareRisk(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: "risk-http", Key: core.PathRiskKey("head-risk-http"), Source: core.RiskSourcePathRule,
		Tier: riskpolicy.TierMedium, HeadSHA: "head-risk-http", Reason: "matched path rules: GitHub Actions workflows (medium)", HumanApprovals: 1}); err != nil {
		t.Fatal(err)
	}
	type riskRead struct {
		Risk *core.TaskRisk `json:"risk"`
	}
	read := func(path string) riskRead {
		t.Helper()
		response := riskRequest(handler, http.MethodGet, "operator-token", path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body)
		}
		var value riskRead
		requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &value))
		return value
	}
	rest := read("/v1/tasks/risk-http?workspace_id=alpha")
	if rest.Risk == nil || rest.Risk.Tier != riskpolicy.TierMedium || rest.Risk.Source != core.RiskSourcePathRule || rest.Risk.Head != "head-risk-http" ||
		rest.Risk.Mode != riskpolicy.ModeRecord || rest.Risk.RequiredAgentReviews != 1 || rest.Risk.RequiredHumanApprovals != 0 ||
		rest.Risk.CalculatedHumanApprovals != 1 || rest.Risk.RecordedApprovals != 0 || rest.Risk.AuthorApprovalNeeded {
		t.Fatalf("REST risk=%+v", rest.Risk)
	}
	if off := read("/v1/tasks/risk-off?workspace_id=alpha"); off.Risk != nil {
		t.Fatalf("off task read carries risk %+v", off.Risk)
	}
	detail := read("/v1/tasks/risk-http/activity?workspace_id=alpha")
	if detail.Risk == nil || !sameRisk(*detail.Risk, *rest.Risk) {
		t.Fatalf("detail risk=%+v, REST risk=%+v", detail.Risk, rest.Risk)
	}
	unrated := read("/v1/tasks/risk-review?workspace_id=alpha")
	if unrated.Risk == nil || unrated.Risk.Tier != "" || unrated.Risk.Source != "" || len(unrated.Risk.Ratings) != 0 {
		t.Fatalf("an unrated task invented a rating: %+v", unrated.Risk)
	}
	list := riskRequest(handler, http.MethodGet, "operator-token", "/v1/tasks?workspace_id=alpha", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body)
	}
	var rows []struct {
		ID   string         `json:"id"`
		Risk *core.TaskRisk `json:"risk"`
	}
	requireOKHTTP(t, json.Unmarshal(list.Body.Bytes(), &rows))
	seen := 0
	for _, row := range rows {
		switch row.ID {
		case "risk-http":
			seen++
			if row.Risk == nil || !sameRisk(*row.Risk, *rest.Risk) {
				t.Fatalf("list risk=%+v", row.Risk)
			}
		case "risk-off":
			seen++
			if row.Risk != nil {
				t.Fatalf("off list row carries risk %+v", row.Risk)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("list rows=%s", list.Body)
	}
	mcp := mcpToolText(t, handler, "operator-token", "get_task", map[string]any{"workspace_id": "alpha", "task_id": "risk-http"})
	var items struct {
		Items []riskRead `json:"items"`
	}
	requireOKHTTP(t, json.Unmarshal([]byte(mcp), &items))
	if len(items.Items) != 1 || items.Items[0].Risk == nil || !sameRisk(*items.Items[0].Risk, *rest.Risk) {
		t.Fatalf("MCP get_task=%s", mcp)
	}
	offMCP := mcpToolText(t, handler, "operator-token", "get_task", map[string]any{"workspace_id": "alpha", "task_id": "risk-off"})
	if strings.Contains(offMCP, `"risk"`) {
		t.Fatalf("MCP get_task of an off task carries risk: %s", offMCP)
	}
}

func sameRisk(a, b core.TaskRisk) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// mcpToolText calls one MCP tool and returns its text result, failing on a
// protocol error.
func mcpToolText(t *testing.T, handler http.Handler, token, tool string, args map[string]any) string {
	t.Helper()
	text, isError := mcpToolCall(t, handler, token, tool, args)
	if isError {
		t.Fatalf("%s error: %s", tool, text)
	}
	return text
}

func mcpToolCall(t *testing.T, handler http.Handler, token, tool string, args map[string]any) (string, bool) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	requireOKHTTP(t, err)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", tool, response.Code, response.Body)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	if envelope.Error != nil {
		return envelope.Error.Message, true
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatalf("%s returned no content: %s", tool, response.Body)
	}
	return envelope.Result.Content[0].Text, envelope.Result.IsError
}

// TestSubmitReviewVerdictRiskArgumentTypes: a risk_tier or risk_reason of any
// JSON type but string, null included, is refused before the verdict, and
// the seat stays claimed (req-risk-based-review AC-5.2).
func TestSubmitReviewVerdictRiskArgumentTypes(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	ctx := store.WithWorkspace(t.Context(), "alpha")
	base := func() map[string]any {
		return map[string]any{"workspace_id": "alpha", "work_order_id": "risk-review-review-1-seat-1", "session_id": "risk-seat",
			"verdict": "approve", "reason_code": "verified", "summary": "risk argument types",
			"requirement_citations":  map[string]any{"applicable": false, "cited_ids": []string{}, "unknown_ids": []string{}, "unserved_ids": []string{}, "conflicts": []string{}},
			"done_criteria_coverage": map[string]any{"applicable": false, "satisfied": []string{}, "unsatisfied": []string{}, "unverified": []string{}, "conflicts": []string{}},
			"governance_assessment":  map[string]any{"design_applicable": false, "decision_citable": false, "cited_ids": []string{}, "unknown_ids": []string{}, "ungoverned_ids": []string{}, "superseded_ids": []string{}, "conflicts": []string{}}}
	}
	for _, tc := range []struct {
		name  string
		field string
		value any
		want  string
	}{
		{"NullTier", "risk_tier", nil, "risk_tier must be a string"},
		{"NumberTier", "risk_tier", 2, "risk_tier must be a string"},
		{"FractionTier", "risk_tier", 1.5, "risk_tier must be a string"},
		{"BooleanTier", "risk_tier", true, "risk_tier must be a string"},
		{"ListTier", "risk_tier", []string{"low"}, "risk_tier must be a string"},
		{"NullReason", "risk_reason", nil, "risk_reason must be a string"},
		{"NumberReason", "risk_reason", 7, "risk_reason must be a string"},
		{"UnknownTier", "risk_tier", "critical", "risk_tier must be low, medium, or high"},
		{"BlankReason", "risk_reason", " ", "risk_reason must be a non-blank line"},
		{"MultiLineReason", "risk_reason", "one\ntwo", "risk_reason must be one line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := base()
			args["risk_tier"], args["risk_reason"] = "low", "small change"
			args[tc.field] = tc.value
			text, isError := mcpToolCall(t, handler, "operator-token", "submit_review_verdict", args)
			if !isError || !strings.Contains(text, tc.want) {
				t.Fatalf("submit_review_verdict with %s=%v: error=%t text=%s", tc.field, tc.value, isError, text)
			}
			order, err := st.GetWorkOrder(ctx, "risk-review-review-1-seat-1")
			requireOKHTTP(t, err)
			if order.State != core.WorkOrderClaimed || order.SessionID != "risk-seat" || riskRatingCount(t, st, "risk-review") != 0 {
				t.Fatalf("refused verdict changed the seat: %+v", order)
			}
		})
	}
}

// TestTaskDetailSummariesCarrySeatRisk: the bounded detail's review results
// carry each seat's stated tier and reason for the review card, and a verdict
// without one carries none (req-risk-based-review AC-5.3, REQ-11).
func TestTaskDetailSummariesCarrySeatRisk(t *testing.T) {
	at := time.Now().UTC()
	events := []core.Event{
		{ID: 1, TaskID: "risk-summary", JobID: "seat-1", Kind: "review.completed", At: at, Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": "seat-1", "verdict": "approve", "summary": "ok", "risk_tier": "medium", "risk_reason": "new store write"})},
		{ID: 2, TaskID: "risk-summary", JobID: "seat-2", Kind: "review.completed", At: at, Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": "seat-2", "verdict": "approve", "summary": "ok"})},
	}
	summaries := deriveTaskDetailSummaries(events, nil, nil)
	first, second := summaries.ReviewResults.ByOrder["seat-1"], summaries.ReviewResults.ByOrder["seat-2"]
	if first.RiskTier != "medium" || first.RiskReason != "new store write" || second.RiskTier != "" || second.RiskReason != "" {
		t.Fatalf("seat results=%+v %+v", first, second)
	}
	encoded, err := json.Marshal(second)
	requireOKHTTP(t, err)
	if strings.Contains(string(encoded), "risk_") {
		t.Fatalf("a verdict without a tier serialized risk fields: %s", encoded)
	}
}

// TestRaiseRiskTierHTTPBodyBound: the route reads its whole body through the
// 64 KiB bound before decoding. A body over the bound answers 413 whether it
// pads a valid object with whitespace, trails more JSON after it, is cut
// inside the object, or declares no length, and no rating or event is
// written; a valid body of exactly the bound is accepted (component-http-api
// invariant 17; DEC-105(3)).
func TestRaiseRiskTierHTTPBodyBound(t *testing.T) {
	valid := `{"tier":"high","reason":"raise"}`
	pad := func(body string, size int) string { return body + strings.Repeat(" ", size-len(body)) }
	for _, tc := range []struct {
		name          string
		body          string
		unknownLength bool
	}{
		{"PaddedToLimitThenTrailingObject", pad(valid, riskTierBodyLimit) + "{}", false},
		{"PaddedToLimitThenTrailingWhitespace", pad(valid, riskTierBodyLimit) + "   ", false},
		{"PaddedPastLimitUnknownLength", pad(valid, riskTierBodyLimit) + "{}", true},
		{"ObjectCutByLimit", `{"tier":"high","reason":"` + strings.Repeat("x", riskTierBodyLimit) + `"}`, false},
		{"ObjectCutByLimitUnknownLength", `{"tier":"high","reason":"` + strings.Repeat("x", riskTierBodyLimit) + `"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, st := riskHTTPFixture(t)
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/risk-http/risk-tier?workspace_id=alpha", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer operator-token")
			if tc.unknownLength {
				request.ContentLength = -1
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusRequestEntityTooLarge || seatErrorCode(t, response) != "body_too_large" {
				t.Fatalf("status=%d body=%.200s", response.Code, response.Body)
			}
			if count := riskRatingCount(t, st, "risk-http"); count != 0 {
				t.Fatalf("an oversized body recorded %d ratings", count)
			}
			events, err := st.ListEvents(store.WithWorkspace(t.Context(), "alpha"), "risk-http")
			requireOKHTTP(t, err)
			for _, event := range events {
				if event.Kind == core.RiskRatedEvent {
					t.Fatalf("an oversized body appended %+v", event)
				}
			}
		})
	}
	t.Run("ExactlyAtLimitIsAccepted", func(t *testing.T) {
		handler, st := riskHTTPFixture(t)
		response := postRiskTier(handler, "operator-token", "risk-http", pad(valid, riskTierBodyLimit))
		if response.Code != http.StatusOK || riskRatingCount(t, st, "risk-http") != 1 {
			t.Fatalf("status=%d body=%.200s", response.Code, response.Body)
		}
	})
}

// TestRaiseRiskTierHTTPWaitsForTheTaskLock: the raise runs under the task
// side-effect lock every merge act holds, so a merge holding it finishes
// under the tier it read before the raise records anything
// (feature-risk-based-review RR-6).
func TestRaiseRiskTierHTTPWaitsForTheTaskLock(t *testing.T) {
	handler, st := riskHTTPFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		_ = st.WithTaskSideEffectLock(ctx, "risk-http", func(context.Context) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	go func() {
		done <- postRiskTier(handler, "operator-token", "risk-http", `{"tier":"high","reason":"payments"}`)
	}()
	select {
	case response := <-done:
		t.Fatalf("the raise answered %d while a merge act held the task lock", response.Code)
	case <-time.After(150 * time.Millisecond):
	}
	if count := riskRatingCount(t, st, "risk-http"); count != 0 {
		t.Fatalf("the raise recorded %d ratings while the lock was held", count)
	}
	close(release)
	if response := <-done; response.Code != http.StatusOK {
		t.Fatalf("raise after the lock: status=%d body=%s", response.Code, response.Body)
	}
	if count := riskRatingCount(t, st, "risk-http"); count != 1 {
		t.Fatalf("ratings=%d, want 1", count)
	}
}
