package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type LineageFixture struct {
	Store          store.Store
	Context        context.Context
	ForeignContext context.Context
	Workspace      string
	SeedLegacy     func(*testing.T, string) (int, func(*testing.T))
	// SeedHistoricalLink inserts a retained, non-projector-owned link row; see
	// Fixture.SeedHistoricalLink.
	SeedHistoricalLink func(*testing.T, context.Context, core.LineageLink)
	// HTTP returns the real httpapi handler over Store, bound to Workspace and
	// authenticated as its operator by LineageHTTPToken; see
	// Fixture.LineageHTTP. The dependency-removal case requires it.
	HTTP func(*testing.T) http.Handler
	// SeedEvents inserts events without running the projector, modelling
	// history recorded before a projector case existed; see
	// Fixture.SeedEvents. The decision-citation case requires it.
	SeedEvents func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event
}

// LineageHTTPToken is the shared operator bearer each entry point configures
// on the handler Fixture.LineageHTTP returns.
const LineageHTTPToken = "storetest-lineage-operator"

type LineageFactory func(*testing.T, []config.Repo) LineageFixture

// RunLineageConformance drives ordinary store writers and asserts the common
// event projection/rebuild contract for memory and Postgres.
func RunLineageConformance(t *testing.T, factory LineageFactory) {
	t.Helper()
	assertCanonicalVocabulary(t)
	fixture := factory(t, []config.Repo{{Name: "conveyor", Base: "main"}})
	st, ctx := fixture.Store, fixture.Context
	now := time.Now().UTC()
	parent := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	dependency := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []*core.Task{&parent, &dependency} {
		task.BaseBranch = "main"
		task.Branch = "conveyor/task-" + task.ID
		task.Title = task.ID
	}
	for _, task := range []core.Task{parent, dependency} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	child := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning,
		ParentTaskID: parent.ID, OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: now}
	child.BaseBranch, child.Branch, child.Title = "main", "conveyor/task-"+child.ID, child.ID
	if err := st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	// Workspace-scoped reads never fall back to another graph.
	if foreign, err := st.ListLineageLinks(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), fixture.Workspace+"-other")); err != nil || len(foreign) != 0 {
		t.Fatalf("cross-workspace lineage leaked: links=%v err=%v", foreign, err)
	}
	assertAbandonedDraftSupersession(t, st, ctx)
	assertEligibleReviewSupport(t, st, ctx, fixture.Workspace, "in-process")
	assertEligibleReviewSupport(t, st, ctx, fixture.Workspace, "external-mcp")
	assertLineageArtifactOrderingAndBounds(t, st, ctx, fixture.Workspace)
	assertReferenceDocumentLineage(t, st, ctx)
	assertSystemDesignEventIsolation(t, st, ctx, fixture.ForeignContext, fixture.Workspace)
	assertSystemDesignAndDecisionLineage(t, st, ctx, parent.ID)
	assertRequirementDeliveryLineage(t, st, ctx, fixture.ForeignContext, fixture.Workspace)
	assertTaskContext := assertTaskContextLineage(t, st, ctx, fixture.Workspace)
	for _, event := range []core.Event{
		{TaskID: child.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": child.ID + "-duplicate"})},
		{TaskID: child.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": child.ID + "-duplicate"})},
		{TaskID: child.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 99})},
		// Initial projection knows the historical default predecessor. Rebuild
		// cannot prove it from durable requirement versions, so it must retain
		// the event-provenanced row as unregenerable.
		{TaskID: child.ID, Kind: "requirement.version_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": "historical-conformance", "version": 2})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	wantExisting := 0
	var assertLegacy func(*testing.T)
	if fixture.SeedLegacy != nil {
		wantExisting, assertLegacy = fixture.SeedLegacy(t, child.ID)
		assertLegacy(t)
	}
	before := lineageSnapshot(t, st, ctx)
	if !hasLineageKind(before, "depends_on") {
		t.Fatalf("real dependency writer omitted lineage: %v", before)
	}
	if !hasLineageKind(before, "materializes") {
		t.Fatalf("real task writer omitted lineage: %v", before)
	}
	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "conformance", RequestID: core.NewTaskID()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Existing != wantExisting {
		t.Fatalf("existing=%d want=%d result=%+v", result.Existing, wantExisting, result)
	}
	if result.PreservedUnregenerable != 1 || result.Unsupported != 1 || result.Ambiguous != 1 {
		t.Fatalf("rebuild classification=%+v, want preserved=1 unsupported=1 ambiguous=1", result)
	}
	if assertLegacy != nil {
		assertLegacy(t)
	}
	after := lineageSnapshot(t, st, ctx)
	if result.Projected+result.PreservedUnregenerable != len(after) {
		t.Fatalf("result=%+v links=%v", result, after)
	}
	if len(before) != len(after) {
		t.Fatalf("before=%v after=%v", before, after)
	}
	for key, eventID := range before {
		if after[key] != eventID {
			t.Fatalf("edge %s provenance before=%d after=%d", key, eventID, after[key])
		}
	}
	assertTaskContext(t)
	t.Run("dependency removal suppresses its edge across HTTP reads and rebuild", func(t *testing.T) {
		assertDependencyRemovalLineage(t, fixture)
	})
	t.Run("retired feature history survives replay and bounded traversal", func(t *testing.T) {
		assertRetiredFeatureHistory(t, fixture)
	})
	t.Run("requirement versions, plan joins, and start-over successors are reachable and rebuild", func(t *testing.T) {
		assertVersionPlanAndSuccessorLineage(t, fixture)
	})
	t.Run("a hub requirement serving 40 other tasks keeps the root's own outcomes", func(t *testing.T) {
		assertHubFanoutKeepsRootOutcomes(t, fixture)
	})
	t.Run("a decision cited by 100 other verdicts keeps the root's own outcomes", func(t *testing.T) {
		assertDecisionHubKeepsRootOutcomes(t, fixture)
	})
	t.Run("node statuses and out-link reads cover tasks, versions, decisions, bundles, and verdicts", func(t *testing.T) {
		assertLineageNodeStatusesAndOutLinks(t, fixture)
	})
	// Runs last: its seeded history sits above every later event ID.
	t.Run("decision citations from verdicts and confirmed documents are reachable and rebuild", func(t *testing.T) {
		assertDecisionCitationLineage(t, fixture)
	})
}

// lineageWalkHTTP reads GET /v1/lineage/{type}/{id} with the default budget.
func lineageWalkHTTP(t *testing.T, fixture LineageFixture, handler http.Handler, root core.LineageNode) core.LineageTraversal {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/lineage/"+string(root.Type)+"/"+root.ID, nil)
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("X-Workspace-ID", fixture.Workspace)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/lineage/%s/%s status=%d body=%s", root.Type, root.ID, response.Code, response.Body.String())
	}
	var graph core.LineageTraversal
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

// confirmedDecision proposes and confirms one operator decision.
func confirmedDecision(t *testing.T, st store.Store, ctx context.Context, statement, supersedes string) core.Decision {
	t.Helper()
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: statement, Context: "Decision lineage conformance.", AlternativesRejected: "None recorded.", Origin: core.DecisionOriginOperator, Supersedes: supersedes})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err = st.ConfirmDecision(ctx, decision.ID); err != nil {
		t.Fatal(err)
	}
	return decision
}

// assertDecisionCitationLineage proves the DEC-90 relationships end to end
// on every backend (component-lineage; req-260802-72fc68 AC-1.1, AC-1.3,
// AC-6.1–AC-6.4). The real confirmation writers record cited_decision_ids
// and the real review writer records the governance assessment; a decision
// root reaches the citing versions, the verdict, its work order, its task,
// and its pull request; and a rebuild projects a seeded historical verdict's
// citation without new ambiguous or unsupported counts.
func assertDecisionCitationLineage(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.HTTP == nil || fixture.SeedEvents == nil {
		t.Fatal("lineage fixture needs HTTP and SeedEvents for the decision-citation case")
	}
	handler := fixture.HTTP(t)
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-decisions", Role: core.ActorHuman})
	cited := confirmedDecision(t, st, ctx, "Cite decisions from verdicts.", "")
	retired := confirmedDecision(t, st, ctx, "A decision that will be superseded.", "")
	successor := confirmedDecision(t, st, ctx, "Supersede the retired decision.", retired.ID)
	uncited := confirmedDecision(t, st, ctx, "A confirmed decision nobody cites.", "")
	// U's number followed by 0 is a different token, so it never cites U
	// (core.DecisionTokens; DEC-4 never matches DEC-40).
	longer := uncited.ID + "0"

	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-decisions-" + core.NewTaskID(), Title: "Decision-citing intent"}, core.RequirementVersion{
		Content: "# Decision-citing intent\n\nRelies on " + cited.ID + " and " + successor.ID + ".", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Name the decisions relied on."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	designID := "design-decisions-" + core.NewTaskID()
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: designID, Title: "Decision-citing mechanism", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Decision-citing mechanism\n\nFollows " + cited.ID + ", " + successor.ID + ", and not " + longer + ".\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/decisions/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version, 0); err != nil {
		t.Fatal(err)
	}

	task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, BaseBranch: "main", CreatedAt: time.Now().UTC()}
	task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": job.ID})},
		{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "number": 4545, "base_sha": "base-" + task.ID, "head_sha": "head-" + task.ID})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	if err = For(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: job.ID,
		ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "other", Summary: "decision lineage", Reviewer: "decision-lineage", MaxBounces: 3,
		GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &yes, DecisionCitable: &yes,
			CitedIDs: []string{cited.ID, successor.ID, cited.ID, design.ID}, UnknownIDs: []string{}, UngovernedIDs: []string{},
			SupersededIDs: []string{retired.ID}, Conflicts: []string{}}}); err != nil {
		t.Fatal(err)
	}

	requirementVersion := core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(requirement.ID, first.Version)}
	systemDesignVersion := core.LineageNode{Type: core.LineageSystemDesignVersion, ID: core.SystemDesignVersionLineageID(design.ID, designVersion.Version)}
	verdict := core.LineageNode{Type: core.LineageVerdict, ID: core.VerdictLineageID(job.ID)}
	decisionNode := func(id string) core.LineageNode { return core.LineageNode{Type: core.LineageDecision, ID: id} }
	key := func(src core.LineageNode, kind, decisionID string) string {
		return lineageKey(core.LineageLink{SrcType: src.Type, SrcID: src.ID, DstType: core.LineageDecision, DstID: decisionID, Kind: kind})
	}
	stored := func(t *testing.T) map[string]core.LineageLink {
		t.Helper()
		links, err := st.ListLineageLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]core.LineageLink{}
		for _, link := range links {
			out[lineageKey(link)] = link
		}
		return out
	}
	wantKeys := []string{
		key(requirementVersion, "cites", cited.ID), key(requirementVersion, "cites", successor.ID),
		key(systemDesignVersion, "cites", cited.ID), key(systemDesignVersion, "cites", successor.ID),
		key(verdict, "cites", cited.ID), key(verdict, "cites", successor.ID), key(verdict, "flags_superseded", retired.ID),
	}
	before := stored(t)
	decisionLinks := 0
	for _, link := range before {
		if link.Kind == "cites" || link.Kind == "flags_superseded" {
			if link.DstType != core.LineageDecision {
				t.Fatalf("decision kind ends at %v: %+v", link.DstType, link)
			}
			if link.SrcID == requirementVersion.ID || link.SrcID == systemDesignVersion.ID || link.SrcID == verdict.ID {
				decisionLinks++
			}
		}
		if link.SrcID == uncited.ID || link.DstID == uncited.ID {
			t.Fatalf("an uncited confirmed decision has a link: %+v", link)
		}
	}
	for _, want := range wantKeys {
		if before[want].CreatedByEventID == 0 {
			t.Fatalf("live writers did not project %q", want)
		}
	}
	if decisionLinks != len(wantKeys) {
		t.Fatalf("decision links from the citing nodes=%d, want exactly %d (one key per decision)", decisionLinks, len(wantKeys))
	}

	reached := func(t *testing.T, root core.LineageNode, want ...core.LineageNode) core.LineageTraversal {
		t.Helper()
		graph := lineageWalkHTTP(t, fixture, handler, root)
		nodes := map[core.LineageNode]bool{}
		for _, node := range graph.Nodes {
			nodes[core.LineageNode{Type: node.Type, ID: node.ID}] = true
		}
		for _, node := range want {
			if !nodes[node] {
				t.Fatalf("walk from %v missed %v; reached %v", root, node, graph.Nodes)
			}
		}
		return graph
	}
	reached(t, decisionNode(cited.ID), requirementVersion, systemDesignVersion, verdict,
		core.LineageNode{Type: core.LineageWorkOrder, ID: job.ID}, core.LineageNode{Type: core.LineageTask, ID: task.ID},
		core.LineageNode{Type: core.LineagePullRequest, ID: core.PullRequestLineageID("conveyor", 4545)})
	reached(t, decisionNode(retired.ID), decisionNode(successor.ID), verdict)
	reached(t, decisionNode(successor.ID), decisionNode(retired.ID), requirementVersion, systemDesignVersion, verdict)
	if graph := reached(t, decisionNode(uncited.ID)); len(graph.Nodes) != 1 || len(graph.Links) != 0 {
		t.Fatalf("uncited decision walk nodes=%v links=%v, want only the root", graph.Nodes, graph.Links)
	}

	rebuild := func(t *testing.T) core.LineageRebuildResult {
		t.Helper()
		result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "decision lineage conformance", RequestID: core.NewTaskID()})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	baseline := rebuild(t)
	// A verdict recorded before the decision cases existed: its event is on
	// the ledger, and only a rebuild projects its citation.
	historicalOrder := task.ID + "-review-historical"
	seeded := fixture.SeedEvents(t, ctx, 0, []core.Event{{ID: 1, TaskID: task.ID, Kind: "review.completed", At: time.Now().UTC(),
		Payload: core.JSONPayload(map[string]any{"review_work_order_id": historicalOrder, "verdict": "approve",
			"governance_assessment": map[string]any{"design_applicable": false, "decision_citable": true, "cited_ids": []string{cited.ID}, "unknown_ids": []string{}, "ungoverned_ids": []string{}, "superseded_ids": []string{}, "conflicts": []string{}}})}})
	historicalKey := key(core.LineageNode{Type: core.LineageVerdict, ID: core.VerdictLineageID(historicalOrder)}, "cites", cited.ID)
	if _, ok := stored(t)[historicalKey]; ok {
		t.Fatal("seeded history projected before rebuild")
	}
	restored := rebuild(t)
	after := stored(t)
	if after[historicalKey].CreatedByEventID != seeded[0].ID {
		t.Fatalf("rebuild historical verdict citation=%+v, want provenance %d", after[historicalKey], seeded[0].ID)
	}
	if restored.Projected != baseline.Projected+2 || restored.PreservedUnregenerable != baseline.PreservedUnregenerable ||
		restored.Ambiguous != baseline.Ambiguous || restored.Unsupported != baseline.Unsupported {
		t.Fatalf("rebuild with history=%+v baseline=%+v, want two more projected keys (produced_verdict, cites) and nothing else", restored, baseline)
	}
	for _, want := range wantKeys {
		if after[want].CreatedByEventID != before[want].CreatedByEventID {
			t.Fatalf("rebuild key %q provenance=%d want %d", want, after[want].CreatedByEventID, before[want].CreatedByEventID)
		}
	}
}

