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
	for _, required := range []string{"End your answer with exactly one machine-owned block", "```conveyor:review", `"verdict":"approve|changes_requested"`} {
		if !strings.Contains(inProcess, required) {
			t.Fatalf("in-process review role is missing %q: %s", required, inProcess)
		}
	}
	if strings.Contains(inProcess, "submit_review_verdict") {
		t.Fatalf("in-process review role requires an unavailable MCP tool: %s", inProcess)
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
