package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestLoadValidatesAndCachesWholePack(t *testing.T) {
	dir := t.TempDir()
	for _, subdir := range []string{"roles"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range pipelineStages {
		if err := os.WriteFile(filepath.Join(dir, "roles", string(stage)+".md"), []byte("role "+string(stage)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "roles", "planning.md"), []byte("role planning"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles", "spec.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageSpec)
	if err != nil || role != "role spec" {
		t.Fatalf("cached role = %q, err=%v", role, err)
	}
}

func TestPackRequiresAndLoadsPlanningRole(t *testing.T) {
	dir := filepath.Join("..", "..", "pack")
	bundle, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	role, err := bundle.PlanningRole()
	if err != nil || !strings.Contains(role, "in-product planning agent") {
		t.Fatalf("planning role=%q err=%v", role, err)
	}
}

func TestReviewRoleCompletionContractMatchesExecutionPath(t *testing.T) {
	role, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(role, "submit_review_verdict") || strings.Contains(role, "```conveyor:review") {
		t.Fatalf("shared review role contains an execution-specific terminal contract: %s", role)
	}

	mcp := MCPReviewRole(role)
	normalized := strings.Join(strings.Fields(mcp), " ")
	for _, required := range []string{
		"call Conveyor's `submit_review_verdict` MCP tool",
		"wait for and observe a successful tool response",
		"Printing, returning, or describing verdict JSON is not completion",
		"A missing or failed tool response is not terminal success",
		"call `report_usage` at natural checkpoints",
		"immediately before `submit_review_verdict`",
		"missing usage must never block a review verdict (DEC-1)",
	} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("MCP review role is missing %q: %s", required, mcp)
		}
	}
	if strings.Contains(mcp, "```conveyor:review") {
		t.Fatalf("MCP review role still permits output-only completion: %s", mcp)
	}

	inProcess := InProcessReviewRole(role)
	for _, required := range []string{"End your answer with exactly one machine-owned block", "```conveyor:review", `"verdict":"approve|changes_requested"`, `"requirement_citations"`, `"applicable":true`, `"cited_ids"`, `"unknown_ids"`, `"unserved_ids"`, `"conflicts"`} {
		if !strings.Contains(inProcess, required) {
			t.Fatalf("in-process review role is missing %q: %s", required, inProcess)
		}
	}
	if strings.Contains(inProcess, "submit_review_verdict") {
		t.Fatalf("in-process review role requires an unavailable MCP tool: %s", inProcess)
	}
}

func TestDoneCriteriaContractRendersPlanAndTaskFallback(t *testing.T) {
	t.Parallel()
	plan := "## Approach\nShip.\n\n## Files touched\n- file.go\n\n## Ordering\n1. Edit.\n\n## Risks\n- None.\n\n## Done criteria\n- Tests pass."
	review := DoneCriteriaContract(core.StageReview, plan, "fallback body", true)
	for _, required := range []string{"Tests pass", "beside the pinned served-requirement acceptance criteria", "done_criteria_coverage", "verbatim-trimmed", "evidence-backed", "cannot be established", "four lists are disjoint"} {
		if !strings.Contains(review, required) {
			t.Fatalf("review contract missing %q: %s", required, review)
		}
	}
	fallback := DoneCriteriaContract(core.StageReview, "legacy spec", "Task body is done", false)
	for _, required := range []string{"Task body is done", "applicable=false", "all four finding lists empty"} {
		if !strings.Contains(fallback, required) {
			t.Fatalf("fallback contract missing %q: %s", required, fallback)
		}
	}
	legacy := "## Definition of done\n\n- Legacy checks pass.\n\n```conveyor:spec\n{}\n```"
	if HasExecutionPlan(legacy) || strings.Contains(DoneCriteriaContract(core.StageReview, legacy, "legacy task body", false), "applicable=true") {
		t.Fatalf("legacy spec with a done heading was treated as an execution plan")
	}
	if !HasExecutionPlan(plan) {
		t.Fatalf("valid plan was not recognized")
	}
}

func TestGovernanceContractExplainsAttachmentPinDowngrade(t *testing.T) {
	t.Parallel()
	contract := RenderGovernanceContract(core.StageReview, core.GovernanceSnapshot{
		Designs:         []core.GovernanceDesignContext{{ID: "DESIGN-runtime", Version: 2, Category: "Architecture", Content: "# Runtime", PinnedAtAttachment: true}},
		ResolutionNotes: []string{"pending proposal DESIGN-runtime v3 has no valid proposal event and was omitted"},
	})
	for _, required := range []string{"pinned_at_attachment=true", "older confirmed version is binding", "Governance resolution notes", "confer no authority"} {
		if !strings.Contains(contract, required) {
			t.Fatalf("governance contract missing %q: %s", required, contract)
		}
	}
}

func TestMCPRolePromptsRequireBestEffortCumulativeUsage(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "pack")
	for _, tc := range []struct {
		stage    core.Stage
		terminal string
	}{
		{stage: core.StageSpec, terminal: "submit_plan"},
		{stage: core.StageImplement, terminal: "submit_for_review"},
	} {
		role, err := (Loader{Dir: dir}).Role(tc.stage)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"report_usage", "natural checkpoints", "cumulative", "immediately before `" + tc.terminal + "`", "missing usage must never block"} {
			if !strings.Contains(role, required) {
				t.Fatalf("%s role is missing %q: %s", tc.stage, required, role)
			}
		}
	}
}

