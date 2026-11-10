package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Tests for POST /v1/tasks/{id}/approvals and
// POST /v1/tasks/{id}/approvals/waive-author (req-risk-based-review AC-8.3,
// AC-8.5, AC-8.7, AC-11.2; feature-risk-based-review RR-7;
// component-http-api invariant 17).

const approvalHead = "approval-head"

type approvalHTTP struct {
	handler http.Handler
	st      store.Store
	ctx     context.Context
	forge   *githubtrigger.PullRequest
	cfg     *config.Config
}

// trust sets the repository's live trusted_authors list.
func (f approvalHTTP) trust(userIDs ...string) { f.cfg.Repos[0].TrustedAuthors = userIDs }

// approvalHTTPFixture serves workspace "alpha" with an enforcing high-tier
// task "risk-approve" waiting at the merge gate at approvalHead in review
// round 1, authored by "author", and a record-mode task "risk-record" at the
// gate. operator, second, and author hold operate_gates; contributor does
// not.
func approvalHTTPFixture(t *testing.T) approvalHTTP {
	t.Helper()
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator":    {"alpha": core.WorkspaceRoleOperator},
			"second":      {"alpha": core.WorkspaceRoleOperator},
			"author":      {"alpha": core.WorkspaceRoleOperator},
			"contributor": {"alpha": core.WorkspaceRoleContributor},
		},
	}
	st := store.WithCapabilityAuthorizer(store.NewMemory(), fixture)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	for id, mode := range map[string]riskpolicy.Mode{"risk-approve": riskpolicy.ModeEnforce, "risk-record": riskpolicy.ModeRecord} {
		task := core.Task{ID: id, Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview,
			PolicyVersion: 1, ReviewedHeadSHA: approvalHead, CreatedAt: time.Now().UTC()}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
		requireOKHTTP(t, st.CreateTask(ctx, task))
		if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: id, Key: core.PathRiskKey(approvalHead), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierHigh,
			HeadSHA: approvalHead, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "path rule"}); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		jobID := id + "-review-1-seat-1"
		requireOKHTTP(t, st.CreateJob(ctx, core.Job{ID: jobID, TaskID: id, Stage: core.StageReview, State: core.JobDone}))
		requireOKHTTP(t, storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: jobID, JobID: jobID, TaskID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: approvalHead}))
		claim := core.WorkOrder{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID("author")}
		requireOKHTTP(t, st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "work_order.claimed", Payload: store.WorkOrderEventPayload(claim)}))
	}
	cfg := &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "conveyor", GitHub: "acme/conveyor"}}}
	pr := &githubtrigger.PullRequest{Number: 9, State: "open", Mergeable: "MERGEABLE", HeadSHA: approvalHead}
	d := dispatch.New(st, cfg, nil)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) { return *pr, nil }
	d.RequestMerge = func(context.Context, string, int) error { t.Fatal("an approval requested a merge"); return nil }
	server := NewServer(st)
	server.Workspaces, server.Memberships = fixture, fixture
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	server.OnRecordApproval = d.RecordApproval
	server.OnWaiveAuthorApproval = d.WaiveAuthorApproval
	server.Credentials = staticCredentialVerifier{
		"operator-token":    {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"second-token":      {ID: "pat_second", OwnerUserID: "second", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"author-token":      {ID: "pat_author", OwnerUserID: "author", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"contributor-token": {ID: "pat_contributor", OwnerUserID: "contributor", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	return approvalHTTP{handler: server.Handler(), st: st, ctx: ctx, forge: pr, cfg: cfg}
}

func (f approvalHTTP) post(token, taskID, route, body string) *routeResult {
	response := riskRequest(f.handler, http.MethodPost, token, "/v1/tasks/"+taskID+route+"?workspace_id=alpha", body)
	return &routeResult{Code: response.Code, Body: response.Body.String()}
}

type routeResult struct {
	Code int
	Body string
}

func (f approvalHTTP) records(t *testing.T, taskID string) []core.TaskApproval {
	t.Helper()
	records, err := f.st.ListTaskApprovals(f.ctx, taskID)
	requireOKHTTP(t, err)
	return records
}

// Two people are needed: the first approval answers 202 with the shortfall,
// a repeat counts once, and the trusted author's approval answers 200 with
// every requirement met. The
// REST read carries the same count. No approval merges.
func TestApprovalsRouteCountsDistinctPeople(t *testing.T) {
	f := approvalHTTPFixture(t)
	body := `{"head_sha":"` + approvalHead + `"}`
	first := f.post("operator-token", "risk-approve", "/approvals", body)
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body, `"approvals_required"`) || !strings.Contains(first.Body, `"recorded":true`) {
		t.Fatalf("first approval status=%d body=%s", first.Code, first.Body)
	}
	if again := f.post("operator-token", "risk-approve", "/approvals", body); again.Code != http.StatusAccepted || !strings.Contains(again.Body, `"recorded":false`) {
		t.Fatalf("repeat approval status=%d body=%s", again.Code, again.Body)
	}
	if n := len(f.records(t, "risk-approve")); n != 1 {
		t.Fatalf("records=%d, want 1", n)
	}
	var read struct {
		Risk core.TaskRisk `json:"risk"`
	}
	response := riskRequest(f.handler, http.MethodGet, "operator-token", "/v1/tasks/risk-approve?workspace_id=alpha", "")
	requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &read))
	if read.Risk.RecordedApprovals != 1 || read.Risk.RequiredHumanApprovals != 2 || read.Risk.RemainingApprovals != 1 || read.Risk.ApprovalHead != approvalHead {
		t.Fatalf("REST risk=%+v", read.Risk)
	}
	// The trusted author's approval is the second person and meets the
	// author condition (AC-8.4).
	second := f.post("author-token", "risk-approve", "/approvals", body)
	if second.Code != http.StatusOK || !strings.Contains(second.Body, `"approvals_satisfied":true`) {
		t.Fatalf("second approval status=%d body=%s", second.Code, second.Body)
	}
	if current, _ := f.st.GetTask(f.ctx, "risk-approve"); current.State != core.TaskAwaiting {
		t.Fatalf("approvals changed the task state to %s", current.State)
	}
}

// Every malformed body is 400 and records nothing; an oversized body is 413
// before any read or write; a stale head, a record-mode task, and a person
// without operate_gates record nothing.
func TestApprovalsRouteRefusals(t *testing.T) {
	f := approvalHTTPFixture(t)
	for name, body := range map[string]string{
		"NotJSON":       `head`,
		"Array":         `["` + approvalHead + `"]`,
		"NumberHead":    `{"head_sha":7}`,
		"NullHead":      `{"head_sha":null}`,
		"MissingHead":   `{}`,
		"BlankHead":     `{"head_sha":" "}`,
		"UnknownField":  `{"head_sha":"` + approvalHead + `","count":2}`,
		"ReasonOnPlain": `{"head_sha":"` + approvalHead + `","reason":"x"}`,
		"Trailing":      `{"head_sha":"` + approvalHead + `"}{}`,
	} {
		if response := f.post("operator-token", "risk-approve", "/approvals", body); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body)
		}
	}
	for name, body := range map[string]string{
		"BlankReason":     `{"head_sha":"` + approvalHead + `","reason":"  "}`,
		"MultiLineReason": `{"head_sha":"` + approvalHead + `","reason":"one\ntwo"}`,
		"NumberReason":    `{"head_sha":"` + approvalHead + `","reason":3}`,
		"MissingReason":   `{"head_sha":"` + approvalHead + `"}`,
	} {
		if response := f.post("operator-token", "risk-approve", "/approvals/waive-author", body); response.Code != http.StatusBadRequest {
			t.Fatalf("waiver %s: status=%d body=%s", name, response.Code, response.Body)
		}
	}
	oversized := `{"head_sha":"` + approvalHead + `","reason":"` + strings.Repeat("a", approvalBodyLimit) + `"}`
	for _, route := range []string{"/approvals", "/approvals/waive-author"} {
		if response := f.post("operator-token", "risk-approve", route, oversized); response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized %s: status=%d", route, response.Code)
		}
	}
	if response := f.post("operator-token", "risk-approve", "/approvals", `{"head_sha":"old-head"}`); response.Code != http.StatusConflict {
		t.Fatalf("stale head status=%d body=%s", response.Code, response.Body)
	}
	if response := f.post("operator-token", "risk-record", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusConflict {
		t.Fatalf("record-mode status=%d body=%s", response.Code, response.Body)
	}
	// An untrusted author's approval does not count: 409 with nothing
	// recorded (AC-9.2).
	untrusted := approvalHTTPFixture(t)
	untrusted.trust("second")
	if response := untrusted.post("author-token", "risk-approve", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body, "approval_not_counted") {
		t.Fatalf("untrusted author status=%d body=%s", response.Code, response.Body)
	}
	if records := untrusted.records(t, "risk-approve"); len(records) != 0 {
		t.Fatalf("untrusted author recorded %+v", records)
	}
	if response := f.post("contributor-token", "risk-approve", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusNotFound && response.Code != http.StatusForbidden {
		t.Fatalf("contributor status=%d body=%s", response.Code, response.Body)
	}
	if response := f.post("", "risk-approve", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
	f.forge.Mergeable = "CONFLICTING"
	if response := f.post("operator-token", "risk-approve", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body, "CONFLICTING") {
		t.Fatalf("unready head status=%d body=%s", response.Code, response.Body)
	}
	for _, id := range []string{"risk-approve", "risk-record"} {
		if records := f.records(t, id); len(records) != 0 {
			t.Fatalf("%s refusals recorded %+v", id, records)
		}
	}
}

// The waiver is refused for an author, recorded once for a non-author with an
// audited event, and leaves the required number unchanged (AC-8.5).
func TestWaiveAuthorRoute(t *testing.T) {
	f := approvalHTTPFixture(t)
	body := `{"head_sha":"` + approvalHead + `","reason":"the author is on leave"}`
	if response := f.post("author-token", "risk-approve", "/approvals/waive-author", body); response.Code != http.StatusConflict {
		t.Fatalf("author waiver status=%d body=%s", response.Code, response.Body)
	}
	response := f.post("operator-token", "risk-approve", "/approvals/waive-author", body)
	if response.Code != http.StatusOK {
		t.Fatalf("waiver status=%d body=%s", response.Code, response.Body)
	}
	var result struct {
		Risk core.TaskRisk `json:"risk"`
	}
	requireOKHTTP(t, json.Unmarshal([]byte(response.Body), &result))
	if result.Risk.AuthorApprovalNeeded || result.Risk.RequiredHumanApprovals != 2 || result.Risk.RecordedApprovals != 0 {
		t.Fatalf("waived risk=%+v", result.Risk)
	}
	events, err := f.st.ListEvents(f.ctx, "risk-approve")
	requireOKHTTP(t, err)
	// The waiver is the act's last event before the policy status
	// re-evaluation it triggers (req-risk-based-review AC-10.1).
	last := events[len(events)-1]
	for i := len(events) - 1; i >= 0 && strings.HasPrefix(events[i].Kind, "review_policy_status."); i-- {
		last = events[max(i-1, 0)]
	}
	if last.Kind != core.TaskAuthorWaiverEvent || last.ActorID != store.UserActorID("operator") || !strings.Contains(string(last.Payload), "the author is on leave") {
		t.Fatalf("last event=%+v", last)
	}
}

// The merge gate's Approve act on an enforcing task is the person's counted
// approval: short of the count it answers 202 and leaves the gate pending,
// so Approve only never passes the count (AC-8.7).
func TestMergeGateApproveRecordsTheCountedApproval(t *testing.T) {
	f := approvalHTTPFixture(t)
	response := riskRequest(f.handler, http.MethodPost, "operator-token", "/v1/tasks/risk-approve/review?workspace_id=alpha", `{"action":"approve","reason_code":"approved"}`)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"approvals_required"`) {
		t.Fatalf("approve status=%d body=%s", response.Code, response.Body)
	}
	if current, _ := f.st.GetTask(f.ctx, "risk-approve"); current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval {
		t.Fatalf("approve moved the task to %s/%s", current.State, current.GateKind)
	}
	if items, _ := f.st.ListInterventions(f.ctx, "risk-approve"); len(items) != 0 {
		t.Fatalf("approve recorded interventions %+v", items)
	}
	if records := f.records(t, "risk-approve"); len(records) != 1 || records[0].UserID != "operator" {
		t.Fatalf("records=%+v", records)
	}
}

// A merge short of the required approvals answers 202 with the shortfall
// and whether the person's approval was recorded; nothing merged (AC-8.7).
func TestMergeRouteAnswersAShortMergeWithTheShortfall(t *testing.T) {
	f := approvalHTTPFixture(t)
	server := NewServer(f.st)
	fixture := &membershipFixture{workspaces: []core.Workspace{{ID: "alpha"}}, roles: map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}}}
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	server.OnGateMerge = func(context.Context, core.Task, core.Actor) (core.Task, error) {
		return core.Task{}, &dispatch.RiskApprovalPending{Recorded: true}
	}
	response := riskRequest(server.Handler(), http.MethodPost, "operator-token", "/v1/tasks/risk-approve/merge?workspace_id=alpha", "")
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"recorded":true`) || !strings.Contains(response.Body.String(), `"merged":false`) {
		t.Fatalf("short merge status=%d body=%s", response.Code, response.Body)
	}
}

// The next action is stated for the signed-in person: after one approval of
// a high-tier task whose sole author is trusted, the person who approved is
// told the approval is already recorded, a second non-author is offered an
// approval that does not approve the merge, because the trusted author's
// approval is still needed, and the trusted author is offered the approval
// that completes the requirements (req-risk-based-review AC-8.7, AC-11.1).
func TestNextActionStatesTheCallersOwnApproval(t *testing.T) {
	f := approvalHTTPFixture(t)
	if response := f.post("operator-token", "risk-approve", "/approvals", `{"head_sha":"`+approvalHead+`"}`); response.Code != http.StatusAccepted {
		t.Fatalf("approval status=%d body=%s", response.Code, response.Body)
	}
	approveChoice := func(token string) (string, string) {
		t.Helper()
		response := riskRequest(f.handler, http.MethodGet, token, "/v1/tasks/risk-approve/next-action?workspace_id=alpha", "")
		if response.Code != http.StatusOK {
			t.Fatalf("next action status=%d body=%s", response.Code, response.Body)
		}
		var action struct {
			Choices []struct {
				ID     string `json:"id"`
				Effect string `json:"effect"`
			} `json:"choices"`
			Details struct {
				Refusals []struct {
					Choice string `json:"choice"`
					Text   string `json:"text"`
				} `json:"refusals"`
			} `json:"details"`
		}
		requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &action))
		for _, choice := range action.Choices {
			if choice.ID == "approve" {
				return choice.Effect, ""
			}
		}
		for _, refusal := range action.Details.Refusals {
			if refusal.Choice == "approve" {
				return "", refusal.Text
			}
		}
		t.Fatalf("no approve choice or refusal: %s", response.Body)
		return "", ""
	}
	if effect, refusal := approveChoice("operator-token"); effect != "" || !strings.HasPrefix(refusal, "Your approval of this head is already recorded.") {
		t.Fatalf("operator effect=%q refusal=%q", effect, refusal)
	}
	if effect, _ := approveChoice("second-token"); !strings.Contains(effect, "It does not approve the merge, because the merge still needs 1 more approval, including a trusted author's approval.") {
		t.Fatalf("second effect=%q", effect)
	}
	if effect, _ := approveChoice("author-token"); !strings.Contains(effect, "Your approval completes the required approvals.") {
		t.Fatalf("author effect=%q", effect)
	}
}
