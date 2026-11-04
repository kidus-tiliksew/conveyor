package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	// Stricter than the live corpus on purpose (req-design-delivery AC-1.1).
	deliveryDesignCount   = 37
	deliveryDecisionCount = 103
	deliveryResponseBound = 65536
	deliveryPlanMinBytes  = 12 << 10
	deliveryDesignBytes   = 30 << 10
	deliveryPlanSentinel  = "PLAN-BODY-SENTINEL"
	deliveryDoneSentinel  = "DONE-CRITERION-SENTINEL"
	deliveryBodySentinel  = "DESIGN-BODY-SENTINEL"
	deliveryRecordMarker  = "DECISION-RECORD-SENTINEL"
	deliveryOverSentinel  = "SUPERSEDED-STATEMENT-SENTINEL"
	deliveryStatementMark = "SERVED-STATEMENT-SENTINEL"
)

type deliveryFixture struct {
	server   *Server
	store    store.Store
	ctx      context.Context
	task     core.Task
	plan     string
	designs  []string
	decision []string
}

// Designs attached out of ID order, one reached by a submission's diff, one
// covered only by the review diff's paths, and the rest governing elsewhere.
const (
	deliveryAttachedSecond = "component-delivery-fixture-30"
	deliveryAttachedFirst  = "component-delivery-fixture-31"
	deliverySubmissionDiff = "component-delivery-fixture-20"
	deliveryReviewDiff     = "component-delivery-fixture-21"
)

func deliveryDesignID(index int) string {
	return fmt.Sprintf("component-delivery-fixture-%02d", index)
}