// assertDecisionHubKeepsRootOutcomes proves that a decision cited by 100
// other verdicts cannot crowd a task's own outcomes out of a default-budget
// walk (component-lineage; req-260802-72fc68 AC-2.3, AC-2.5, AC-6.4). The
// walk reads through ListLineageNeighborhood and GET /v1/lineage, equals a
// traversal over every workspace link, and names one literal node set on
// every backend.
func assertDecisionHubKeepsRootOutcomes(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.HTTP == nil {
		t.Fatal("lineage fixture has no HTTP handler; the decision hub case cannot read GET /v1/lineage")
	}
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-decision-hub", Role: core.ActorHuman})
	hub := confirmedDecision(t, st, ctx, "A decision many deliveries rely on.", "")
	newTask := func() core.Task {
		t.Helper()
		task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
		task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	root, other := newTask(), newTask()
	citing := func(workOrderID string) map[string]any {
		return map[string]any{"review_work_order_id": workOrderID, "verdict": "approve",
			"governance_assessment": map[string]any{"design_applicable": false, "decision_citable": true, "cited_ids": []string{hub.ID}, "unknown_ids": []string{}, "ungoverned_ids": []string{}, "superseded_ids": []string{}, "conflicts": []string{}}}
	}
	implementID, reviewID := root.ID+"-implement-1", root.ID+"-review-1"
	events := []core.Event{
		{TaskID: root.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": implementID})},
		{TaskID: root.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "number": 4646, "base_sha": "base-" + root.ID, "head_sha": "head-" + root.ID})},
		{TaskID: root.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": reviewID})},
		{TaskID: root.ID, Kind: "review.completed", Payload: core.JSONPayload(citing(reviewID))},
		{TaskID: root.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "base_sha": "merge-base-" + root.ID, "head_sha": "merge-head-" + root.ID})},
	}
	roles := map[core.LineageNode]string{
		{Type: core.LineageTask, ID: root.ID}:                                                                                    "root",
		{Type: core.LineageBlueprint, ID: root.ID}:                                                                               "root-blueprint",
		{Type: core.LineageWorkOrder, ID: implementID}:                                                                           "root-implement",
		{Type: core.LineageWorkOrder, ID: reviewID}:                                                                              "root-review",
		{Type: core.LineageVerdict, ID: core.VerdictLineageID(reviewID)}:                                                         "root-verdict",
		{Type: core.LineagePullRequest, ID: core.PullRequestLineageID("conveyor", 4646)}:                                         "root-pr",
		{Type: core.LineageCommitRange, ID: core.CommitRangeLineageID("conveyor", "base-"+root.ID, "head-"+root.ID)}:             "root-submitted-range",
		{Type: core.LineageCommitRange, ID: core.CommitRangeLineageID("conveyor", "merge-base-"+root.ID, "merge-head-"+root.ID)}: "root-merged-range",
		{Type: core.LineageDecision, ID: hub.ID}:                                                                                 "decision",
	}
	for index := 0; index < 100; index++ {
		workOrderID := fmt.Sprintf("%s-other-%03d", other.ID, index)
		events = append(events, core.Event{TaskID: other.ID, Kind: "review.completed", Payload: core.JSONPayload(citing(workOrderID))})
		roles[core.LineageNode{Type: core.LineageWorkOrder, ID: workOrderID}] = fmt.Sprintf("other-%03d", index)
		roles[core.LineageNode{Type: core.LineageVerdict, ID: core.VerdictLineageID(workOrderID)}] = fmt.Sprintf("other-%03d-verdict", index)
	}
	for _, event := range events {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	rootNode := core.LineageNode{Type: core.LineageTask, ID: root.ID}
	budget := core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes,
		MaxLinks: config.DefaultLineageContextNodes * config.DefaultLineageContextLinksPerNode, Workspace: fixture.Workspace}.Normalized()
	fetched, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(fetched, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	full, err := core.TraverseLineage(all, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Nodes, full.Nodes) || !reflect.DeepEqual(graph.Links, full.Links) || graph.Omissions != full.Omissions || !reflect.DeepEqual(graph.ExhaustionReasons, full.ExhaustionReasons) {
		t.Fatalf("fetched walk differs from the full walk\nfetched %+v\nfull    %+v", graph, full)
	}
	named := func(t *testing.T, nodes []core.LineageNode) []string {
		t.Helper()
		out := make([]string, 0, len(nodes))
		for _, node := range nodes {
			role, ok := roles[core.LineageNode{Type: node.Type, ID: node.ID}]
			if !ok {
				t.Fatalf("decision hub walk reached a node outside the fixture: %v", node)
			}
			out = append(out, role)
		}
		sort.Strings(out)
		return out
	}
	// Default fan-out 8: the decision at depth 3 examines the root's verdict
	// and the seven earliest other verdicts, and each of those reaches its
	// work order at depth 5; every backend names this one set.
	want := []string{"decision", "root", "root-blueprint", "root-implement", "root-merged-range", "root-pr", "root-review", "root-submitted-range", "root-verdict"}
	for index := 0; index < 7; index++ {
		want = append(want, fmt.Sprintf("other-%03d", index), fmt.Sprintf("other-%03d-verdict", index))
	}
	sort.Strings(want)
	if got := named(t, graph.Nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("decision hub walk nodes=%v\nwant %v", got, want)
	}
	if !graph.Truncated || graph.Omissions.FanoutNodes != 1 || !reflect.DeepEqual(graph.ExhaustionReasons, []string{"fanout"}) {
		t.Fatalf("decision hub walk truncation=%v omissions=%+v reasons=%v", graph.Truncated, graph.Omissions, graph.ExhaustionReasons)
	}
	served := lineageWalkHTTP(t, fixture, fixture.HTTP(t), rootNode)
	if got := named(t, served.Nodes); !reflect.DeepEqual(got, want) || served.Omissions != graph.Omissions || served.Budget.MaxFanout != budget.MaxFanout {
		t.Fatalf("HTTP decision hub walk nodes=%v omissions=%+v budget=%+v\nwant %v %+v", got, served.Omissions, served.Budget, want, graph.Omissions)
	}
}

