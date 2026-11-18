package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Full served-body parity for GET /v1/tasks, GET /v1/next-actions and the MCP
// list_operator_inbox read. The oracle is assembled from retained store reads
// with unloaded projection inputs and the risk fold applied per task. It never
// calls a served handler or a batch projection on the oracle path.
//
// Label: candidate-revision:retained-full-api+consumer-fold. This is not the
// literal old revision; shared pure builders (inboxTaskItems,
// attachInboxPullRequests, documentProposalItems, sortInbox,
// nextaction.CompactFrom) are reused by both sides.

// fullBodyRow mirrors the anonymous row writeTasks serves.
type fullBodyRow struct {
	core.Task
	NextAction nextaction.Compact `json:"next_action"`
	Risk       *core.TaskRisk     `json:"risk,omitempty"`
}

type fullBodyFixture struct {
	st         store.Store
	ctx        context.Context
	membership *membershipFixture
	cfg        *config.Config
	clock      time.Time
	rest       http.Handler
	mcp        http.Handler
}

// newFullBodyFixture extends approvalHTTPFixture's workspace "alpha" with:
//   - fb-plan-gate, fb-plan-dependent: a plan gate and a task depending on it;
//   - fb-stalled: a running task whose queued implement order is retry-suppressed
//     with a recovery deadline;
//   - fb-pr-opened: a merge-gate task with a recorded pull_request.opened event;
//   - fb-risk-record: a record-mode task with a medium-tier rating and a head risk order
//   - req-fb-cited: a confirmed requirement with a pending implementation
//     proposal from gate's origin, and a task_context proposal on the gate.
//
// The approval fixture's risk-approve task (enforce mode) already carries one
// recorded approval from operator and a trusted author configured in cfg.
func newFullBodyFixture(t *testing.T) *fullBodyFixture {
	t.Helper()
	approval := approvalHTTPFixture(t)
	approval.trust("author")
	st, ctx, cfg := approval.st, approval.ctx, approval.cfg
	clock := time.Now().UTC().Truncate(time.Second)

	body := `{"head_sha":"` + approvalHead + `"}`
	if seed := approval.post("operator-token", "risk-approve", "/approvals", body); seed.Code != http.StatusAccepted || !strings.Contains(seed.Body, `"recorded":true`) {
		t.Fatalf("seed approval status=%d body=%s", seed.Code, seed.Body)
	}

	gate := core.Task{ID: "fb-plan-gate", Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-fb-plan-gate",
		State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: clock.Add(-2 * time.Hour)}
	dependent := core.Task{ID: "fb-plan-dependent", Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-fb-plan-dependent",
		State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: clock.Add(-time.Hour)}
	stalled := core.Task{ID: "fb-stalled", Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-fb-stalled",
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: clock.Add(-3 * time.Hour)}
	prOpened := core.Task{ID: "fb-pr-opened", Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-fb-pr-opened",
		State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, CreatedAt: clock.Add(-4 * time.Hour)}
	for _, task := range []core.Task{gate, dependent, stalled, prOpened} {
		requireOKHTTP(t, st.CreateTask(ctx, task))
	}
	_, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent.ID, DependsOnTaskID: gate.ID, Reason: "gate first", RequestID: "fb-dep-1"})
	requireOKHTTP(t, err)

	requireOKHTTP(t, st.CreateJob(ctx, core.Job{ID: "fb-stalled-job", TaskID: stalled.ID, Stage: core.StageImplement, State: core.JobPending}))
	requireOKHTTP(t, storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "fb-stalled-order", TaskID: stalled.ID, JobID: "fb-stalled-job",
		Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: clock.Add(-time.Hour), QueueDeadline: clock.Add(time.Hour), RetrySuppressed: true}))

	requireOKHTTP(t, st.AppendEvent(ctx, core.Event{TaskID: prOpened.ID, Kind: "pull_request.opened",
		Payload: core.JSONPayload(map[string]any{"url": "https://github.com/acme/runtime/pull/1709", "repository": "acme/runtime", "number": 1709, "base_sha": "base", "head_sha": approvalHead})}))

	// risk-record carries a high-tier rating, and PlanRiskOrderCreate makes no
	// order for a high tier. A record-mode task with a medium-tier rating is
	// therefore the one that gets a head risk order, with its rating and order
	// both present.
	rules := []riskpolicy.RuleMatch{{Source: riskpolicy.RuleBuiltin, Index: 0, Description: "Internal risk code", MinTier: riskpolicy.TierMedium,
		MinHumanApprovals: 1, Paths: []string{"internal/risk.go"}}}
	riskTask := core.Task{ID: "fb-risk-record", Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-fb-risk-record",
		State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, ReviewedHeadSHA: approvalHead, CreatedAt: clock.Add(-5 * time.Hour)}
	riskTask.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)},
		Risk: &riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}}
	requireOKHTTP(t, st.CreateTask(ctx, riskTask))
	if _, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: riskTask.ID, Key: core.PathRiskKey(approvalHead), Source: core.RiskSourcePathRule,
		Tier: riskpolicy.TierMedium, HeadSHA: approvalHead, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "path rule",
		HumanApprovals: 1, MatchedRules: rules}); err != nil {
		t.Fatal(err)
	}
	_, err = taskops.ExecuteWorkOrder(ctx, st, riskTask.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		order, made, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: riskTask.ID,
			Subject: core.RiskOrderSubject{HeadSHA: approvalHead, BaseSHA: "base"}, ChangedPaths: []string{"internal/risk.go"}, MatchedRules: rules})
		if err == nil && !made {
			err = errors.New("risk order was not created")
		}
		return order, err
	})
	requireOKHTTP(t, err)

	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-fb-cited", Title: "Cited requirement"}, core.RequirementVersion{
		Content: "# Cited", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The read cites this."}}})
	requireOKHTTP(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version)
	requireOKHTTP(t, err)
	_, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Cited v2",
		Origin: core.RequirementOriginImplementation, OriginTaskID: gate.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The read cites this revision."}}})
	requireOKHTTP(t, err)
	proposer := store.WithActor(ctx, store.Actor{ID: "agent:triage", Role: core.ActorAgent})
	_, _, err = st.ProposeTaskContext(proposer, core.TaskContextProposalInput{TaskID: gate.ID, TargetKind: core.TaskContextProposalRequirement,
		TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "The gate cites this requirement."})
	requireOKHTTP(t, err)

	membership := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator":    {"alpha": core.WorkspaceRoleOperator},
			"second":      {"alpha": core.WorkspaceRoleOperator},
			"author":      {"alpha": core.WorkspaceRoleOperator},
			"contributor": {"alpha": core.WorkspaceRoleContributor},
		},
	}
	return &fullBodyFixture{st: st, ctx: ctx, membership: membership, cfg: cfg, clock: clock,
		rest: fullBodyServer(t, st, membership, cfg, clock),
		// A separate NewServer has its own read memo, so the MCP read is not
		// answered from a REST cache entry.
		mcp: fullBodyServer(t, st, membership, cfg, clock)}
}

