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

func TestReviewRoleRequiresSuccessfulTerminalVerdictToolCall(t *testing.T) {
	role, err := (Loader{Dir: filepath.Join("..", "..", "pack")}).Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(role), " ")
	for _, required := range []string{
		"call Conveyor's `submit_review_verdict` MCP tool",
		"wait for and observe a successful tool response",
		"Printing, returning, or describing verdict JSON is not completion",
		"A missing or failed tool response is not terminal success",
	} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("review role is missing %q: %s", required, role)
		}
	}
	if strings.Contains(role, "end your answer with") || strings.Contains(role, "```conveyor:review") {
		t.Fatalf("review role still permits output-only completion: %s", role)
	}
}