func TestRolePromptsEnforceOperatorAuthorityBoundary(t *testing.T) {
	t.Parallel()
	loader := Loader{Dir: filepath.Join("..", "..", "pack")}

	spec, err := loader.Role(core.StageSpec)
	if err != nil {
		t.Fatal(err)
	}
	normalizedSpec := strings.Join(strings.Fields(spec), " ")
	for _, required := range []string{
		"repository checkout, repository Make targets",
		"Gate approval, repository-drift resolution",
		"System Design revision or decision",
		"task-authored governance proposal tools",
		"complete revision proposals first",
		"pending proposal identifiers",
		"proposals already pending",
		"requirement-clause revisions",
		"task cancel/hold",
		"no applicable task-authored proposal surface is available",
		`"pause and report until the operator has done X,"`,
		"state why proposing is unavailable",
		"reaching and reporting the checkpoint satisfies the agent's obligation",
		"operator checkpoint reached",
		"monitor-sourced `chore` tasks",
		"check, not a keyword parser",
	} {
		if !strings.Contains(normalizedSpec, required) {
			t.Errorf("spec role is missing %q", required)
		}
	}

	implement, err := loader.Role(core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	normalizedImplement := strings.Join(strings.Fields(implement), " ")
	for _, required := range []string{
		"explicit operator checkpoint",
		"report_progress",
		"release_work_order",
		core.WorkOrderReleaseReasonOperatorCheckpointReached,
		"currently confirmed corpus authority",
		"needed requirement and complete System Design revision proposals",
		"task-authored governance proposal tools",
		"pending for operator confirmation",
		"decision_request",
		"distinct from the progress report",
		"pending proposal identifier",
		"class: authority_conflict",
		"document_id",
		"cited_version",
		"statement_or_section_id",
		"proposal tools are unavailable",
		"credential lacks the proposal capability",
		"proposal call fails",
		"release anyway",
		"truthful checkpoint release is never blocked on proposal authorship",
		"request_plan_revision",
		"repository reality conflicts with the approved plan",
		"never authorize changing or departing from the approved plan",
		"existing `released` outcome",
		"do not enter an automatic recovery loop",
		"`wip(attempt-` checkpoint commit",
		"never validation evidence",
		"resumed session",
		"same checkpoint reason",
		"re-derive the blocking condition",
		"currently served requirements",
		"current operator direction",
		"served-authority `id vN` version checked",
		"historical context",
	} {
		if !strings.Contains(normalizedImplement, required) {
			t.Errorf("implement role is missing %q", required)
		}
	}

	review, err := loader.Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	normalizedReview := strings.Join(strings.Fields(review), " ")
	for _, required := range []string{
		"blocking authority-boundary finding",
		"task-authored System Design or decision revision proposal surface",
		"accept propose-first checkpoint phrasing",
		"author complete proposals",
		"cite their pending identifiers",
		"bare pause-and-report checkpoint",
		"neither a proposal step nor a stated reason why proposing is unavailable",
		"repository-drift resolution",
		"requirement-clause revisions",
		"task cancel/hold",
		"without an applicable proposal surface",
		"agent obligation ends when reached",
		"reasoned reviewer check, not a text parser",
	} {
		if !strings.Contains(normalizedReview, required) {
			t.Errorf("review role is missing %q", required)
		}
	}
}

func TestRequirementCitationContractsAreAuthorityAware(t *testing.T) {
	t.Parallel()
	requirements := []core.ServedRequirementContext{{ID: "req-runtime", Title: "Runtime", Version: 2, Statements: []core.RequirementStatement{{ID: "REQ-3", Statement: "Retries stop.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-3.1", Statement: "Retry state is durable."}}}}}}
	implement := WithRequirementCitationContract("implement", core.StageImplement, requirements)
	review := WithRequirementCitationContract("review", core.StageReview, requirements)
	unlinked := WithRequirementCitationContract("review", core.StageReview, nil)
	unlinkedImplement := WithRequirementCitationContract("implement", core.StageImplement, nil)
	for _, required := range []string{"REQ-3: Retries stop.", "AC-3.1: Retry state is durable.", "cite the applicable stable REQ-n IDs or AC-n.m IDs", "confirmed DEC-n decisions", "governing System Design document ID", "(design-task-lifecycle)", "Do not add ornamental citations"} {
		if !strings.Contains(implement, required) {
			t.Fatalf("implement contract missing %q: %s", required, implement)
		}
	}
	for _, required := range []string{"Pinned served requirement citation authority", "req-runtime v2", "against the pinned versions above", "approved governing execution plan", "unknown_ids", "not a claim of exhaustive source parsing"} {
		if !strings.Contains(review, required) {
			t.Fatalf("review contract missing %q: %s", required, review)
		}
	}
	if !strings.Contains(unlinked, "applicable=false") || !strings.Contains(unlinked, "unlinked task remains legal") {
		t.Fatalf("unlinked contract=%s", unlinked)
	}
	for _, required := range []string{"confirmed DEC-n authority", "governing System Design document ID", "(design-task-lifecycle)", "Do not add ornamental citations"} {
		if !strings.Contains(unlinkedImplement, required) {
			t.Fatalf("unlinked implement contract missing %q: %s", required, unlinkedImplement)
		}
	}
	for _, obsolete := range []string{"spec " + "§", "alongside existing", "governing " + "spec"} {
		if strings.Contains(implement, obsolete) || strings.Contains(unlinkedImplement, obsolete) || strings.Contains(review, obsolete) {
			t.Fatalf("citation contract contains obsolete authority text %q", obsolete)
		}
	}
}