// assertLineageNodeStatusesAndOutLinks proves the two bounded operator reads
// behind GET /v1/lineage status and the impact read (component-lineage;
// req-260802-72fc68 AC-5.2): each node class reports its status from one
// batched read, version nodes classify as current, superseded, proposed, or
// dismissed, a verdict reports its outcome and seat, and the out-link read
// returns one kind from the named sources in event order under its limit and
// never another workspace's links.
func assertLineageNodeStatusesAndOutLinks(t *testing.T, fixture LineageFixture) {
	t.Helper()
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-status", Role: core.ActorHuman})
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-status-" + core.NewTaskID(), Title: "Status intent"}, core.RequirementVersion{
		Content: "# Status intent", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Report status."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	newTask := func() core.Task {
		t.Helper()
		task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
		task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	oldTask := newTask()
	second, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Status intent v2", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Report status for every node."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, second.Version); err != nil {
		t.Fatal(err)
	}
	newTaskOnSecond := newTask()
	third, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Status intent v3", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Report status for every node class."}}})
	if err != nil {
		t.Fatal(err)
	}
	design, designFirst, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-status-" + core.NewTaskID(), Title: "Status design", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Status design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/status/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designFirst.Version, 0); err != nil {
		t.Fatal(err)
	}
	designDismissed, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Status design v2\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/status/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	designCurrent, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Status design v3\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/status/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designCurrent.Version, designFirst.Version); err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Report lineage status.", Context: "Operators read node state.", AlternativesRejected: "Labels alone hide outcomes.", Origin: core.DecisionOriginImplementation, OriginTaskID: oldTask.ID})
	if err != nil {
		t.Fatal(err)
	}
	reviewID := oldTask.ID + "-review-1-seat-2"
	if err = st.CreateJob(ctx, core.Job{ID: reviewID, TaskID: oldTask.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = CreateWorkOrder(ctx, st, core.WorkOrder{ID: reviewID, TaskID: oldTask.ID, JobID: reviewID, Stage: core.StageReview, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: oldTask.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": reviewID})},
		{TaskID: oldTask.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": reviewID, "verdict": "request_changes", "review_round": 1, "review_seat": 2})},
		{TaskID: oldTask.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "base_sha": "status-base-" + oldTask.ID, "head_sha": "status-head-" + oldTask.ID})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-status-" + core.NewTaskID(), Title: "Status planning", Goal: core.PlanningGoalSystemDesign})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "bundle-status-" + core.NewTaskID(), SessionID: session.ID, Title: "Status bundle",
		Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: third.Version}},
		Tasks:     []core.PlanningBundleTask{{MemberID: "one", Title: "One", Body: "Status fixture", Repo: "conveyor"}}})
	if err != nil {
		t.Fatal(err)
	}
	bundleNode := core.LineageNode{Type: core.LineagePlanningBundle, ID: bundle.ID}
	if records, err := st.ListLineageNodeRecords(ctx, []core.LineageNode{bundleNode}); err != nil || records[bundleNode].Title != "Status bundle" {
		t.Fatalf("planning bundle label record=%+v err=%v", records, err)
	}
	version := func(id string, number int) core.LineageNode {
		return core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(id, number)}
	}
	designVersion := func(number int) core.LineageNode {
		return core.LineageNode{Type: core.LineageSystemDesignVersion, ID: core.SystemDesignVersionLineageID(design.ID, number)}
	}
	want := map[core.LineageNode]core.LineageNodeStatus{
		{Type: core.LineageTask, ID: oldTask.ID}:                         {State: string(core.TaskRunning)},
		{Type: core.LineageBlueprint, ID: newTaskOnSecond.ID}:            {State: string(core.TaskRunning)},
		{Type: core.LineageWorkOrder, ID: reviewID}:                      {State: string(core.WorkOrderQueued)},
		{Type: core.LineageVerdict, ID: core.VerdictLineageID(reviewID)}: {Outcome: "request_changes", ReviewRound: 1, ReviewSeat: 2},
		{Type: core.LineageRequirement, ID: requirement.ID}:              {CurrentVersion: second.Version},
		version(requirement.ID, first.Version):                           {VersionStatus: core.LineageVersionSuperseded, CurrentVersion: second.Version},
		version(requirement.ID, second.Version):                          {VersionStatus: core.LineageVersionCurrent, CurrentVersion: second.Version},
		version(requirement.ID, third.Version):                           {VersionStatus: core.LineageVersionProposed, CurrentVersion: second.Version},
		{Type: core.LineageSystemDesign, ID: design.ID}:                  {CurrentVersion: designCurrent.Version},
		designVersion(designFirst.Version):                               {VersionStatus: core.LineageVersionSuperseded, CurrentVersion: designCurrent.Version},
		designVersion(designDismissed.Version):                           {VersionStatus: core.LineageVersionDismissed, CurrentVersion: designCurrent.Version},
		designVersion(designCurrent.Version):                             {VersionStatus: core.LineageVersionCurrent, CurrentVersion: designCurrent.Version},
		{Type: core.LineageDecision, ID: decision.ID}:                    {State: string(core.DecisionProposed)},
		bundleNode: {State: string(bundle.Status)},
		{Type: core.LineagePlanningSession, ID: session.ID}: {State: string(session.Status)},
	}
	nodes := make([]core.LineageNode, 0, len(want)+2)
	for node := range want {
		nodes = append(nodes, node)
	}
	// A caller's label never splits a status key.
	absent := []core.LineageNode{{Type: core.LineageTask, ID: "absent-" + core.NewTaskID()}, version(requirement.ID, 99), {Type: core.LineageTask, ID: oldTask.ID, Label: "labeled"}}
	nodes = append(nodes, absent...)
	statuses, err := st.ListLineageNodeStatuses(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("lineage node statuses=%+v\nwant %+v", statuses, want)
	}
	if foreign, err := st.ListLineageNodeStatuses(fixture.ForeignContext, nodes); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign-workspace statuses=%+v err=%v", foreign, err)
	}
	// The second version serves only the task attached while it was current.
	sources := []core.LineageNode{version(requirement.ID, first.Version), version(requirement.ID, second.Version), version(requirement.ID, first.Version)}
	served, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: sources, Kind: "serves", FarType: core.LineageTask, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	pairs := func(links []core.LineageLink) []string {
		out := make([]string, 0, len(links))
		for _, link := range links {
			out = append(out, link.SrcID+"->"+link.DstID)
		}
		return out
	}
	wantServed := []string{sources[0].ID + "->" + oldTask.ID, sources[1].ID + "->" + newTaskOnSecond.ID}
	if got := pairs(served); !reflect.DeepEqual(got, wantServed) {
		t.Fatalf("version serves out-links=%v want %v", got, wantServed)
	}
	if limited, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: sources, Kind: "serves", FarType: core.LineageTask, Limit: 1}); err != nil || !reflect.DeepEqual(pairs(limited), wantServed[:1]) {
		t.Fatalf("limited out-links=%v err=%v", pairs(limited), err)
	}
	merged, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: []core.LineageNode{{Type: core.LineageTask, ID: oldTask.ID}, {Type: core.LineageTask, ID: newTaskOnSecond.ID}}, Kind: "merged_range", FarType: core.LineageCommitRange, Limit: 10})
	if err != nil || !reflect.DeepEqual(pairs(merged), []string{oldTask.ID + "->" + core.CommitRangeLineageID("conveyor", "status-base-"+oldTask.ID, "status-head-"+oldTask.ID)}) {
		t.Fatalf("merged range out-links=%v err=%v", pairs(merged), err)
	}
	if foreign, err := st.ListLineageKindLinks(fixture.ForeignContext, core.LineageKindLinkQuery{Nodes: sources, Kind: "serves", FarType: core.LineageTask, Limit: 10}); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign-workspace out-links=%v err=%v", foreign, err)
	}
	if _, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Kind: "serves", FarType: core.LineageTask, Limit: 10}); err == nil {
		t.Fatal("a kind-link read without nodes was accepted")
	}
	// Inbound reads name the destination: the old task's version edge is
	// found from the task without enumerating the requirement's versions.
	inbound, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: []core.LineageNode{{Type: core.LineageTask, ID: oldTask.ID}}, Kind: "serves", Inbound: true, FarType: core.LineageRequirementVersion, Limit: 10})
	if err != nil || !reflect.DeepEqual(pairs(inbound), wantServed[:1]) {
		t.Fatalf("inbound version serves=%v err=%v", pairs(inbound), err)
	}
	assertLineageReadsBoundVersionHistory(t, st, ctx)
	// A verdict reports the latest review.completed event that names its
	// order, whatever else the task's review history holds.
	_, orders := SeedLineageVerdictHistory(t, st, ctx, fixture.Workspace, 3, 12)
	verdicts := []core.LineageNode{{Type: core.LineageVerdict, ID: core.VerdictLineageID(orders[0])}, {Type: core.LineageVerdict, ID: core.VerdictLineageID(orders[2])}}
	verdictStatuses, err := st.ListLineageNodeStatuses(ctx, verdicts)
	wantVerdicts := map[core.LineageNode]core.LineageNodeStatus{
		verdicts[0]: {Outcome: "request_changes", ReviewRound: 1, ReviewSeat: 1},
		verdicts[1]: {Outcome: "approve", ReviewRound: 1, ReviewSeat: 3},
	}
	if err != nil || !reflect.DeepEqual(verdictStatuses, wantVerdicts) {
		t.Fatalf("verdict statuses=%+v err=%v\nwant %+v", verdictStatuses, err, wantVerdicts)
	}
}

// SeedLineageVersionHistory creates a requirement with versions confirmed
// versions and returns its ID, for backend fetch-bound tests of the lineage
// status read (component-lineage).
func SeedLineageVersionHistory(t *testing.T, st store.Store, ctx context.Context, versions int) string {
	t.Helper()
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-history-" + core.NewTaskID(), Title: "Long history"}, core.RequirementVersion{
		Content: "# Long history v1", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep history."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	for number := 2; number <= versions; number++ {
		proposed, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: fmt.Sprintf("# Long history v%d", number), Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: fmt.Sprintf("Keep history %d.", number)}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
			t.Fatal(err)
		}
	}
	return requirement.ID
}

// SeedLineageVerdictHistory creates a task with orders review work orders
// and a long review history: other seats' review.completed events, then an
// approve and a later request_changes for the first order, then one approve
// for each other order. It returns the task and order IDs, for backend
// fetch-bound tests of verdict status (component-lineage).
func SeedLineageVerdictHistory(t *testing.T, st store.Store, ctx context.Context, workspace string, orders, otherEvents int) (string, []string) {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, orders)
	for index := range ids {
		ids[index] = fmt.Sprintf("%s-review-1-seat-%d", task.ID, index+1)
		if err := st.CreateJob(ctx, core.Job{ID: ids[index], TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: ids[index], TaskID: task.ID, JobID: ids[index], Stage: core.StageReview, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	completed := func(orderID, verdict string, round, seat int) {
		t.Helper()
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": orderID, "verdict": verdict, "review_round": round, "review_seat": seat, "summary": strings.Repeat("history ", 64)})}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < otherEvents; index++ {
		completed(fmt.Sprintf("%s-review-9-seat-%d", task.ID, index+1), "approve", 9, index+1)
	}
	completed(ids[0], "approve", 1, 1)
	completed(ids[0], "request_changes", 1, 1)
	for index := 1; index < orders; index++ {
		completed(ids[index], "approve", 1, index+1)
	}
	return task.ID, ids
}

// assertLineageReadsBoundVersionHistory proves both reads stay bounded by
// their request when a document's history is far larger: a status read
// returns only the nodes it names, and a kind-link read of the document's
// versions returns at most its limit in event order (component-lineage).
func assertLineageReadsBoundVersionHistory(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	const history = 24
	requirement := core.Requirement{ID: SeedLineageVersionHistory(t, st, ctx, history)}
	root := core.LineageNode{Type: core.LineageRequirement, ID: requirement.ID}
	old := core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(requirement.ID, 3)}
	statuses, err := st.ListLineageNodeStatuses(ctx, []core.LineageNode{root, old})
	want := map[core.LineageNode]core.LineageNodeStatus{
		root: {CurrentVersion: history},
		old:  {VersionStatus: core.LineageVersionSuperseded, CurrentVersion: history},
	}
	if err != nil || !reflect.DeepEqual(statuses, want) {
		t.Fatalf("long-history statuses=%+v err=%v\nwant %+v", statuses, err, want)
	}
	versions, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: []core.LineageNode{root}, Kind: "versions", FarType: core.LineageRequirementVersion, Limit: 5})
	if err != nil || len(versions) != 5 {
		t.Fatalf("bounded version links=%d err=%v", len(versions), err)
	}
	for index, link := range versions {
		if want := core.RequirementVersionLineageID(requirement.ID, index+1); link.DstID != want {
			t.Fatalf("version link %d=%s want %s", index, link.DstID, want)
		}
	}
	newest, err := st.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: []core.LineageNode{root}, Kind: "versions", FarType: core.LineageRequirementVersion, Limit: 5, Newest: true})
	if err != nil || len(newest) != 5 {
		t.Fatalf("newest version links=%d err=%v", len(newest), err)
	}
	for index, link := range newest {
		if want := core.RequirementVersionLineageID(requirement.ID, history-4+index); link.DstID != want {
			t.Fatalf("newest version link %d=%s want %s", index, link.DstID, want)
		}
	}
}

