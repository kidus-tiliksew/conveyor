package storetest

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runDocumentationClosure(t *testing.T, x Fixture) {
	policy := core.DocumentationPolicy{
		Enabled:        true,
		BaseSHA:        "0123456789abcdef0123456789abcdef01234567",
		ContentHash:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Paths:          []string{"docs/knowledge-base/**"},
		NoneStatement:  "docs: none",
		ReasonRequired: true,
		RuleText:       "Durable docs describe merged behavior only.",
	}
	t.Run("pin_is_set_once", func(t *testing.T) {
		task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "policy", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		requireOK(t, x.Backend.CreateTask(x.Context, task))

		pinned, err := x.Backend.PinTaskDocumentationPolicy(x.Context, task.ID, policy)
		requireOK(t, err)
		if !pinned {
			t.Fatal("first pin was not recorded")
		}
		loaded, err := x.Backend.GetTask(x.Context, task.ID)
		requireOK(t, err)
		if loaded.DocumentationPolicy == nil || !loaded.DocumentationPolicy.Enabled || len(loaded.DocumentationPolicy.Paths) != 1 {
			t.Fatalf("hydrated policy=%+v", loaded.DocumentationPolicy)
		}

		replacement := policy
		replacement.BaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
		replacement.Paths = []string{"docs/other/**"}
		pinned, err = x.Backend.PinTaskDocumentationPolicy(x.Context, task.ID, replacement)
		requireOK(t, err)
		if pinned {
			t.Fatal("second pin overwrote the first")
		}
		loaded, err = x.Backend.GetTask(x.Context, task.ID)
		requireOK(t, err)
		if loaded.DocumentationPolicy.BaseSHA != policy.BaseSHA || loaded.DocumentationPolicy.Paths[0] != policy.Paths[0] {
			t.Fatalf("pin changed: %+v", loaded.DocumentationPolicy)
		}
	})

	t.Run("off_pin_and_validation", func(t *testing.T) {
		task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "off", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		if _, err := x.Backend.PinTaskDocumentationPolicy(x.Context, task.ID, core.DocumentationPolicy{}); err == nil {
			t.Fatal("invalid off reason accepted")
		}
		pinned, err := x.Backend.PinTaskDocumentationPolicy(x.Context, task.ID, core.DocumentationPolicy{OffReason: store.DocumentationOffAbsent})
		requireOK(t, err)
		if !pinned {
			t.Fatal("off pin was not recorded")
		}
		loaded, err := x.Backend.GetTask(x.Context, task.ID)
		requireOK(t, err)
		if loaded.DocumentationPolicy == nil || loaded.DocumentationPolicy.Enabled || loaded.DocumentationPolicy.OffReason != store.DocumentationOffAbsent {
			t.Fatalf("off policy=%+v", loaded.DocumentationPolicy)
		}
		if _, err := x.Backend.PinTaskDocumentationPolicy(x.Context, "missing-task", policy); err == nil {
			t.Fatal("pin for a missing task accepted")
		}
	})

	t.Run("evidence_per_head", func(t *testing.T) {
		task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "evidence", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		head := "00112233445566778899aabbccddeeff00112233"
		requireOK(t, x.Backend.RecordDocumentationGateEvidence(x.Context, core.DocumentationGateEvidence{
			TaskID: task.ID, HeadSHA: head, MatchedPaths: []string{"docs/knowledge-base/b.md", "docs/knowledge-base/a.md"},
			NoneStatement: "docs: none", NoneReason: "internal refactor",
		}))
		got, ok, err := x.Backend.GetDocumentationGateEvidence(x.Context, task.ID, head)
		requireOK(t, err)
		if !ok || len(got.MatchedPaths) != 2 || got.MatchedPaths[0] != "docs/knowledge-base/a.md" || got.NoneReason != "internal refactor" || got.RecordedAt.IsZero() {
			t.Fatalf("evidence=%+v ok=%t", got, ok)
		}
		if _, ok, err := x.Backend.GetDocumentationGateEvidence(x.Context, task.ID, "ffffffffffffffffffffffffffffffffffffffff"); err != nil || ok {
			t.Fatalf("unknown head ok=%t err=%v", ok, err)
		}
		// A resubmission at the same head replaces the evidence.
		requireOK(t, x.Backend.RecordDocumentationGateEvidence(x.Context, core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: head, MatchedPaths: []string{"docs/knowledge-base/c.md"}}))
		got, ok, err = x.Backend.GetDocumentationGateEvidence(x.Context, task.ID, head)
		requireOK(t, err)
		if !ok || len(got.MatchedPaths) != 1 || got.MatchedPaths[0] != "docs/knowledge-base/c.md" {
			t.Fatalf("re-recorded evidence=%+v", got)
		}
		if err := x.Backend.RecordDocumentationGateEvidence(x.Context, core.DocumentationGateEvidence{TaskID: task.ID}); err == nil {
			t.Fatal("evidence without a head accepted")
		}
	})
}
