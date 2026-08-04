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

func TestRequirementCitationContractsAreAuthorityAware(t *testing.T) {
	t.Parallel()
	requirements := []core.ServedRequirementContext{{ID: "req-runtime", Title: "Runtime", Version: 2, Statements: []core.RequirementStatement{{ID: "REQ-3", Statement: "Retries stop."}}}}
	implement := WithRequirementCitationContract("implement", core.StageImplement, requirements)
	review := WithRequirementCitationContract("review", core.StageReview, requirements)
	unlinked := WithRequirementCitationContract("review", core.StageReview, nil)
	if !strings.Contains(implement, "REQ-3: Retries stop.") || !strings.Contains(implement, "cite the applicable stable REQ-n IDs") {
		t.Fatalf("implement contract=%s", implement)
	}
	if !strings.Contains(review, "Pinned served requirement citation authority") || !strings.Contains(review, "req-runtime v2") || !strings.Contains(review, "unknown_ids") || !strings.Contains(review, "not a claim of exhaustive source parsing") {
		t.Fatalf("review contract=%s", review)
	}
	if !strings.Contains(unlinked, "applicable=false") || !strings.Contains(unlinked, "unlinked task remains legal") {
		t.Fatalf("unlinked contract=%s", unlinked)
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

func TestImplementRoleKeepsAwaitingReviewThroughPanelDeadline(t *testing.T) {
	role, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageImplement)
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(role), " ")
	for _, required := range []string{
		"`pending` means the review panel is still within its execution window",
		"Keep calling `await_review` until it returns a terminal result or `latest_seat_execution_deadline` has passed",
		"Use the pending payload's seat deadlines to bound the maximum wait",
		"Repeated pending responses alone do not mean the lifecycle is stalled",
	} {
		if !strings.Contains(normalized, required) {
			t.Errorf("implement role is missing %q", required)
		}
	}
}