func fullBodyServer(t *testing.T, st store.Store, membership *membershipFixture, cfg *config.Config, clock time.Time) http.Handler {
	t.Helper()
	server := NewServer(st)
	server.Workspaces, server.Memberships = membership, membership
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	server.nextActionClock = func() time.Time { return clock }
	server.readMemo.clock = func() time.Time { return clock }
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	return server.Handler()
}

// trustedFor reads the live trusted_authors of the task's repository from the
// same configuration the request reads.
func (f *fullBodyFixture) trustedFor(task core.Task) []string {
	repo, _ := f.cfg.Repo(task.Repo)
	return repo.TrustedAuthors
}

// oracleOptions are the caller inputs the operator credential produces, with
// the fixed clock. Markers, orders, and proposals are left unloaded here, so
// ReadNextActions reads what it needs itself.
func (f *fullBodyFixture) oracleOptions(t *testing.T) store.NextActionOptions {
	t.Helper()
	caps, err := f.membership.AuthorizeWorkspaceCapabilities(f.ctx, "operator", "alpha", nextActionCapabilities)
	requireOKHTTP(t, err)
	return store.NextActionOptions{Now: f.clock, Capabilities: caps, CallerUserID: "operator",
		TrustedAuthors: func(_ context.Context, task core.Task) ([]string, error) { return f.trustedFor(task), nil }}
}

// oracleRisk is the retained-API risk fold for one task: its ratings and orders
// from the per-task store reads, the review seat minimum, and the enforce-mode
// approval evaluation. It does not use the batch projection.
func (f *fullBodyFixture) oracleRisk(t *testing.T, task core.Task) *core.TaskRisk {
	t.Helper()
	if !task.SetupContract.Risk.Enabled() {
		return nil
	}
	ratings, err := f.st.ListTaskRiskRatings(f.ctx, task.ID)
	requireOKHTTP(t, err)
	orders, err := f.st.ListTaskRiskOrders(f.ctx, task.ID)
	requireOKHTTP(t, err)
	seats := store.ReviewSeatMinimum(task.SetupContract, len(f.cfg.Review.Seats))
	projection := store.ProjectTaskRisk(task, ratings, seats)
	if projection == nil {
		return nil
	}
	projection.Orders = core.TaskRiskOrders(orders)
	if task.SetupContract.Risk.Enforcing() {
		evaluation, err := store.EvaluateTaskApprovals(f.ctx, f.st, task, f.trustedFor(task), "")
		requireOKHTTP(t, err)
		store.ProjectTaskApprovals(projection, evaluation)
	}
	return projection
}

