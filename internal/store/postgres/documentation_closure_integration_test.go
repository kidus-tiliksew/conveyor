package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestDocumentationClosureIntegration(t *testing.T) {
	st, ctx, ws := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	task := phase61Task(ws, core.NewTaskID(), core.TaskRunning, "")
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	policy := core.DocumentationPolicy{
		Enabled:        true,
		BaseSHA:        "0123456789abcdef0123456789abcdef01234567",
		ContentHash:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Paths:          []string{"docs/knowledge-base/**"},
		NoneStatement:  "docs: none",
		ReasonRequired: true,
		RuleText:       "Durable docs describe merged behavior only.",
	}
	pinned, err := st.PinTaskDocumentationPolicy(ctx, task.ID, policy)
	if err != nil || !pinned {
		t.Fatalf("pin=%t err=%v", pinned, err)
	}
	pinned, err = st.PinTaskDocumentationPolicy(ctx, task.ID, core.DocumentationPolicy{OffReason: store.DocumentationOffAbsent})
	if err != nil || pinned {
		t.Fatalf("second pin=%t err=%v", pinned, err)
	}
	loaded, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DocumentationPolicy == nil || !loaded.DocumentationPolicy.Enabled || len(loaded.DocumentationPolicy.Paths) != 1 || loaded.DocumentationPolicy.BaseSHA != policy.BaseSHA {
		t.Fatalf("persisted policy=%+v", loaded.DocumentationPolicy)
	}
	// The column stores the policy payload; assert the row is durable and set-once.
	var rows int
	if err = st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_documentation_policies WHERE workspace_id=$1 AND task_id=$2`, ws, task.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("policy rows=%d err=%v", rows, err)
	}
	head := "00112233445566778899aabbccddeeff00112233"
	if err = st.RecordDocumentationGateEvidence(ctx, core.DocumentationGateEvidence{
		TaskID: task.ID, HeadSHA: head, MatchedPaths: []string{"docs/knowledge-base/b.md", "docs/knowledge-base/a.md"},
		NoneStatement: "docs: none", NoneReason: "internal refactor", RecordedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	evidence, ok, err := st.GetDocumentationGateEvidence(ctx, task.ID, head)
	if err != nil || !ok || len(evidence.MatchedPaths) != 2 || evidence.MatchedPaths[0] != "docs/knowledge-base/a.md" {
		t.Fatalf("evidence=%+v ok=%t err=%v", evidence, ok, err)
	}
	if _, ok, err = st.GetDocumentationGateEvidence(ctx, task.ID, "ffffffffffffffffffffffffffffffffffffffff"); err != nil || ok {
		t.Fatalf("unknown head ok=%t err=%v", ok, err)
	}
}