// assertHubFanoutKeepsRootOutcomes proves that a requirement serving 40
// other tasks cannot spend the default budget before the root's own work
// orders, verdict, pull request, and ranges (component-lineage;
// req-260802-72fc68 AC-2.3, AC-2.4, AC-2.5). The walk reads through
// ListLineageNeighborhood and GET /v1/lineage, equals a traversal over every
// workspace link, and names one literal node set on every backend.
func assertHubFanoutKeepsRootOutcomes(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.HTTP == nil {
		t.Fatal("lineage fixture has no HTTP handler; the hub case cannot read GET /v1/lineage")
	}
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-hub", Role: core.ActorHuman})
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-hub-" + core.NewTaskID(), Title: "Hub intent"}, core.RequirementVersion{
		Content: "# Hub intent", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Serve many tasks."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	newTask := func() core.Task {
		t.Helper()
		task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
		task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	root := newTask()
	roles := map[core.LineageNode]string{
		{Type: core.LineageTask, ID: root.ID}:               "root",
		{Type: core.LineageBlueprint, ID: root.ID}:          "root-blueprint",
		{Type: core.LineageRequirement, ID: requirement.ID}: "requirement",
		{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(requirement.ID, version.Version)}: "requirement:v1",
	}
	for index := 0; index < 40; index++ {
		sibling := newTask()
		roles[core.LineageNode{Type: core.LineageTask, ID: sibling.ID}] = fmt.Sprintf("sibling-%02d", index)
		roles[core.LineageNode{Type: core.LineageBlueprint, ID: sibling.ID}] = fmt.Sprintf("sibling-%02d-blueprint", index)
	}
	implementID, reviewID := root.ID+"-implement-1", root.ID+"-review-1"
	for _, event := range []core.Event{
		{TaskID: root.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": implementID})},
		{TaskID: root.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "number": 4343, "base_sha": "base-" + root.ID, "head_sha": "head-" + root.ID})},
		{TaskID: root.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": reviewID})},
		{TaskID: root.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": reviewID, "verdict": "approve"})},
		{TaskID: root.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "base_sha": "merge-base-" + root.ID, "head_sha": "merge-head-" + root.ID})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	own := map[core.LineageNode]string{
		{Type: core.LineageWorkOrder, ID: implementID}:                                                                           "root-implement",
		{Type: core.LineageWorkOrder, ID: reviewID}:                                                                              "root-review",
		{Type: core.LineageVerdict, ID: core.VerdictLineageID(reviewID)}:                                                         "root-verdict",
		{Type: core.LineagePullRequest, ID: core.PullRequestLineageID("conveyor", 4343)}:                                         "root-pr",
		{Type: core.LineageCommitRange, ID: core.CommitRangeLineageID("conveyor", "base-"+root.ID, "head-"+root.ID)}:             "root-submitted-range",
		{Type: core.LineageCommitRange, ID: core.CommitRangeLineageID("conveyor", "merge-base-"+root.ID, "merge-head-"+root.ID)}: "root-merged-range",
	}
	for node, role := range own {
		roles[node] = role
	}
	rootNode := core.LineageNode{Type: core.LineageTask, ID: root.ID}
	budget := core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes,
		MaxLinks: config.DefaultLineageContextNodes * config.DefaultLineageContextLinksPerNode, Workspace: fixture.Workspace}.Normalized()
	fetched, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(fetched, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	full, err := core.TraverseLineage(all, []core.LineageNode{rootNode}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Nodes, full.Nodes) || !reflect.DeepEqual(graph.Links, full.Links) || graph.Omissions != full.Omissions || !reflect.DeepEqual(graph.ExhaustionReasons, full.ExhaustionReasons) {
		t.Fatalf("fetched walk differs from the full walk\nfetched %+v\nfull    %+v", graph, full)
	}
	named := func(t *testing.T, nodes []core.LineageNode) []string {
		t.Helper()
		out := make([]string, 0, len(nodes))
		for _, node := range nodes {
			role, ok := roles[core.LineageNode{Type: node.Type, ID: node.ID}]
			if !ok {
				t.Fatalf("hub walk reached a node outside the fixture: %v", node)
			}
			out = append(out, role)
		}
		sort.Strings(out)
		return out
	}
	// Default fan-out 8: the requirement and its version each examine the
	// root and seven siblings, so seven siblings and their blueprints join
	// the root's own nodes; every backend names this one set.
	want := []string{"requirement", "requirement:v1", "root", "root-blueprint", "root-implement", "root-merged-range", "root-pr", "root-review", "root-submitted-range", "root-verdict"}
	for index := 0; index < 7; index++ {
		want = append(want, fmt.Sprintf("sibling-%02d", index), fmt.Sprintf("sibling-%02d-blueprint", index))
	}
	sort.Strings(want)
	if got := named(t, graph.Nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("hub walk nodes=%v\nwant %v", got, want)
	}
	if !graph.Truncated || graph.Omissions.FanoutNodes != 2 || !reflect.DeepEqual(graph.ExhaustionReasons, []string{"fanout"}) {
		t.Fatalf("hub walk truncation=%v omissions=%+v reasons=%v", graph.Truncated, graph.Omissions, graph.ExhaustionReasons)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/lineage/task/"+root.ID, nil)
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("X-Workspace-ID", fixture.Workspace)
	response := httptest.NewRecorder()
	fixture.HTTP(t).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/lineage/task/%s status=%d body=%s", root.ID, response.Code, response.Body.String())
	}
	var served core.LineageTraversal
	if err = json.Unmarshal(response.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if got := named(t, served.Nodes); !reflect.DeepEqual(got, want) || served.Omissions != graph.Omissions || served.Budget.MaxFanout != budget.MaxFanout {
		t.Fatalf("HTTP hub walk nodes=%v omissions=%+v budget=%+v\nwant %v %+v", got, served.Omissions, served.Budget, want, graph.Omissions)
	}
}

// assertVersionPlanAndSuccessorLineage proves the four joins that make
// lineage queryable end to end (component-lineage; req-260802-72fc68 AC-1.1,
// AC-1.3, AC-5.1, AC-5.2): a requirement reaches its versions; a requirement
// version reaches the task that served it, its work order, and its pull
// request; a task reaches its blueprint and the planning session that
// produced it; and a started-over task reaches its successor. Every read goes
// through GET /v1/lineage, and every new key survives a fresh rebuild as
// projected, never preserved or ambiguous.
func assertVersionPlanAndSuccessorLineage(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.HTTP == nil {
		t.Fatal("lineage fixture has no HTTP handler; the version-lineage case cannot read GET /v1/lineage")
	}
	handler := fixture.HTTP(t)
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-versions", Role: core.ActorHuman})
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-versions-" + core.NewTaskID(), Title: "Versioned intent"}, core.RequirementVersion{
		Content: "# Versioned intent", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Name the served version."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Versioned intent, revised", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Name the served version, revised."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, second.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
	task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	attach := func(t *testing.T) {
		t.Helper()
		if _, err := st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
			t.Fatal(err)
		}
	}
	attach(t)
	workOrderID, sessionID := task.ID+"-implement-1", "planning-"+task.ID
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": workOrderID})},
		{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "number": 4242, "base_sha": "base-" + task.ID, "head_sha": "head-" + task.ID})},
		{TaskID: task.ID, Kind: "planning_session.finalized", Payload: core.JSONPayload(map[string]any{"session_id": sessionID, "produced_task_id": task.ID})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	requirementNode := core.LineageNode{Type: core.LineageRequirement, ID: requirement.ID}
	firstNode := core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(requirement.ID, first.Version)}
	secondNode := core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(requirement.ID, second.Version)}
	taskNode := core.LineageNode{Type: core.LineageTask, ID: task.ID}
	walk := func(t *testing.T, root core.LineageNode) (map[core.LineageNode]bool, map[string]core.LineageLink) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/lineage/"+string(root.Type)+"/"+root.ID, nil)
		request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
		request.Header.Set("X-Workspace-ID", fixture.Workspace)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /v1/lineage/%s/%s status=%d body=%s", root.Type, root.ID, response.Code, response.Body.String())
		}
		var graph core.LineageTraversal
		if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
			t.Fatal(err)
		}
		nodes, links := map[core.LineageNode]bool{}, map[string]core.LineageLink{}
		for _, node := range graph.Nodes {
			nodes[core.LineageNode{Type: node.Type, ID: node.ID}] = true
		}
		for _, link := range graph.Links {
			links[lineageKey(link)] = link
		}
		return nodes, links
	}
	expectNodes := func(t *testing.T, name string, root core.LineageNode, want ...core.LineageNode) {
		t.Helper()
		nodes, _ := walk(t, root)
		for _, node := range want {
			if !nodes[node] {
				t.Fatalf("%s: walk from %v missed %v; reached %v", name, root, node, nodes)
			}
		}
	}
	expectNodes(t, "requirement root", requirementNode, firstNode, secondNode)
	expectNodes(t, "requirement version root (AC-5.2)", secondNode, taskNode,
		core.LineageNode{Type: core.LineageWorkOrder, ID: workOrderID},
		core.LineageNode{Type: core.LineagePullRequest, ID: core.PullRequestLineageID("conveyor", 4242)})
	expectNodes(t, "task root", taskNode,
		core.LineageNode{Type: core.LineageBlueprint, ID: task.ID},
		core.LineageNode{Type: core.LineagePlanningSession, ID: sessionID})
	expectNodes(t, "work order root", core.LineageNode{Type: core.LineageWorkOrder, ID: workOrderID},
		core.LineageNode{Type: core.LineageBlueprint, ID: task.ID}, requirementNode)

	servedKey := lineageKey(core.LineageLink{SrcType: secondNode.Type, SrcID: secondNode.ID, DstType: core.LineageTask, DstID: task.ID, Kind: "serves"})
	newKeys := []string{
		lineageKey(core.LineageLink{SrcType: core.LineageRequirement, SrcID: requirement.ID, DstType: firstNode.Type, DstID: firstNode.ID, Kind: "versions"}),
		lineageKey(core.LineageLink{SrcType: core.LineageRequirement, SrcID: requirement.ID, DstType: secondNode.Type, DstID: secondNode.ID, Kind: "versions"}),
		servedKey,
		lineageKey(core.LineageLink{SrcType: core.LineageBlueprint, SrcID: task.ID, DstType: core.LineageTask, DstID: task.ID, Kind: "plans"}),
	}
	stored := func(t *testing.T) map[string]core.LineageLink {
		t.Helper()
		links, err := st.ListLineageLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]core.LineageLink{}
		for _, link := range links {
			out[lineageKey(link)] = link
		}
		return out
	}
	rebuild := func(t *testing.T) core.LineageRebuildResult {
		t.Helper()
		result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "version lineage conformance", RequestID: core.NewTaskID()})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	latestRequirementEvent := func(t *testing.T, taskID, kind string) core.Event {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		var latest core.Event
		for _, event := range events {
			if event.Kind == kind && event.ID > latest.ID {
				latest = event
			}
		}
		if latest.ID == 0 {
			t.Fatalf("no %s event on %s", kind, taskID)
		}
		return latest
	}
	var attached struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(latestRequirementEvent(t, task.ID, store.TaskContextRequirementAdded).Payload, &attached); err != nil || attached.Version != second.Version {
		t.Fatalf("attachment recorded version=%d err=%v, want the current confirmed v%d", attached.Version, err, second.Version)
	}
	before := stored(t)
	for _, key := range newKeys {
		if before[key].CreatedByEventID == 0 {
			t.Fatalf("live writers did not project %q", key)
		}
	}

	baseline := rebuild(t)
	if _, err = st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
		t.Fatal(err)
	}
	var removed struct {
		Versions []int `json:"versions"`
	}
	if err = json.Unmarshal(latestRequirementEvent(t, task.ID, store.TaskContextRequirementRemoved).Payload, &removed); err != nil || !reflect.DeepEqual(removed.Versions, []int{second.Version}) {
		t.Fatalf("removal recorded versions=%v err=%v, want [%d]", removed.Versions, err, second.Version)
	}
	if _, ok := stored(t)[servedKey]; ok {
		t.Fatal("removal left requirement_version -serves-> task in place")
	}
	if _, links := walk(t, secondNode); links[servedKey].Kind != "" {
		t.Fatalf("removed version edge still served over HTTP: %+v", links[servedKey])
	}
	attach(t)
	readded := latestRequirementEvent(t, task.ID, store.TaskContextRequirementAdded).ID
	if _, links := walk(t, secondNode); links[servedKey].CreatedByEventID != readded {
		t.Fatalf("re-attached version edge over HTTP=%+v want event %d", links[servedKey], readded)
	}
	restored := rebuild(t)
	if restored.Projected != baseline.Projected || restored.PreservedUnregenerable != baseline.PreservedUnregenerable ||
		restored.Ambiguous != baseline.Ambiguous || restored.Unsupported != baseline.Unsupported {
		t.Fatalf("rebuild after remove and re-attach=%+v baseline=%+v", restored, baseline)
	}
	after := stored(t)
	for _, key := range newKeys {
		want := before[key].CreatedByEventID
		if key == servedKey {
			want = readded
		}
		if after[key].CreatedByEventID != want {
			t.Fatalf("rebuild key %q provenance=%d want %d", key, after[key].CreatedByEventID, want)
		}
	}

	result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: task.ID, RequestID: "lineage-start-over-" + task.ID, Reason: "retry", CanConfirmDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	successorNode := core.LineageNode{Type: core.LineageTask, ID: result.Successor.ID}
	expectNodes(t, "retired task root", taskNode, successorNode)
	expectNodes(t, "successor root", successorNode, taskNode, secondNode)
	successorKey := lineageKey(core.LineageLink{SrcType: core.LineageTask, SrcID: task.ID, DstType: core.LineageTask, DstID: result.Successor.ID, Kind: "succeeded_by"})
	beforeSuccessor := stored(t)[successorKey]
	if beforeSuccessor.CreatedByEventID != latestRequirementEvent(t, task.ID, "task.started_over").ID {
		t.Fatalf("succeeded_by provenance=%+v", beforeSuccessor)
	}

	// A removal recorded before removals carried versions still ends the
	// version edge on rebuild (LineageReplay). The live writers never write
	// this shape any more; it is seeded history.
	historical := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
	historical.Branch, historical.Title = "conveyor/task-"+historical.ID, historical.ID
	if err = st.CreateTask(ctx, historical); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: historical.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": second.Version})},
		{TaskID: historical.ID, Kind: store.TaskContextRequirementRemoved, Payload: core.JSONPayload(map[string]any{"id": requirement.ID})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	historicalKey := lineageKey(core.LineageLink{SrcType: secondNode.Type, SrcID: secondNode.ID, DstType: core.LineageTask, DstID: historical.ID, Kind: "serves"})
	if _, ok := stored(t)[historicalKey]; !ok {
		t.Fatal("seeded versioned attachment projected no version edge")
	}
	final := rebuild(t)
	if _, ok := stored(t)[historicalKey]; ok {
		t.Fatal("rebuild kept the version edge a historical removal ended")
	}
	if final.PreservedUnregenerable != baseline.PreservedUnregenerable || final.Ambiguous != baseline.Ambiguous || final.Unsupported != baseline.Unsupported {
		t.Fatalf("final rebuild=%+v baseline=%+v", final, baseline)
	}
	if after := stored(t)[successorKey]; after.CreatedByEventID != beforeSuccessor.CreatedByEventID {
		t.Fatalf("rebuild succeeded_by provenance=%+v want %+v", after, beforeSuccessor)
	}
}