// oracleTasks is the expected GET /v1/tasks body: the store's first page in
// request order, compact actions from unloaded inputs, and the risk fold.
func (f *fullBodyFixture) oracleTasks(t *testing.T) []fullBodyRow {
	t.Helper()
	page, err := f.st.ListTaskPage(f.ctx, store.TaskOperationsQuery{Limit: defaultTaskListLimit})
	requireOKHTTP(t, err)
	opts := f.oracleOptions(t)
	opts.CompactOnly = true
	projected, err := store.ReadNextActions(f.ctx, f.st, page.Tasks, opts)
	requireOKHTTP(t, err)
	rows := make([]fullBodyRow, len(page.Tasks))
	for i, task := range page.Tasks {
		rows[i] = fullBodyRow{Task: task, NextAction: nextaction.CompactFrom(projected[task.ID]), Risk: f.oracleRisk(t, task)}
	}
	return rows
}

// oracleInbox is the full, sorted, band-filtered inbox before any page is cut.
// It uses retained reads for open tasks, their markers, and pending proposals,
// then the shared item builders.
func (f *fullBodyFixture) oracleInbox(t *testing.T, band string) []inboxItem {
	t.Helper()
	candidates, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{Open: true})
	requireOKHTTP(t, err)
	ids := make([]string, 0, len(candidates))
	for _, task := range candidates {
		ids = append(ids, task.ID)
	}
	markers, err := f.st.ListActivityMarkersForTasks(f.ctx, ids)
	requireOKHTTP(t, err)
	lastEvent := map[string]time.Time{}
	for _, marker := range markers {
		lastEvent[marker.TaskID] = marker.LastEventAt
	}
	proposals, err := f.st.ListPendingProposals(f.ctx)
	requireOKHTTP(t, err)
	opts := f.oracleOptions(t)
	opts.ProposalsLoaded, opts.Proposals = true, proposals
	projected, err := store.ReadNextActions(f.ctx, f.st, candidates, opts)
	requireOKHTTP(t, err)
	items := inboxTaskItems(candidates, projected, lastEvent)
	requireOKHTTP(t, (&Server{Store: f.st}).attachInboxPullRequests(f.ctx, items))
	items = append(items, documentProposalItems(proposals, f.clock)...)
	if band != "" {
		filtered := make([]inboxItem, 0, len(items))
		for _, item := range items {
			if item.Band == band {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	sortInbox(items)
	return items
}

// fullBodyCanonical decodes JSON so both sides compare by content: key order,
// spacing, and how a nil slice encodes do not matter, but every value does.
func fullBodyCanonical(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// fullBodyCanonicalItems normalizes a null item list to an empty list, so a
// band with no items compares equal whether the encoder saw nil or empty.
func fullBodyCanonicalItems(t *testing.T, raw []byte) []any {
	t.Helper()
	items, _ := fullBodyCanonical(t, raw).([]any)
	if items == nil {
		return []any{}
	}
	return items
}

func fullBodyMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fullBodyGet(t *testing.T, handler http.Handler, path string) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// fullBodyMCPPage is the mcpReadPage envelope that list_operator_inbox returns
// in-band. Its keys match the struct in mcp_reads.go; snapshot and expires_at
// are checked for presence, not value.
type fullBodyMCPPage struct {
	Items      json.RawMessage `json:"items"`
	Total      int             `json:"total"`
	Offset     int             `json:"offset"`
	Limit      int             `json:"limit"`
	NextOffset *int            `json:"next_offset"`
	Snapshot   string          `json:"snapshot"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Evidence   string          `json:"evidence"`
}

// fullBodyMCPInbox calls list_operator_inbox over /mcp and decodes the in-band
// mcpReadPage envelope. Unknown envelope keys fail the read.
func fullBodyMCPInbox(t *testing.T, handler http.Handler, args map[string]any) fullBodyMCPPage {
	t.Helper()
	payload := fullBodyMarshal(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_operator_inbox", "arguments": args}})
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list_operator_inbox status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("list_operator_inbox refused: %s", response.Body.String())
	}
	text := []byte(envelope.Result.Content[0].Text)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(text, &keys); err != nil {
		t.Fatalf("list_operator_inbox text is not an envelope object: %v: %s", err, text)
	}
	allowed := map[string]bool{"items": true, "total": true, "offset": true, "limit": true, "next_offset": true,
		"snapshot": true, "expires_at": true, "evidence": true}
	for key := range keys {
		if !allowed[key] {
			t.Fatalf("list_operator_inbox envelope has unexpected key %q: %s", key, text)
		}
	}
	var page fullBodyMCPPage
	if err := json.Unmarshal(text, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestReadMemoBaselineFoldServedTasksBody(t *testing.T) {
	f := newFullBodyFixture(t)
	oracle := f.oracleTasks(t)
	if len(oracle) < 6 {
		t.Fatalf("oracle task page has %d rows; the parity check is vacuous", len(oracle))
	}
	want := fullBodyCanonical(t, fullBodyMarshal(t, oracle))

	var served []fullBodyRow
	for _, phase := range []string{"cold", "warm"} {
		raw := fullBodyGet(t, f.rest, "/v1/tasks?workspace_id=alpha")
		if got := fullBodyCanonical(t, raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s GET /v1/tasks differs from the retained fold:\ngot  %s\nwant %s", phase, raw, fullBodyMarshal(t, oracle))
		}
		if phase == "cold" {
			if err := json.Unmarshal(raw, &served); err != nil {
				t.Fatal(err)
			}
		}
	}
	byID := map[string]*core.TaskRisk{}
	for _, row := range served {
		byID[row.ID] = row.Risk
	}
	// Explicit record/enforce risk fields, so the parity cannot pass by
	// omitting risk on both sides.
	enforce := byID["risk-approve"]
	if enforce == nil || enforce.Mode != riskpolicy.ModeEnforce || enforce.RecordedApprovals != 1 || enforce.RemainingApprovals != 1 {
		t.Fatalf("enforce risk row = %+v, want one recorded approval and one remaining", enforce)
	}
	record := byID["fb-risk-record"]
	if record == nil || record.Mode != riskpolicy.ModeRecord || len(record.Ratings) == 0 || len(record.Orders) == 0 {
		t.Fatalf("record risk row = %+v, want ratings and a risk order", record)
	}
}

func TestReadMemoBaselineFoldServedInboxBodies(t *testing.T) {
	f := newFullBodyFixture(t)
	for _, band := range []string{"", "decisions", "blocked", "automatic"} {
		all := f.oracleInbox(t, band)
		if band == "" && len(all) < 3 {
			t.Fatalf("unfiltered inbox oracle has %d items; the parity check is vacuous", len(all))
		}
		page := all[:min(defaultInboxPageLimit, len(all))]
		path := "/v1/next-actions?workspace_id=alpha"
		args := map[string]any{"workspace_id": "alpha"}
		if band != "" {
			path += "&band=" + band
			args["band"] = band
		}
		wantPage := fullBodyCanonicalItems(t, fullBodyMarshal(t, page))
		for _, phase := range []string{"cold", "warm"} {
			raw := fullBodyGet(t, f.rest, path)
			var body struct {
				Items json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if got := fullBodyCanonicalItems(t, body.Items); !reflect.DeepEqual(got, wantPage) {
				t.Fatalf("%s GET %s items differ from the retained fold:\ngot  %s\nwant %s", phase, path, body.Items, fullBodyMarshal(t, page))
			}
		}
		mcp := fullBodyMCPInbox(t, f.mcp, args)
		if mcp.Total != len(all) || mcp.Offset != 0 || mcp.Limit <= 0 || mcp.Evidence != mcpReadEvidence || mcp.Snapshot == "" || mcp.ExpiresAt.IsZero() {
			t.Fatalf("MCP list_operator_inbox band=%q envelope total=%d offset=%d limit=%d evidence=%q snapshot=%q, want total=%d offset=0 with evidence and a snapshot", band, mcp.Total, mcp.Offset, mcp.Limit, mcp.Evidence, mcp.Snapshot, len(all))
		}
		wantNext := len(all) > mcp.Limit
		if wantNext != (mcp.NextOffset != nil) || (wantNext && *mcp.NextOffset != mcp.Limit) {
			t.Fatalf("MCP list_operator_inbox next_offset=%v for total=%d limit=%d", mcp.NextOffset, len(all), mcp.Limit)
		}
		first := all[:min(mcp.Limit, len(all))]
		if got := fullBodyCanonicalItems(t, mcp.Items); !reflect.DeepEqual(got, fullBodyCanonicalItems(t, fullBodyMarshal(t, first))) {
			t.Fatalf("MCP list_operator_inbox band=%q page differs from the retained fold:\ngot  %s\nwant %s", band, mcp.Items, fullBodyMarshal(t, first))
		}
		if band == "" {
			// The cited proposal and the recorded pull request must reach the
			// output, so the comparison cannot pass by omission.
			text := string(mcp.Items)
			if !strings.Contains(text, "req-fb-cited") || !strings.Contains(text, `"number":1709`) {
				t.Fatalf("inbox omits the cited proposal or the recorded pull request: %s", text)
			}
		}
	}
}