func newDeliveryFixture(t *testing.T, id string, configure ...func(*core.Task)) *deliveryFixture {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(withCredentialActor(t.Context(), core.AuthenticatedCredential{ID: "pat-delivery", OwnerUserID: "usr-delivery", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}), "demo")
	body := strings.Repeat("Deliver this task against the confirmed requirements and record the evidence the reviewer reads. ", 10)
	task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Title: "Work orders name governing documents", Body: body, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	for _, apply := range configure {
		apply(&task)
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	f := &deliveryFixture{store: st, ctx: ctx, task: task}
	for index := range deliveryDesignCount {
		designID := deliveryDesignID(index)
		paths := "    - nowhere/" + designID + "/**\n"
		switch designID {
		case deliverySubmissionDiff:
			paths = "    - internal/submitted/**\n"
		case deliveryReviewDiff:
			paths = "    - internal/reviewed/**\n"
		}
		title := fmt.Sprintf("Delivery fixture component number %02d", index)
		content := fmt.Sprintf("# %s\n\n%s-%s\n\n%s\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n%s```\n", title, deliveryBodySentinel, designID, strings.Repeat("Mechanism prose the session reads by section. ", deliveryDesignBytes/46), paths)
		design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: designID, Title: title, Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
			t.Fatal(err)
		}
		f.designs = append(f.designs, designID)
	}
	// The last long statement replaces a retired decision, so the fixture holds
	// deliveryDecisionCount confirmed statements over the summary bound.
	longStatement := func(index int) string {
		statement := fmt.Sprintf("Decision %03d\n\nkeeps every delivered list bounded across lines, ", index) + strings.Repeat("and the operator reads the whole record elsewhere ", 8)
		if index%10 == 0 {
			statement = fmt.Sprintf("Decision %03d は", index) + strings.Repeat("決定の本文をここに長く記す。", 40)
		}
		return statement
	}
	for index := range deliveryDecisionCount - 1 {
		decision, err := st.ProposeDecision(ctx, core.Decision{Statement: longStatement(index), Context: deliveryRecordMarker + " context", AlternativesRejected: deliveryRecordMarker + " alternatives", Origin: core.DecisionOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ConfirmDecision(ctx, decision.ID); err != nil {
			t.Fatal(err)
		}
		f.decision = append(f.decision, decision.ID)
	}
	superseded, err := st.ProposeDecision(ctx, core.Decision{Statement: deliveryOverSentinel, Context: "Retired.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, superseded.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := st.ProposeDecision(ctx, core.Decision{Statement: longStatement(deliveryDecisionCount - 1), Context: "Current.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator, Supersedes: superseded.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	f.decision = append(f.decision, replacement.ID)

	statements := make([]core.RequirementStatement, 0, 3)
	var yaml strings.Builder
	for index := range 3 {
		statement := fmt.Sprintf("%s %d says a work order shall carry the instructions of the session and name its governing documents.", deliveryStatementMark, index)
		criterion := fmt.Sprintf("When a session reads work order %d over MCP, the response shall carry the role instructions and no System Design body.", index)
		statements = append(statements, core.RequirementStatement{ID: fmt.Sprintf("REQ-%d", index+1), Statement: statement, AcceptanceCriteria: []core.AcceptanceCriterion{{ID: fmt.Sprintf("AC-%d.1", index+1), Statement: criterion}}})
		fmt.Fprintf(&yaml, "- id: REQ-%d\n  statement: %s\n  acceptance_criteria:\n    - id: AC-%d.1\n      statement: %s\n", index+1, statement, index+1, criterion)
	}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-delivery-fixture", Title: "Delivery fixture"}, core.RequirementVersion{Content: "# Delivery fixture\n\nProse beside the statements.\n\n```conveyor:requirements\n" + yaml.String() + "```", Statements: statements, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}, DesignIDs: []string{deliveryAttachedFirst}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{DesignIDs: []string{deliveryAttachedSecond}}}); err != nil {
		t.Fatal(err)
	}

	var plan strings.Builder
	plan.WriteString("## Approach\n" + deliveryPlanSentinel + " opens the approach.\n")
	for plan.Len() < deliveryPlanMinBytes-2048 {
		plan.WriteString("Keep the complete snapshot on the server and deliver one compact list of references to each session.\n")
	}
	plan.WriteString("\n## Files touched\n- internal/pack/pack.go\n\n## Ordering\n1. Project.\n\n## Risks\n- Size.\n\n## Done criteria\n")
	for index := 1; plan.Len() < deliveryPlanMinBytes; index++ {
		fmt.Fprintf(&plan, "%d. %s %d holds for the whole serialized response.\n", index, deliveryDoneSentinel, index)
	}
	f.plan = plan.String()
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: f.plan})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}

	// The exit's corpus: every design and decision the fixture stored counts.
	corpusBytes := 0
	for _, id := range f.designs {
		version, err := st.GetSystemDesignVersion(ctx, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		corpusBytes += len(version.Content)
	}
	if len(f.designs) != deliveryDesignCount || corpusBytes <= 1<<20 {
		t.Fatalf("fixture has %d designs totalling %d bytes", len(f.designs), corpusBytes)
	}
	for _, id := range f.decision {
		decision, err := st.GetDecision(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Status != core.DecisionConfirmed || utf8.RuneCountInString(decision.Statement) <= core.DecisionSummaryMaxRunes {
			t.Fatalf("decision %s is %s with a %d-rune statement", id, decision.Status, utf8.RuneCountInString(decision.Statement))
		}
	}
	if len(f.decision) < deliveryDecisionCount || len(f.plan) < deliveryPlanMinBytes {
		t.Fatalf("fixture has %d decisions and a %d-byte plan", len(f.decision), len(f.plan))
	}

	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Repos: []config.Repo{{Name: "conveyor", URL: "https://github.com/org/conveyor", GitHub: "org/conveyor", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"verify":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	f.server = NewServer(st)
	f.server.Workspace = "demo"
	f.server.WorkOrders = &workorder.Service{Store: st, Pack: bundle, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	return f
}

// order creates a queued order of stage for the fixture's task.
func (f *deliveryFixture) order(t *testing.T, stage core.Stage) string {
	t.Helper()
	job := core.Job{ID: f.task.ID + "-" + string(stage) + "-1", TaskID: f.task.ID, Stage: stage, State: core.JobPending}
	if err := f.store.CreateJob(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: f.task.ID, JobID: job.ID, Stage: stage}
	if stage != core.StageImplement {
		order.ReviewRound, order.ReviewSeat, order.BaselineSHA, order.HeadSHA = 1, 1, strings.Repeat("a", 40), strings.Repeat("b", 40)
	}
	if err := storetest.For(f.store).CreateWorkOrder(f.ctx, order); err != nil {
		t.Fatal(err)
	}
	return job.ID
}

// call posts one tools/call through the real handler and returns the whole
// JSON-RPC body and the tool result's text.
func (f *deliveryFixture) call(t *testing.T, tool string, arguments map[string]any) ([]byte, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	f.server.handleMCP(response, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload)).WithContext(f.ctx))
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s response: %v", tool, err)
	}
	if envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("%s result=%s", tool, response.Body.Bytes())
	}
	return response.Body.Bytes(), envelope.Result.Content[0].Text
}

func deliveryArgs(order, session string, extra map[string]any) map[string]any {
	arguments := map[string]any{"workspace_id": "demo", "work_order_id": order, "session_id": session}
	for key, value := range extra {
		arguments[key] = value
	}
	return arguments
}

// requireNoGovernanceBodies refuses any body or full-record sentinel anywhere
// in the serialized response (req-design-delivery AC-1.1).
func requireNoGovernanceBodies(t *testing.T, tool, text string) {
	t.Helper()
	for _, forbidden := range []string{deliveryBodySentinel, deliveryRecordMarker, deliveryOverSentinel, `"governance_snapshot":`, `"served_requirement_snapshot":`, `"alternatives_rejected"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s response carries %q", tool, forbidden)
		}
	}
}

// wireBytesWithoutDiff is the JSON-RPC body size with only the review diff
// removed from the tool result.
func wireBytesWithoutDiff(t *testing.T, text string) int {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "diff")
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: map[string]any{"content": []map[string]string{{"type": "text", "text": string(rebuilt)}}}})
	if err != nil {
		t.Fatal(err)
	}
	return len(body) + 1 // the encoder's trailing newline
}

var deliveryDecisionLine = regexp.MustCompile(`(?m)^- (DEC-\d+): (.*)$`)

// requireDecisionLines checks the confirmed list and each summary's bounds
// (req-design-delivery AC-1.3).
func requireDecisionLines(t *testing.T, role string, want []string) {
	t.Helper()
	got := map[string]string{}
	for _, match := range deliveryDecisionLine.FindAllStringSubmatch(role, -1) {
		if _, duplicate := got[match[1]]; duplicate {
			t.Fatalf("decision %s listed twice", match[1])
		}
		got[match[1]] = match[2]
		if utf8.RuneCountInString(match[2]) > core.DecisionSummaryMaxRunes || len(match[2]) > core.DecisionSummaryMaxBytes || !utf8.ValidString(match[2]) {
			t.Fatalf("%s summary out of bounds: %d runes, %d bytes", match[1], utf8.RuneCountInString(match[2]), len(match[2]))
		}
	}
	for _, id := range want {
		if got[id] == "" {
			t.Fatalf("confirmed decision %s missing from role", id)
		}
	}
	if long := got[want[1]]; !strings.HasSuffix(long, "…") {
		t.Fatalf("long statement lost its visible ellipsis: %q", long)
	}
}

// requireOrderedRoleGroups asserts each design appears once, at its version,
// under its group, with the groups in AC-1.2 order.
func requireOrderedRoleGroups(t *testing.T, role string, wantDiff []string) {
	t.Helper()
	attached := strings.Index(role, "## Attached to the task")
	diff := strings.Index(role, "## Covering the task's diff")
	repository := strings.Index(role, "## Governing the repository")
	if attached < 0 || repository < attached {
		t.Fatalf("group order attached=%d diff=%d repository=%d", attached, diff, repository)
	}
	if len(wantDiff) == 0 && diff >= 0 {
		t.Fatal("no-diff order has a diff group")
	}
	if len(wantDiff) > 0 && (diff < attached || repository < diff) {
		t.Fatalf("group order attached=%d diff=%d repository=%d", attached, diff, repository)
	}
	first, second := strings.Index(role, "- "+deliveryAttachedFirst+" v1:"), strings.Index(role, "- "+deliveryAttachedSecond+" v1:")
	if first < attached || second < first {
		t.Fatalf("attachment order lost: first=%d second=%d attached=%d", first, second, attached)
	}
	for _, id := range wantDiff {
		if at := strings.Index(role, "- "+id+" v1:"); at < diff || at > repository {
			t.Fatalf("%s at %d, want inside the diff group %d..%d", id, at, diff, repository)
		}
	}
	for index := range deliveryDesignCount {
		id := deliveryDesignID(index)
		if count := strings.Count(role, "- "+id+" v1: Delivery fixture component"); count != 1 {
			t.Fatalf("design %s listed %d times", id, count)
		}
	}
}

// TestWorkOrderDeliveryMCPResponses walks claim, renewal, get_work_order, and
// refresh over the real handler with a corpus stricter than the live one
// (req-design-delivery REQ-1; req-260802-72fc68 AC-2.2; req-task-centric-delivery AC-1.2).
func TestWorkOrderDeliveryMCPResponses(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, "delivery-implement")
	order := f.order(t, core.StageImplement)
	const session = "delivery-session"

	_, claimText := f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "delivery-token", "agent": "claude-code", "model": "claude-opus-5-5", "lease_seconds": 3600}))
	_, renewText := f.call(t, "renew_work_order", deliveryArgs(order, session, map[string]any{"lease_seconds": 3600}))
	contractBody, contractText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	_, refreshText := f.call(t, "refresh_work_order_context", deliveryArgs(order, session, nil))

	for tool, text := range map[string]string{"claim_work_order": claimText, "renew_work_order": renewText, "get_work_order": contractText, "refresh_work_order_context": refreshText} {
		requireNoGovernanceBodies(t, tool, text)
		if len(text) >= deliveryResponseBound {
			t.Fatalf("%s result is %d bytes", tool, len(text))
		}
	}
	t.Logf("get_work_order: %d-byte JSON-RPC body, %d-byte tool result", len(contractBody), len(contractText))
	if len(contractBody) >= deliveryResponseBound {
		t.Fatalf("get_work_order body is %d bytes, bound %d", len(contractBody), deliveryResponseBound)
	}

	// The compact responses that carry no role hold the list once at the root.
	for tool, text := range map[string]string{"claim_work_order": claimText, "renew_work_order": renewText, "refresh_work_order_context": refreshText} {
		var response struct {
			References *core.GovernanceReferences `json:"governance_references"`
		}
		if err := json.Unmarshal([]byte(text), &response); err != nil {
			t.Fatal(err)
		}
		if response.References == nil || len(response.References.Designs) != deliveryDesignCount {
			t.Fatalf("%s references=%+v", tool, response.References)
		}
		designs := response.References.Designs
		if designs[0].ID != deliveryAttachedFirst || designs[0].Group != core.GovernanceDesignAttached || designs[1].ID != deliveryAttachedSecond || designs[2].Group != core.GovernanceDesignRepository || designs[0].Version != 1 || designs[0].Title == "" {
			t.Fatalf("%s design order=%+v", tool, designs[:3])
		}
		confirmed := 0
		for _, decision := range response.References.Decisions {
			if decision.Status == core.DecisionConfirmed && decision.Summary != "" {
				confirmed++
			}
		}
		if confirmed != len(f.decision) {
			t.Fatalf("%s lists %d confirmed decisions, want %d", tool, confirmed, len(f.decision))
		}
		if strings.Count(text, deliveryAttachedFirst) != 1 {
			t.Fatalf("%s lists a design %d times", tool, strings.Count(text, deliveryAttachedFirst))
		}
		if strings.Contains(text, "role_prompt") {
			t.Fatalf("%s carries a role prompt", tool)
		}
	}

	var contract map[string]json.RawMessage
	if err := json.Unmarshal([]byte(contractText), &contract); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := json.Unmarshal(contract["role_prompt"], &role); err != nil {
		t.Fatal(err)
	}
	if _, listed := contract["governance_references"]; listed {
		t.Fatal("get_work_order carries a structured list beside its role prompt")
	}
	if _, listed := contract["served_requirements"]; listed {
		t.Fatal("get_work_order repeats the served requirements outside its role prompt")
	}
	shipped, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	shippedRole, err := shipped.Role(core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(role, strings.TrimSpace(shippedRole)) {
		t.Fatal("role prompt does not open with the shipped implement role")
	}
	requireOrderedRoleGroups(t, role, nil)
	requireDecisionLines(t, role, f.decision)

	// The whole plan, its done criteria, and each statement stay, once each.
	var spec core.SpecVersion
	if err := json.Unmarshal(contract["approved_spec"], &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Content != f.plan || len(f.plan) < deliveryPlanMinBytes {
		t.Fatalf("plan changed: %d bytes, want %d", len(spec.Content), len(f.plan))
	}
	if !strings.Contains(role, "`approved_spec.content`") {
		t.Fatal("role does not point at the plan's done criteria")
	}
	for sentinel, want := range map[string]int{deliveryPlanSentinel: 1, deliveryDoneSentinel + " 1 ": 1, deliveryStatementMark + " 0": 1, deliveryStatementMark + " 2": 1} {
		if got := strings.Count(contractText, sentinel); got != want {
			t.Fatalf("%q appears %d times, want %d", sentinel, got, want)
		}
	}
	if !strings.Contains(role, "When a session reads work order 2 over MCP") {
		t.Fatal("acceptance criterion missing from role")
	}

	// Stored authority is untouched by the projection, and the delivery write
	// still reads the complete snapshot.
	consulted, err := f.store.ListEvents(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range consulted {
		var payload struct {
			DocumentID  string `json:"document_id"`
			WorkOrderID string `json:"work_order_id"`
		}
		if event.Kind == "system_design.consulted" && json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID == order {
			seen[payload.DocumentID] = true
		}
	}
	if len(seen) != deliveryDesignCount {
		t.Fatalf("delivery consultation recorded %d designs, want %d", len(seen), deliveryDesignCount)
	}
}

// TestWorkOrderDeliveryReviewListsPinsAndOrdersTheDiff pins authority at claim,
// moves the live corpus, and proves the list and the freshness comparison each
// keep their meaning (req-design-delivery AC-1.2, AC-1.4, AC-6.1).
func TestWorkOrderDeliveryReviewListsPinsAndOrdersTheDiff(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, "delivery-review")
	if _, err := f.store.AttachSubmissionGovernance(f.ctx, f.task.ID, "conveyor", []string{"internal/submitted/a.go"}, store.SubmissionGovernanceAttribution{WorkOrderID: "delivery-implement", SessionID: "implement-session"}); err != nil {
		t.Fatal(err)
	}
	diff := "diff --git a/internal/reviewed/b.go b/internal/reviewed/b.go\n--- a/internal/reviewed/b.go\n+++ b/internal/reviewed/b.go\n@@ -1 +1 @@\n-old\n+" + strings.Repeat("new change line\n+", 8000) + "\n"
	f.server.WorkOrders.ReviewDiffBetween = func(context.Context, string, string, string) (string, error) { return diff, nil }
	f.server.WorkOrders.ReviewPRDescription = func(context.Context, string, string) (string, error) { return "Pull request description.", nil }
	order := f.order(t, core.StageReview)
	const session = "delivery-review-session"
	_, reviewClaimText := f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "delivery-review-token", "agent": "codex", "model": "gpt-6.1-sol", "lease_seconds": 3600}))
	requireNoGovernanceBodies(t, "claim_work_order", reviewClaimText)
	if !strings.Contains(reviewClaimText, `"governance_snapshot_digest":`) || !strings.Contains(reviewClaimText, `"governance_references":`) {
		t.Fatalf("review claim lost its pin identity or list: %d bytes", len(reviewClaimText))
	}
	// Every other lifecycle response of a pinned order stays free of bodies too.
	_, progressText := f.call(t, "report_progress", deliveryArgs(order, session, map[string]any{"message": "Reading the pinned designs."}))
	requireNoGovernanceBodies(t, "report_progress", progressText)
	_, usageText := f.call(t, "report_usage", deliveryArgs(order, session, map[string]any{"tokens_in": 10, "tokens_out": 5}))
	requireNoGovernanceBodies(t, "report_usage", usageText)

	before, err := f.store.GetWorkOrder(f.ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	if before.GovernanceSnapshot == nil || len(before.GovernanceSnapshot.Designs) != deliveryDesignCount || len(before.GovernanceSnapshot.Decisions) <= deliveryDecisionCount || !strings.Contains(before.GovernanceSnapshot.Designs[0].Content, deliveryBodySentinel) {
		t.Fatal("review claim did not pin complete authority")
	}
	digest := before.GovernanceSnapshotDigest

	_, baselineText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	var baseline struct {
		ContextFreshness core.ContextFreshness `json:"context_freshness"`
	}
	if err = json.Unmarshal([]byte(baselineText), &baseline); err != nil {
		t.Fatal(err)
	}

	// Move the live corpus past the pin.
	for _, id := range []string{deliveryAttachedFirst, deliveryDesignID(0)} {
		next, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: id, Content: "# Newer\n\nNEWER-BODY-SENTINEL\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - nowhere/**\n```\n", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, id, next.Version); err != nil {
			t.Fatal(err)
		}
	}
	later, err := f.store.ProposeDecision(f.ctx, core.Decision{Statement: "A decision confirmed after the pin.", Context: "Later.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ConfirmDecision(f.ctx, later.ID); err != nil {
		t.Fatal(err)
	}

	_, contractText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	requireNoGovernanceBodies(t, "get_work_order", strings.ReplaceAll(contractText, diff, ""))
	if size := wireBytesWithoutDiff(t, contractText); size >= deliveryResponseBound {
		t.Fatalf("review get_work_order is %d bytes excluding the diff, bound %d", size, deliveryResponseBound)
	} else {
		t.Logf("review get_work_order: %d-byte JSON-RPC body excluding the diff", size)
	}
	var contract struct {
		RolePrompt string `json:"role_prompt"`
		Diff       string `json:"diff"`
		Order      struct {
			Digest string `json:"governance_snapshot_digest"`
		} `json:"work_order"`
	}
	if err = json.Unmarshal([]byte(contractText), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Diff != diff || contract.Order.Digest != digest {
		t.Fatalf("diff bytes=%d digest=%q, want the pinned digest %q", len(contract.Diff), contract.Order.Digest, digest)
	}
	requireOrderedRoleGroups(t, contract.RolePrompt, []string{deliverySubmissionDiff, deliveryReviewDiff})
	requireDecisionLines(t, contract.RolePrompt, f.decision)
	if strings.Contains(contract.RolePrompt, "- "+later.ID+":") || strings.Contains(contract.RolePrompt, "NEWER-BODY-SENTINEL") || strings.Contains(contract.RolePrompt, deliveryAttachedFirst+" v2") {
		t.Fatal("review role names authority newer than its pin")
	}
	if !strings.Contains(contract.RolePrompt, "read every section, including every page of a large one, of each design listed as attached to the task or covering the diff") {
		t.Fatal("review role does not require reading governing designs in full")
	}

	// Compact responses name the same pins, and freshness still sees the move.
	_, renewText := f.call(t, "renew_work_order", deliveryArgs(order, session, map[string]any{"lease_seconds": 3600}))
	_, refreshText := f.call(t, "refresh_work_order_context", deliveryArgs(order, session, map[string]any{"prior_revision": baseline.ContextFreshness.SelectionRevision}))
	for tool, text := range map[string]string{"renew_work_order": renewText, "refresh_work_order_context": refreshText} {
		requireNoGovernanceBodies(t, tool, text)
		var response struct {
			References core.GovernanceReferences `json:"governance_references"`
		}
		if err = json.Unmarshal([]byte(text), &response); err != nil {
			t.Fatal(err)
		}
		if response.References.Designs[0].ID != deliveryAttachedFirst || response.References.Designs[0].Version != 1 {
			t.Fatalf("%s lists %+v, want the pinned version", tool, response.References.Designs[0])
		}
		for _, decision := range response.References.Decisions {
			if decision.ID == later.ID {
				t.Fatalf("%s lists a decision newer than its pin", tool)
			}
		}
	}

	after, err := f.store.GetWorkOrder(f.ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	if after.GovernanceSnapshotDigest != digest || !strings.Contains(after.GovernanceSnapshot.Designs[0].Content, deliveryBodySentinel) {
		t.Fatal("delivery changed the stored pin")
	}
}

// TestWorkOrderDeliveryVerifyListsPinsAndReportsDivergence keeps verify's
// pinned list and its live comparison apart (req-design-delivery AC-1.4;
// component-context-freshness, Pinned verify authority).
func TestWorkOrderDeliveryVerifyListsPinsAndReportsDivergence(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, "delivery-verify", func(task *core.Task) {
		task.SetupContract.VerifyStage = true
		task.NextStage = core.StageVerify
		task.ReviewedHeadSHA = strings.Repeat("b", 40)
	})
	order := f.order(t, core.StageVerify)
	const session = "delivery-verify-session"
	f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "delivery-verify-token", "agent": "codex", "model": "gpt-6.1-sol", "lease_seconds": 3600}))
	next, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: deliveryDesignID(0), Content: "# Newer\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - nowhere/**\n```\n", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, deliveryDesignID(0), next.Version); err != nil {
		t.Fatal(err)
	}
	_, contractText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	requireNoGovernanceBodies(t, "get_work_order", contractText)
	var contract struct {
		RolePrompt string `json:"role_prompt"`
	}
	if err = json.Unmarshal([]byte(contractText), &contract); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contract.RolePrompt, "- "+deliveryDesignID(0)+" v1:") || strings.Contains(contract.RolePrompt, "- "+deliveryDesignID(0)+" v2:") {
		t.Fatal("verify role does not name the pinned version")
	}
	_, refreshText := f.call(t, "refresh_work_order_context", deliveryArgs(order, session, nil))
	var refreshed workorder.FreshnessDelivery
	if err = json.Unmarshal([]byte(refreshText), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.AuthorityDivergenceCount == 0 || refreshed.GovernanceReferences == nil {
		t.Fatalf("verify refresh lost its divergence or its list: count=%d", refreshed.AuthorityDivergenceCount)
	}
	for _, design := range refreshed.GovernanceReferences.Designs {
		if design.ID == deliveryDesignID(0) && design.Version != 1 {
			t.Fatalf("verify refresh lists %+v, want the pinned version", design)
		}
	}
}