// assertDependencyRemovalLineage proves that removing a dependency removes
// its task -depends_on-> task edge from GET /v1/lineage, that a rebuild with a
// fresh request ID neither regenerates nor preserves the key and adds nothing
// ambiguous, and that re-adding the dependency projects the edge again with
// the re-add's provenance (component-lineage; req-260802-72fc68 AC-1.1,
// AC-1.3, AC-2.3). The suite fixture is shared, so rebuild classifications
// are compared with a baseline rebuild of the same workspace.
func assertDependencyRemovalLineage(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.HTTP == nil {
		t.Fatal("lineage fixture has no HTTP handler; the dependency-removal case cannot read GET /v1/lineage")
	}
	handler := fixture.HTTP(t)
	st := fixture.Store
	ctx := store.WithActor(fixture.Context, store.Actor{ID: "operator-lineage-dependency", Role: core.ActorHuman})
	now := time.Now().UTC()
	dependent := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	dependency := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []*core.Task{&dependent, &dependency} {
		task.BaseBranch, task.Branch, task.Title = "main", "conveyor/task-"+task.ID, task.ID
		if err := st.CreateTask(ctx, *task); err != nil {
			t.Fatal(err)
		}
	}
	edgeKey := lineageKey(core.LineageLink{SrcType: core.LineageTask, SrcID: dependent.ID, DstType: core.LineageTask, DstID: dependency.ID, Kind: "depends_on"})
	httpEdge := func(t *testing.T) (core.LineageLink, bool) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/lineage/task/"+dependent.ID, nil)
		request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
		request.Header.Set("X-Workspace-ID", fixture.Workspace)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /v1/lineage/task/%s status=%d body=%s", dependent.ID, response.Code, response.Body.String())
		}
		var graph core.LineageTraversal
		if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
			t.Fatal(err)
		}
		for _, link := range graph.Links {
			if lineageKey(link) == edgeKey {
				return link, true
			}
		}
		return core.LineageLink{}, false
	}
	storedEdge := func(t *testing.T) bool {
		t.Helper()
		links, err := st.ListLineageLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, link := range links {
			if lineageKey(link) == edgeKey {
				return true
			}
		}
		return false
	}
	latestAddition := func(t *testing.T) int64 {
		t.Helper()
		events, err := st.ListEvents(ctx, dependent.ID)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		for _, event := range events {
			if event.Kind == "task.dependency_added" && event.ID > id {
				id = event.ID
			}
		}
		if id == 0 {
			t.Fatalf("no task.dependency_added event on %s", dependent.ID)
		}
		return id
	}
	rebuild := func(t *testing.T) core.LineageRebuildResult {
		t.Helper()
		result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "dependency removal conformance", RequestID: core.NewTaskID()})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	add := func(t *testing.T, requestID string) {
		t.Helper()
		if result, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent.ID, DependsOnTaskID: dependency.ID, Reason: "deliver dependency first", RequestID: requestID}); err != nil || !result.Added {
			t.Fatalf("add dependency result=%+v err=%v", result, err)
		}
	}

	add(t, "lineage-dependency-add-"+dependent.ID)
	if link, ok := httpEdge(t); !ok || link.CreatedByEventID != latestAddition(t) {
		t.Fatalf("added dependency edge over HTTP=%+v present=%v", link, ok)
	}
	baseline := rebuild(t)
	if result, err := st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: dependent.ID, DependsOnTaskID: dependency.ID, Reason: "no longer needed", RequestID: "lineage-dependency-remove-" + dependent.ID}); err != nil || !result.Removed {
		t.Fatalf("remove dependency result=%+v err=%v", result, err)
	}
	if link, ok := httpEdge(t); ok {
		t.Fatalf("removed dependency still served over HTTP: %+v", link)
	}
	if storedEdge(t) {
		t.Fatal("removed dependency edge survived the removal transaction")
	}
	removed := rebuild(t)
	if removed.PreservedUnregenerable != baseline.PreservedUnregenerable || removed.Ambiguous != baseline.Ambiguous || removed.Projected != baseline.Projected-1 {
		t.Fatalf("rebuild after removal=%+v baseline=%+v, want the key neither projected, preserved, nor ambiguous", removed, baseline)
	}
	if storedEdge(t) {
		t.Fatal("rebuild regenerated or preserved the removed dependency edge")
	}
	if link, ok := httpEdge(t); ok {
		t.Fatalf("removed dependency served over HTTP after rebuild: %+v", link)
	}

	add(t, "lineage-dependency-readd-"+dependent.ID)
	readded := latestAddition(t)
	if link, ok := httpEdge(t); !ok || link.CreatedByEventID != readded {
		t.Fatalf("re-added dependency edge over HTTP=%+v present=%v want event %d", link, ok, readded)
	}
	restored := rebuild(t)
	if restored.PreservedUnregenerable != baseline.PreservedUnregenerable || restored.Ambiguous != baseline.Ambiguous || restored.Projected != baseline.Projected {
		t.Fatalf("rebuild after re-add=%+v baseline=%+v", restored, baseline)
	}
	if link, ok := httpEdge(t); !ok || link.CreatedByEventID != readded {
		t.Fatalf("re-added dependency edge after rebuild=%+v present=%v want event %d", link, ok, readded)
	}
}

func lineageKey(link core.LineageLink) string {
	return string(link.SrcType) + "\x00" + link.SrcID + "\x00" + string(link.DstType) + "\x00" + link.DstID + "\x00" + link.Kind
}