func TestGovernanceContractHeadsPinsAndBudgetsAuthority(t *testing.T) {
	t.Parallel()
	design := core.GovernanceDesignContext{ID: "DESIGN-runtime", Version: 3, Category: "Architecture", Content: strings.Repeat("mechanism ", MaxGovernanceContractBytes)}
	snapshot := core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{design},
		Decisions:              []core.Decision{{ID: "DEC-2", Status: core.DecisionConfirmed, Statement: "Use pinned authority."}},
		PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "DESIGN-runtime", Version: 4, ProposalEventID: 42, OriginTaskID: "task-design"}, {DocumentID: "DESIGN-runtime", Version: 5, ProposalEventID: 43, OriginTaskID: "task-design", Confirmed: true}},
	}
	contract := RenderGovernanceContract(core.StageReview, snapshot)
	if len(contract) > MaxGovernanceContractBytes {
		t.Fatalf("contract bytes=%d cap=%d", len(contract), MaxGovernanceContractBytes)
	}
	for _, required := range []string{"Pinned System Design authority", "System Design DESIGN-runtime v3", "Governance authority omitted by prompt budget", "Pinned decision authority", "DEC-2 [confirmed]", "System Design proposal evidence from this task", "DESIGN-runtime v4 (pending", "DESIGN-runtime v5 (confirmed after proposal", "matching pending or confirmed proposal", "confer no authority", "Operator confirmation is not a bounce condition", "design_applicable", "decision_citable"} {
		if !strings.Contains(contract, required) {
			t.Fatalf("contract missing %q: %s", required, contract)
		}
	}
	decisionsOnly := RenderGovernanceContract(core.StageReview, core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: snapshot.Decisions})
	if !strings.Contains(decisionsOnly, "No confirmed System Design governed") || !strings.Contains(decisionsOnly, "# Pinned decision authority") {
		t.Fatalf("decisions-only contract=%s", decisionsOnly)
	}
	implement := WithRequirementCitationContract("implement", core.StageImplement, nil)
	if !strings.Contains(implement, "DEC-n") {
		t.Fatalf("implement guidance=%s", implement)
	}
}