// TestWorkOrderDeliveryFreshnessStillDetectsLiveChange proves an implement
// session still learns the live authority moved, with the changed reference
// named but not the whole list again.
func TestWorkOrderDeliveryFreshnessStillDetectsLiveChange(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, "delivery-freshness")
	order := f.order(t, core.StageImplement)
	const session = "delivery-freshness-session"
	f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "delivery-freshness-token", "agent": "claude-code", "model": "claude-opus-5-5", "lease_seconds": 3600}))
	_, baselineText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	var baseline struct {
		ContextFreshness core.ContextFreshness `json:"context_freshness"`
	}
	if err := json.Unmarshal([]byte(baselineText), &baseline); err != nil {
		t.Fatal(err)
	}
	if len(baseline.ContextFreshness.Snapshot.AuthorityReferences) != 0 || baseline.ContextFreshness.Snapshot.AuthorityDigest == "" {
		t.Fatalf("freshness envelope snapshot=%+v, want the digest without a second list", baseline.ContextFreshness.Snapshot)
	}
	next, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: deliveryDesignID(0), Content: "# Newer\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - nowhere/**\n```\n", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, deliveryDesignID(0), next.Version); err != nil {
		t.Fatal(err)
	}
	_, refreshText := f.call(t, "refresh_work_order_context", deliveryArgs(order, session, map[string]any{"prior_revision": baseline.ContextFreshness.SelectionRevision}))
	var refreshed workorder.FreshnessDelivery
	if err = json.Unmarshal([]byte(refreshText), &refreshed); err != nil {
		t.Fatal(err)
	}
	if !refreshed.AuthorityReferenceChanged || refreshed.PriorAuthorityDigest == "" || refreshed.PriorAuthorityDigest == refreshed.Snapshot.AuthorityDigest {
		t.Fatalf("refresh did not detect the live change: %+v", refreshed.ContextFreshness)
	}
	if len(refreshed.PriorAuthorityReferences) != 1 || refreshed.PriorAuthorityReferences[0] != "system_design:"+deliveryDesignID(0)+":v1" {
		t.Fatalf("prior references=%v, want only the changed one", refreshed.PriorAuthorityReferences)
	}
	if refreshed.GovernanceReferences == nil {
		t.Fatal("refresh carries no list")
	}
	found := false
	for _, design := range refreshed.GovernanceReferences.Designs {
		if design.ID == deliveryDesignID(0) {
			found = design.Version == 2
		}
	}
	if !found {
		t.Fatal("implement refresh does not list the new live version")
	}
}