// assertRetiredFeatureHistory proves that removing the features entity
// (task 261007-9d50e0; component-lineage) leaves its recorded history
// readable: feature events keep their IDs and payloads and project nothing,
// a retained historical_feature_assignment link keeps its legacy provenance
// across two rebuilds, traversal honors its budget, and another workspace's
// walk never reaches it.
func assertRetiredFeatureHistory(t *testing.T, fixture LineageFixture) {
	t.Helper()
	if fixture.SeedHistoricalLink == nil {
		t.Fatal("lineage fixture must seed retained historical links")
	}
	st, ctx := fixture.Store, fixture.Context
	task := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, BaseBranch: "main", CreatedAt: time.Now().UTC()}
	task.Branch, task.Title = "conveyor/task-"+task.ID, task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	featureKinds := map[string]bool{"task.feature_assigned": true, "feature.migrated": true, "migration.feature_node_dropped": true}
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": "feat-retired"})},
		{TaskID: task.ID, Kind: "feature.migrated", Payload: core.JSONPayload(map[string]string{"feature_id": "feat-retired", "requirement_id": "req-feat-retired"})},
		// Malformed and unknown-shaped historical payloads still project nothing.
		{TaskID: task.ID, Kind: "task.feature_assigned", Payload: core.JSONPayload([]any{"feat-retired", 7})},
		{TaskID: task.ID, Kind: "migration.feature_node_dropped", Payload: core.JSONPayload(map[string]any{"feature_id": 7})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	// A retained historical-link graph as migration 046 left it, rooted at the
	// task: req-feat-retired (depth 1) assigned the task and three more tasks
	// (depth 2), and req-feat-chain (depth 3) shares one of them and reaches a
	// tail task (depth 4). Seven nodes and six links, all legacy provenance.
	seededAt := time.Now().UTC().Truncate(time.Microsecond)
	retained := func(requirementID, taskID string, offset int) core.LineageLink {
		return core.LineageLink{Workspace: fixture.Workspace, SrcType: core.LineageRequirement, SrcID: requirementID,
			DstType: core.LineageTask, DstID: taskID, Kind: "historical_feature_assignment", LegacyCreatedByEvent: "feature.migrated",
			CreatedAt: seededAt.Add(time.Duration(offset) * time.Millisecond)}
	}
	chainTask, tailTask := task.ID+"-chain", task.ID+"-tail"
	fanTasks := []string{task.ID + "-fan-1", task.ID + "-fan-2"}
	historical := retained("req-feat-retired", task.ID, 0)
	retainedLinks := []core.LineageLink{
		historical,
		retained("req-feat-retired", chainTask, 1),
		retained("req-feat-retired", fanTasks[0], 2),
		retained("req-feat-retired", fanTasks[1], 3),
		retained("req-feat-chain", chainTask, 4),
		retained("req-feat-chain", tailTask, 5),
	}
	for _, link := range retainedLinks {
		fixture.SeedHistoricalLink(t, ctx, link)
	}

	type recordedEvent struct {
		id      int64
		kind    string
		payload string
	}
	featureEvents := func(t *testing.T) []recordedEvent {
		t.Helper()
		events, err := st.ListEvents(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []recordedEvent
		for _, event := range events {
			if featureKinds[event.Kind] {
				var payload any
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("historical payload unreadable: %s: %v", event.Payload, err)
				}
				canonical, _ := json.Marshal(payload)
				out = append(out, recordedEvent{event.ID, event.Kind, string(canonical)})
			}
		}
		return out
	}
	assertHistory := func(t *testing.T, events []recordedEvent) {
		t.Helper()
		ids := map[int64]bool{}
		for _, event := range events {
			ids[event.id] = true
		}
		links, err := st.ListLineageLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]int{}
		for _, link := range links {
			if ids[link.CreatedByEventID] {
				t.Fatalf("retired feature event projected a link: %+v", link)
			}
			for _, want := range retainedLinks {
				if link.Kind == want.Kind && link.SrcID == want.SrcID && link.DstID == want.DstID {
					found[want.SrcID+"->"+want.DstID]++
					if link.SrcType != want.SrcType || link.DstType != want.DstType ||
						link.LegacyCreatedByEvent != "feature.migrated" || link.CreatedByEventID != 0 || link.Workspace != fixture.Workspace {
						t.Fatalf("historical link provenance changed: %+v", link)
					}
				}
			}
		}
		for _, want := range retainedLinks {
			if found[want.SrcID+"->"+want.DstID] != 1 {
				t.Fatalf("historical feature link %s->%s count=%d in %+v", want.SrcID, want.DstID, found[want.SrcID+"->"+want.DstID], links)
			}
		}
	}
	before := featureEvents(t)
	if len(before) != 4 {
		t.Fatalf("recorded feature events=%+v", before)
	}
	assertHistory(t, before)
	for range 2 {
		if _, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "retired feature history", RequestID: core.NewTaskID()}); err != nil {
			t.Fatal(err)
		}
		after := featureEvents(t)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("recorded feature events changed across rebuild: before=%+v after=%+v", before, after)
		}
		assertHistory(t, after)
	}

	root := core.LineageNode{Type: core.LineageTask, ID: task.ID}
	historicalNode := core.LineageNode{Type: core.LineageRequirement, ID: historical.SrcID}
	chainRequirement := core.LineageNode{Type: core.LineageRequirement, ID: "req-feat-chain"}
	taskNode := func(id string) core.LineageNode { return core.LineageNode{Type: core.LineageTask, ID: id} }
	// The root's own task.created joins it to its blueprint, the one current
	// edge beside the retained history (component-lineage).
	planNode := core.LineageNode{Type: core.LineageBlueprint, ID: task.ID}
	currentLinks := 1
	// walk reads the bounded neighborhood with depth and nodes one beyond the
	// budget and the same fan-out, then traverses with the real budget. A
	// wider fetch traverses identically (component-lineage "Traversal under a
	// budget"), so Truncated still reflects what lies beyond the budget.
	walk := func(t *testing.T, ctx context.Context, budget core.LineageTraversalBudget) core.LineageTraversal {
		t.Helper()
		// Normalize first so the wider fetch keeps the traversal's fan-out.
		budget = budget.Normalized()
		fetch := budget
		fetch.MaxDepth, fetch.MaxNodes = budget.MaxDepth+1, budget.MaxNodes+1
		if budget.MaxLinks > 0 {
			fetch.MaxLinks = budget.MaxLinks + 1
		}
		links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{root}, fetch)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := core.TraverseLineage(links, []core.LineageNode{root}, budget)
		if err != nil {
			t.Fatal(err)
		}
		return graph
	}
	nodeSet := func(graph core.LineageTraversal) map[core.LineageNode]bool {
		out := map[core.LineageNode]bool{}
		for _, node := range graph.Nodes {
			out[node] = true
		}
		return out
	}
	assertNodes := func(t *testing.T, name string, graph core.LineageTraversal, want ...core.LineageNode) {
		t.Helper()
		got := nodeSet(graph)
		if len(got) != len(want) || len(graph.Nodes) != len(want) {
			t.Fatalf("%s nodes=%v want %v", name, graph.Nodes, want)
		}
		for _, node := range want {
			if !got[node] {
				t.Fatalf("%s missing node %v in %v", name, node, graph.Nodes)
			}
		}
	}
	assertLinksWithin := func(t *testing.T, name string, graph core.LineageTraversal) {
		t.Helper()
		reached := nodeSet(graph)
		for _, link := range graph.Links {
			if link.Kind == "plans" && link.SrcType == planNode.Type && link.SrcID == planNode.ID && link.DstType == root.Type && link.DstID == root.ID {
				continue
			}
			if link.Kind != "historical_feature_assignment" || link.LegacyCreatedByEvent != "feature.migrated" ||
				!reached[core.LineageNode{Type: link.SrcType, ID: link.SrcID}] || !reached[core.LineageNode{Type: link.DstType, ID: link.DstID}] {
				t.Fatalf("%s selected a link outside its reached history: %+v", name, link)
			}
		}
	}
	allNodes := []core.LineageNode{root, planNode, historicalNode, taskNode(chainTask), taskNode(fanTasks[0]), taskNode(fanTasks[1]), chainRequirement, taskNode(tailTask)}

	// Unbounded within the default budget: the whole retained graph.
	full := walk(t, ctx, core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 32, Workspace: fixture.Workspace})
	assertNodes(t, "full walk", full, allNodes...)
	assertLinksWithin(t, "full walk", full)
	if full.Depths[historicalNode] != 1 || full.Depths[taskNode(chainTask)] != 2 || full.Depths[chainRequirement] != 3 || full.Depths[taskNode(tailTask)] != 4 ||
		full.Depths[planNode] != 1 || full.Truncated || len(full.Links) != len(retainedLinks)+currentLinks || full.OmittedNodes != 0 || full.OmittedLinks != 0 || len(full.ExhaustionReasons) != 0 {
		t.Fatalf("full walk over retained history: %+v", full)
	}

	// Depth limit: depth 2 reaches the requirement and its three tasks; the
	// chain requirement one hop beyond is omitted and the tail stays unseen.
	depth := walk(t, ctx, core.LineageTraversalBudget{MaxDepth: 2, MaxNodes: 32, Workspace: fixture.Workspace})
	assertNodes(t, "depth limit", depth, root, planNode, historicalNode, taskNode(chainTask), taskNode(fanTasks[0]), taskNode(fanTasks[1]))
	assertLinksWithin(t, "depth limit", depth)
	if !depth.Truncated || depth.OmittedNodes != 1 || depth.OmittedLinks != 0 || len(depth.Links) != 4+currentLinks ||
		!reflect.DeepEqual(depth.ExhaustionReasons, []string{"depth"}) {
		t.Fatalf("depth budget not honored: %+v", depth)
	}

	// Link limit: every node is within depth and node budgets, but only three
	// of the six reached links are kept.
	linkLimited := walk(t, ctx, core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 32, MaxLinks: 3, Workspace: fixture.Workspace})
	assertNodes(t, "link limit", linkLimited, allNodes...)
	assertLinksWithin(t, "link limit", linkLimited)
	if !linkLimited.Truncated || len(linkLimited.Links) != 3 || linkLimited.OmittedLinks != len(retainedLinks)+currentLinks-3 || linkLimited.OmittedNodes != 0 ||
		!reflect.DeepEqual(linkLimited.ExhaustionReasons, []string{"links"}) {
		t.Fatalf("link budget not honored: %+v", linkLimited)
	}

	// Node limit: only the root fits; its historical neighbor and its
	// blueprint are omitted.
	bounded := walk(t, ctx, core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 1, Workspace: fixture.Workspace})
	assertNodes(t, "node limit", bounded, root)
	if !bounded.Truncated || bounded.OmittedNodes != 2 || len(bounded.Links) != 0 ||
		!reflect.DeepEqual(bounded.ExhaustionReasons, []string{"nodes"}) {
		t.Fatalf("node budget not honored: %+v", bounded)
	}

	foreignWorkspace, _ := store.WorkspaceFromContext(fixture.ForeignContext)
	foreign := walk(t, fixture.ForeignContext, core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 32, Workspace: foreignWorkspace})
	assertNodes(t, "foreign walk", foreign, root)
	if len(foreign.Links) != 0 || foreign.Truncated {
		t.Fatalf("foreign workspace reached retired feature history: %+v", foreign)
	}
}