func TestImplementGovernanceContractMakesProposalsFireAndForget(t *testing.T) {
	t.Parallel()
	contract := RenderGovernanceContract(core.StageImplement, core.GovernanceSnapshot{})
	for _, required := range []string{"fire-and-forget", "report the proposal identifier in progress and the implementation handoff", "proceed immediately to `submit_for_review`", "Never wait for, request, or condition progress on operator confirmation"} {
		if !strings.Contains(contract, required) {
			t.Fatalf("implement governance contract missing %q: %s", required, contract)
		}
	}
}

func TestGovernanceContractBudgetsResolutionNotes(t *testing.T) {
	t.Parallel()
	contract := RenderGovernanceContract(core.StageReview, core.GovernanceSnapshot{
		ResolutionNotes: []string{
			"first retained note",
			strings.Repeat("malformed proposal history ", MaxGovernanceContractBytes),
			"last retained note",
		},
	})
	if len(contract) > MaxGovernanceContractBytes {
		t.Fatalf("contract bytes=%d cap=%d", len(contract), MaxGovernanceContractBytes)
	}
	for _, required := range []string{"first retained note", "last retained note", "Governance authority omitted by prompt budget", "governance resolution notes"} {
		if !strings.Contains(contract, required) {
			t.Fatalf("contract missing %q: %s", required, contract)
		}
	}
	if strings.Contains(contract, "malformed proposal history malformed proposal history") {
		t.Fatal("oversized governance resolution note was not omitted")
	}
}

func TestAgentRolesRequireSafeRepositoryValidation(t *testing.T) {
	loader := Loader{Dir: filepath.Join("..", "..", "pack")}
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		role, err := loader.Role(stage)
		if err != nil {
			t.Fatal(err)
		}
		normalized := strings.Join(strings.Fields(role), " ")
		for _, required := range []string{
			"validation only through Make targets",
			"`make test`",
			"`make test-integration`",
			"Never run raw `docker compose down` commands",
		} {
			if !strings.Contains(normalized, required) {
				t.Errorf("%s role is missing %q", stage, required)
			}
		}
	}
}

func TestReviewRoleCarriesDurableRequirementCitationGuidance(t *testing.T) {
	role, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"requirement_citations", "applicable=true", "applicable=false", "cited_ids", "unknown_ids", "unserved_ids", "conflicts"} {
		if !strings.Contains(role, required) {
			t.Fatalf("review role is missing %q: %s", required, role)
		}
	}
}

func TestStageRolesEndAtSubmissionWithoutAwaitingReview(t *testing.T) {
	spec, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageSpec)
	if err != nil {
		t.Fatal(err)
	}
	normalizedSpec := strings.Join(strings.Fields(spec), " ")
	for _, required := range []string{"materialized read-only repository checkout", "do not run `conveyor checkout` for a spec order", "report the result and exit the session"} {
		if !strings.Contains(normalizedSpec, required) {
			t.Errorf("spec role is missing %q", required)
		}
	}

	role, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(role), " ")
	for _, required := range []string{
		"After `submit_for_review` succeeds, report the handoff and exit the session",
		"Never poll `await_review` from an implementation stage session",
		"successor as a new order in a fresh session",
	} {
		if !strings.Contains(normalized, required) {
			t.Errorf("implement role is missing %q", required)
		}
	}
}