func assertRequirementDeliveryLineage(t *testing.T, st store.Store, ctx, foreignCtx context.Context, workspace string) {
	t.Helper()
	requirementID := "req-delivery-" + core.NewTaskID()
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Scoped delivery"}, core.RequirementVersion{
		Content: "# Delivery lineage stays causal.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Walk only delivery edges."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	blueprintID := core.NewTaskID()
	blueprint := core.Task{ID: blueprintID, Workspace: workspace, Title: "Delivery blueprint", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + blueprintID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": requirement.ID})}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})}); err != nil {
		t.Fatal(err)
	}
	childID := core.NewTaskID()
	child := core.Task{ID: childID, Workspace: workspace, Title: "Materialized delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + childID, ParentTaskID: blueprint.ID, OriginSpecVersion: 1, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	unrelatedID := core.NewTaskID()
	unrelated := core.Task{ID: unrelatedID, Workspace: workspace, Title: "Unrelated dependency", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + unrelatedID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: child.ID, Kind: "task.dependency_added", Payload: core.JSONPayload(map[string]any{"task_id": child.ID, "depends_on_task_id": unrelated.ID})}); err != nil {
		t.Fatal(err)
	}

	budget := core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 16, MaxLinks: 16, Workspace: workspace}
	links, err := st.ListRequirementDeliveryLineage(ctx, requirement.ID, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(links, []core.LineageNode{{Type: core.LineageRequirement, ID: requirement.ID}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := map[string]bool{"serves": false, "versions": false, "materializes": false}
	for _, link := range graph.Links {
		if _, ok := wantKinds[link.Kind]; !ok {
			t.Fatalf("delivery read imported non-delivery edge: %+v", link)
		}
		wantKinds[link.Kind] = true
		if link.SrcID == unrelated.ID || link.DstID == unrelated.ID {
			t.Fatalf("delivery read imported dependency task: %+v", link)
		}
	}
	for kind, found := range wantKinds {
		if !found {
			t.Fatalf("delivery read omitted %s: %+v", kind, graph)
		}
	}
	if foreign, foreignErr := st.ListRequirementDeliveryLineage(foreignCtx, requirement.ID, budget); foreignErr != nil || len(foreign) != 0 {
		t.Fatalf("cross-workspace delivery lineage leaked: links=%v err=%v", foreign, foreignErr)
	}

	for index := 0; index < 6; index++ {
		taskID := core.NewTaskID()
		task := core.Task{ID: taskID, Workspace: workspace, Title: fmt.Sprintf("Delivery fan-out %d", index), Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": version.Version})}); err != nil {
			t.Fatal(err)
		}
	}
	bounded := core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 4, MaxLinks: 4, Workspace: workspace}
	links, err = st.ListRequirementDeliveryLineage(ctx, requirement.ID, bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) > bounded.MaxLinks+1 {
		t.Fatalf("delivery read exceeded bounded candidate set: got=%d budget=%+v", len(links), bounded)
	}
	graph, err = core.TraverseLineage(links, []core.LineageNode{{Type: core.LineageRequirement, ID: requirement.ID}}, bounded)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Truncated {
		t.Fatalf("over-budget delivery graph was not truncated: %+v", graph)
	}
}

func assertTaskContextLineage(t *testing.T, st store.Store, ctx context.Context, workspace string) func(*testing.T) {
	t.Helper()
	requirementIDs := []string{"req-" + core.NewTaskID(), "req-" + core.NewTaskID()}
	for _, id := range requirementIDs {
		document, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Task intent " + id}, core.RequirementVersion{
			Content: "# Task intent", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver the attached intent."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, document.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	designIDs := []string{"design-" + core.NewTaskID(), "design-" + core.NewTaskID()}
	for _, id := range designIDs {
		document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Task design " + id, Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# Task design\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/task/**\n```", Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Title: "Contextual task", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, store.TaskContextInput{RequirementIDs: requirementIDs, DesignIDs: designIDs}); err != nil {
		t.Fatal(err)
	}
	active, err := st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: requirementIDs[1:], DesignIDs: designIDs[1:]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Requirements) != 1 || active.Requirements[0].ID != requirementIDs[0] || len(active.Designs) != 1 || active.Designs[0].ID != designIDs[0] || active.Designs[0].Version != 1 {
		t.Fatalf("active task context=%+v", active)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var addedDesignEventID int64
	var removedDesignVersion int
	for _, event := range events {
		if event.Kind == store.TaskContextDesignAdded {
			var payload struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ID == designIDs[1] {
				addedDesignEventID = event.ID
			}
		}
		if event.Kind == store.TaskContextDesignRemoved {
			var payload struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				t.Fatalf("invalid design removal payload: %s", event.Payload)
			}
			removedDesignVersion = payload.Version
		}
	}
	if addedDesignEventID == 0 || removedDesignVersion != 1 {
		t.Fatalf("design audit history add_event=%d removal pinned version=%d want nonzero add and version 1", addedDesignEventID, removedDesignVersion)
	}
	assertActive := func(t *testing.T) {
		t.Helper()
		links, listErr := st.ListLineageLinks(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for index, id := range requirementIDs {
			found := false
			for _, link := range links {
				found = found || (link.SrcType == core.LineageRequirement && link.SrcID == id && link.DstType == core.LineageTask && link.DstID == task.ID && link.Kind == "serves" && link.CreatedByEventID > 0)
			}
			if found != (index == 0) {
				t.Fatalf("active task serves edge for %s found=%t want=%t", id, found, index == 0)
			}
		}
		for index, id := range designIDs {
			found := false
			for _, link := range links {
				found = found || (link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(id, 1) && link.DstType == core.LineageTask && link.DstID == task.ID && link.Kind == "governs" && link.CreatedByEventID > 0)
			}
			if found != (index == 0) {
				t.Fatalf("active task governs edge for %s found=%t want=%t", id, found, index == 0)
			}
		}
		served, servedErr := store.ServedRequirementsForTask(ctx, st, task.ID)
		if servedErr != nil || len(served.Requirements) != 1 || served.Requirements[0].ID != requirementIDs[0] {
			t.Fatalf("served requirements after removal=%+v err=%v", served, servedErr)
		}
		governance, governanceErr := store.GovernanceForTask(ctx, st, task.ID, task.Repo)
		if governanceErr != nil {
			t.Fatalf("governance after removal=%+v err=%v", governance, governanceErr)
		}
		foundDesigns := map[string]int{}
		for _, design := range governance.Designs {
			foundDesigns[design.ID] = design.Version
		}
		if foundDesigns[designIDs[0]] != 1 || foundDesigns[designIDs[1]] != 0 {
			t.Fatalf("task governance after removal=%v want active %s:v1 without %s", foundDesigns, designIDs[0], designIDs[1])
		}
	}
	assertActive(t)
	return assertActive
}

func assertSystemDesignEventIsolation(t *testing.T, st store.Store, ctx, foreignCtx context.Context, workspace string) {
	t.Helper()
	if foreignCtx == nil {
		t.Fatal("lineage conformance fixture must provide a foreign workspace context")
	}
	documentID := "shared-" + core.NewTaskID()
	for _, createCtx := range []context.Context{ctx, foreignCtx} {
		if _, _, err := st.CreateSystemDesign(createCtx,
			core.SystemDesign{ID: documentID, Title: "Workspace-local design", Category: "Architecture"},
			core.SystemDesignVersion{Content: "# Workspace-local design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListSystemDesignEvents(ctx, documentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("system-design timeline returned %d events, want created and proposed only: %+v", len(events), events)
	}
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["workspace_id"] != workspace || payload["document_id"] != documentID {
			t.Fatalf("cross-workspace system-design event leaked: %+v", event)
		}
	}
}

func assertSystemDesignAndDecisionLineage(t *testing.T, st store.Store, ctx context.Context, originTaskID string) {
	t.Helper()
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Design provenance", Goal: core.PlanningGoalSystemDesign})
	if err != nil {
		t.Fatal(err)
	}
	documentID := "design-" + core.NewTaskID()
	document, first, err := st.CreateSystemDesign(ctx,
		core.SystemDesign{ID: documentID, Title: "Dispatch ownership", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, first.Version, 0); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}
	third, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginPlanning, OriginSessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, third.Version, second.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{SessionID: session.ID, SystemDesignID: document.ID, Title: document.Title}); err != nil {
		t.Fatal(err)
	}
	fourth, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership v4\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	fifth, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership v5\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, fifth.Version, third.Version); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListSystemDesignVersions(ctx, document.ID)
	if err != nil || !versions[fourth.Version-1].Dismissed || versions[fourth.Version-1].Confirmed || !versions[fifth.Version-1].Confirmed {
		t.Fatalf("multi-pending versions=%+v err=%v", versions, err)
	}
	if _, err = st.ProposeDecision(ctx, core.Decision{ID: "DEC-2147483648", Statement: "Reject oversized IDs.", Context: "Sequence storage is int32.", AlternativesRejected: "Overflowing the high-water mark.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID}); err == nil {
		t.Fatal("oversized decision id was accepted")
	}
	firstDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Keep dispatch event-derived.", Context: "Lineage must rebuild from durable history.", AlternativesRejected: "Volunteered edges lose provenance.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if firstDecision.ID != "DEC-1" {
		t.Fatalf("invalid explicit id advanced sequence: first=%s", firstDecision.ID)
	}
	if _, err = st.ProposeDecision(ctx, core.Decision{ID: firstDecision.ID, Statement: "Duplicate.", Context: "IDs are immutable.", AlternativesRejected: "Overwriting history.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID}); !errors.Is(err, store.ErrDecisionIDConflict) {
		t.Fatalf("duplicate decision error=%v", err)
	}
	if firstDecision, err = st.ConfirmDecision(ctx, firstDecision.ID); err != nil {
		t.Fatal(err)
	}
	secondDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Project governance from the same event log.", Context: "All graph edges need stable provenance.", AlternativesRejected: "A second mutable graph would drift.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID, Supersedes: firstDecision.ID})
	if err != nil {
		t.Fatal(err)
	}
	competingDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Compete for the same predecessor.", Context: "Proposals must not reserve confirmation slots.", AlternativesRejected: "Proposal-time uniqueness deadlocks recovery.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID, Supersedes: firstDecision.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, secondDecision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, competingDecision.ID); !errors.Is(err, store.ErrDecisionSupersessionConflict) {
		t.Fatalf("competing confirmation error=%v, want typed supersession conflict", err)
	}
	dismissed, err := st.DismissDecision(ctx, competingDecision.ID)
	if err != nil || dismissed.Status != core.DecisionDismissed || dismissed.DismissedBy == "" || dismissed.DismissedAt.IsZero() {
		t.Fatalf("dismissed decision=%+v err=%v", dismissed, err)
	}
	if _, err = st.DismissDecision(ctx, competingDecision.ID); err != nil {
		t.Fatalf("idempotent dismissal error=%v", err)
	}
	if _, err = st.ConfirmDecision(ctx, competingDecision.ID); !errors.Is(err, store.ErrDecisionSupersessionConflict) {
		t.Fatalf("dismissed confirmation error=%v, want typed lifecycle conflict", err)
	}
	if predecessor, getErr := st.GetDecision(ctx, secondDecision.ID); getErr != nil || predecessor.Status != core.DecisionConfirmed {
		t.Fatalf("dismissal altered confirmed successor=%+v err=%v", predecessor, getErr)
	}
	planningDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Record planning provenance.", Context: "Production planning emits session origins.", AlternativesRejected: "Task-only coverage misses the live path.", Origin: core.DecisionOriginPlanning, OriginSessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"system_design:versions":                 false,
		"system_design_version:supersedes":       false,
		"system_design_version:governs-path":     false,
		"system_design_version:proposed-task":    false,
		"system_design_version:proposed-session": false,
		"planning_session:produced-design":       false,
		"decision:proposed_by-task":              false,
		"decision:proposed_by-session":           false,
		"decision:supersedes":                    false,
	}
	for _, link := range links {
		switch {
		case link.SrcType == core.LineageSystemDesign && link.SrcID == document.ID && link.DstType == core.LineageSystemDesignVersion && link.DstID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.Kind == "versions":
			want["system_design:versions"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineageSystemDesignVersion && link.DstID == core.SystemDesignVersionLineageID(document.ID, second.Version) && link.Kind == "supersedes":
			want["system_design_version:supersedes"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineageRepositoryPath && link.DstID == core.RepoPathComponentLineageID("conveyor", "internal/dispatch/**") && link.Kind == "governs":
			want["system_design_version:governs-path"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, second.Version) && link.DstType == core.LineageTask && link.DstID == originTaskID && link.Kind == "proposed_by":
			want["system_design_version:proposed-task"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineagePlanningSession && link.DstID == session.ID && link.Kind == "proposed_by":
			want["system_design_version:proposed-session"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineagePlanningSession && link.SrcID == session.ID && link.DstType == core.LineageSystemDesign && link.DstID == document.ID && link.Kind == "produced_design":
			want["planning_session:produced-design"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == secondDecision.ID && link.DstType == core.LineageTask && link.Kind == "proposed_by":
			want["decision:proposed_by-task"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == planningDecision.ID && link.DstType == core.LineagePlanningSession && link.DstID == session.ID && link.Kind == "proposed_by":
			want["decision:proposed_by-session"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == secondDecision.ID && link.DstType == core.LineageDecision && link.DstID == firstDecision.ID && link.Kind == "supersedes":
			want["decision:supersedes"] = link.CreatedByEventID > 0
		}
	}
	for edge, found := range want {
		if !found {
			t.Fatalf("missing event-provenanced %s edge: %+v", edge, links)
		}
	}
}

func assertReferenceDocumentLineage(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	documentID := "ref-" + core.NewTaskID()
	document, first, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Lineage conformance " + documentID},
		core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Overview\n\nInitial context."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SupersedeReferenceDocument(ctx, document.ID,
		core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Overview\n\nCurrent context."})
	if err != nil || second.SupersedesVersion != first.Version {
		t.Fatalf("reference supersede=%+v err=%v", second, err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Reference lineage"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, second.Version, session.ID); err != nil {
		t.Fatal(err)
	}
	requirementID := "req-" + core.NewTaskID()
	requirement, version, err := st.CreateRequirement(ctx,
		core.Requirement{ID: requirementID, Title: "Derived reference lineage"},
		core.RequirementVersion{
			Content: "# Derived reference lineage.", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
			Statements:  []core.RequirementStatement{{ID: "REQ-1", Statement: "Reference lineage stays durable."}},
			DerivedFrom: &core.RequirementDerivation{DocumentID: document.ID, Version: second.Version, SectionAnchor: "#overview", TargetID: "REQ-1"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]core.LineageLink{
		"consulted": {
			SrcType: core.LineagePlanningSession, SrcID: session.ID,
			DstType: core.LineageReferenceDocumentVersion, DstID: core.ReferenceDocumentVersionLineageID(document.ID, second.Version), Kind: "consulted",
		},
		"derived_from": {
			SrcType: core.LineageRequirementVersion, SrcID: core.RequirementVersionLineageID(requirement.ID, version.Version),
			DstType: core.LineageReferenceDocumentVersion, DstID: core.ReferenceDocumentVersionLineageID(document.ID, second.Version), Kind: "derived_from",
		},
	}
	for _, link := range links {
		candidate, ok := want[link.Kind]
		if ok && link.SrcType == candidate.SrcType && link.SrcID == candidate.SrcID && link.DstType == candidate.DstType && link.DstID == candidate.DstID && link.CreatedByEventID > 0 {
			delete(want, link.Kind)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing corrected reference lineage edges: want=%+v links=%+v", want, links)
	}
}

func assertLineageArtifactOrderingAndBounds(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC()
	rootID := core.NewTaskID()
	root := core.Task{ID: rootID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + rootID, State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(ctx, root); err != nil {
		t.Fatal(err)
	}
	dependencies := make([]core.Task, 36)
	for i := range dependencies {
		id := core.NewTaskID()
		dependencies[i] = core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := st.CreateTask(ctx, dependencies[i]); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: rootID, At: now.Add(time.Duration(i) * time.Second), Kind: "task.dependency_added", Payload: core.JSONPayload(map[string]any{
			"task_id": rootID, "depends_on_task_id": id,
		})}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("artifact-%02d.txt", i), ContentType: "text/plain", TaskID: id}, []byte(fmt.Sprintf("unique-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	sharedContent := []byte("content-deduplicated-artifact")
	shared, err := st.CreateArtifact(ctx, core.Artifact{Name: "shared-near.txt", ContentType: "text/plain", TaskID: dependencies[0].ID}, sharedContent)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, repeatErr := st.CreateArtifact(ctx, core.Artifact{Name: "shared-farther.txt", ContentType: "text/plain", TaskID: dependencies[1].ID}, sharedContent); repeatErr != nil || repeated.ID != shared.ID {
		t.Fatalf("deduplicated artifact=%+v err=%v, want id %s", repeated, repeatErr, shared.ID)
	}
	roleContent := []byte("same-owner-two-role-ordering")
	roleArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "same-owner-context.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: rootID, CreatedAt: now}, roleContent)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, repeatErr := st.CreateArtifact(ctx, core.Artifact{Name: "same-owner-output.txt", ContentType: "text/plain", Role: core.ArtifactRoleGeneratedOutput, TaskID: rootID, CreatedAt: now}, roleContent); repeatErr != nil || repeated.ID != roleArtifact.ID {
		t.Fatalf("same-owner role artifact=%+v err=%v, want id %s", repeated, repeatErr, roleArtifact.ID)
	}
	budget := core.LineageTraversalBudget{MaxDepth: 1, MaxNodes: config.DefaultLineageContextNodes, MaxLinks: config.DefaultLineageContextNodes * config.DefaultLineageContextLinksPerNode, Workspace: workspace}
	links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{{Type: core.LineageTask, ID: rootID}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(links, []core.LineageNode{{Type: core.LineageTask, ID: rootID}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Truncated || len(graph.Nodes) != config.DefaultLineageContextNodes {
		t.Fatalf("over-budget graph=%+v", graph)
	}
	invalid := core.LineageNode{Type: core.LineageNodeType("invalid"), ID: "must-not-match"}
	withInvalid := append(append([]core.LineageNode(nil), graph.Nodes...), invalid)
	artifacts, err := st.ListArtifactsForLineage(ctx, graph.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	invalidArtifacts, err := st.ListArtifactsForLineage(ctx, withInvalid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(artifacts, invalidArtifacts) {
		t.Fatalf("invalid node changed artifact ordering:\nvalid=%+v\ninvalid=%+v", artifacts, invalidArtifacts)
	}
	ranks := make(map[string]int, len(graph.Nodes))
	for i, node := range graph.Nodes {
		ranks[node.ID] = i
	}
	lastRank, sharedNear, sharedFar, outputRole, contextRole := -1, -1, -1, -1, -1
	for i, artifact := range artifacts {
		rank, ok := ranks[artifact.TaskID]
		if !ok || rank < lastRank {
			t.Fatalf("artifact order diverged at %d (%+v), rank=%d after %d", i, artifact, rank, lastRank)
		}
		lastRank = rank
		if artifact.ID == shared.ID && artifact.TaskID == dependencies[0].ID {
			sharedNear = i
		}
		if artifact.ID == shared.ID && artifact.TaskID == dependencies[1].ID {
			sharedFar = i
		}
		if artifact.ID == roleArtifact.ID && artifact.Role == core.ArtifactRoleGeneratedOutput {
			outputRole = i
		}
		if artifact.ID == roleArtifact.ID && artifact.Role == core.ArtifactRoleTaskContext {
			contextRole = i
		}
	}
	if sharedNear < 0 || sharedFar < 0 || sharedNear >= sharedFar {
		t.Fatalf("deduplicated artifact relation order near=%d farther=%d artifacts=%+v", sharedNear, sharedFar, artifacts)
	}
	if outputRole < 0 || contextRole < 0 || outputRole >= contextRole {
		t.Fatalf("same-owner role order output=%d context=%d artifacts=%+v", outputRole, contextRole, artifacts)
	}
}

func assertCanonicalVocabulary(t *testing.T) {
	t.Helper()
	want := []string{"consulted", "creates", "depends_on", "derived_from", "dispatches", "governs", "materializes", "merged_range", "plans", "produced_blueprint", "produced_bundle", "produced_design", "produced_requirement", "produced_verdict", "proposed_by", "proposes", "serves", "submitted_as", "submitted_range", "succeeded_by", "supersedes", "supports", "versions", "cites", "flags_superseded"}
	got := store.CanonicalLineageKinds()
	if len(got) != len(want) {
		t.Fatalf("canonical lineage vocabulary=%v, want %v", got, want)
	}
	for _, kind := range want {
		if _, ok := got[kind]; !ok {
			t.Fatalf("canonical lineage vocabulary omitted %q: %v", kind, got)
		}
	}
}

func assertAbandonedDraftSupersession(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	blueprintID := core.NewTaskID()
	blueprint := core.Task{ID: blueprintID, Workspace: workspaceForFixture(ctx), Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + blueprintID, Title: "Abandoned blueprint draft", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"confirmed v1", "abandoned v2", "proposed v3"} {
		if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{
			TaskID: blueprintID, Content: content,
			Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = findLineageLink(t, st, ctx, "supersedes", core.BlueprintVersionLineageID(blueprintID, 3), core.BlueprintVersionLineageID(blueprintID, 2))
	_ = findLineageLink(t, st, ctx, "supersedes", core.BlueprintVersionLineageID(blueprintID, 2), core.BlueprintVersionLineageID(blueprintID, 1))

	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Lineage conformance"})
	if err != nil {
		t.Fatal(err)
	}
	requirementID := "req-" + core.NewTaskID()
	version := func(content string, statement int) core.RequirementVersion {
		return core.RequirementVersion{
			RequirementID: requirementID, Content: "# " + content, Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Intent revision " + content}},
		}
	}
	_, first, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Intent"}, version("one", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, first.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementVersion(ctx, version("abandoned", 2)); err != nil {
		t.Fatal(err)
	}
	third, err := st.ProposeRequirementVersion(ctx, version("confirmed", 3))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, third.Version); err != nil {
		t.Fatal(err)
	}
	wantSrc, wantDst := core.RequirementVersionLineageID(requirementID, 3), core.RequirementVersionLineageID(requirementID, 1)
	_ = findLineageLink(t, st, ctx, "supersedes", wantSrc, wantDst)
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, 2); err == nil {
		t.Fatal("late confirmation of superseded version 2 succeeded after version 3")
	} else {
		var superseded *store.RequirementVersionSuperseded
		if !errors.As(err, &superseded) {
			t.Fatalf("late confirmation error=%v, want RequirementVersionSuperseded", err)
		}
	}
	assertNoLineageLink(t, st, ctx, "supersedes", core.RequirementVersionLineageID(requirementID, 2), core.RequirementVersionLineageID(requirementID, 3))
}

func workspaceForFixture(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}

func assertEligibleReviewSupport(t *testing.T, st store.Store, ctx context.Context, workspace, reviewer string) {
	t.Helper()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
		Title: reviewer, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: taskID + "-review-1-seat-1", TaskID: taskID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}); err != nil {
		t.Fatal(err)
	}
	eligible, err := st.CreateArtifact(ctx, core.Artifact{Name: reviewer + ".png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: taskID}, testimage.PNG("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	ineligible, err := st.CreateArtifact(ctx, core.Artifact{Name: reviewer + ".txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: taskID}, []byte("context"))
	if err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{TaskID: taskID, JobID: job.ID, ReviewWorkOrderID: job.ID,
		ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "tests", Summary: reviewer,
		Reviewer: reviewer, EvidenceIDs: []string{eligible.ID}, MaxBounces: 3}
	if err = For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	first := findLineageLink(t, st, ctx, "supports", eligible.ID, core.VerdictLineageID(job.ID))
	if err = For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatalf("idempotent review retry: %v", err)
	}
	repeated := findLineageLink(t, st, ctx, "supports", eligible.ID, core.VerdictLineageID(job.ID))
	if repeated.CreatedByEventID != first.CreatedByEventID {
		t.Fatalf("repeated review replaced first provenance: before=%+v after=%+v", first, repeated)
	}
	assertNoLineageLink(t, st, ctx, "supports", ineligible.ID, core.VerdictLineageID(job.ID))
}

func findLineageLink(t *testing.T, st store.Store, ctx context.Context, kind, srcID, dstID string) core.LineageLink {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Kind == kind && link.SrcID == srcID && link.DstID == dstID {
			return link
		}
	}
	t.Fatalf("missing lineage %s %s -> %s in %+v", kind, srcID, dstID, links)
	return core.LineageLink{}
}

func assertNoLineageLink(t *testing.T, st store.Store, ctx context.Context, kind, srcID, dstID string) {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Kind == kind && link.SrcID == srcID && link.DstID == dstID {
			t.Fatalf("unexpected lineage %s %s -> %s: %+v", kind, srcID, dstID, link)
		}
	}
}

func lineageSnapshot(t *testing.T, st store.Store, ctx context.Context) map[string]int64 {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Kind < links[j].Kind })
	out := map[string]int64{}
	canonical := store.CanonicalLineageKinds()
	for _, link := range links {
		if _, ok := canonical[link.Kind]; !ok {
			continue // retained non-projector-owned legacy link
		}
		if !link.SrcType.Valid() || !link.DstType.Valid() || link.CreatedByEventID == 0 {
			t.Fatalf("invalid lineage link: %+v", link)
		}
		out[link.Kind+"\x00"+link.SrcID+"\x00"+link.DstID] = link.CreatedByEventID
	}
	return out
}

func hasLineageKind(snapshot map[string]int64, kind string) bool {
	for key := range snapshot {
		if len(key) > len(kind) && key[:len(kind)+1] == kind+"\x00" {
			return true
		}
	}
	return false
}
