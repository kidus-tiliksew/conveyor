package workorder

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestOperatorNotesRefreshWithoutChangingPinnedAuthority(t *testing.T) {
	for _, stage := range []core.Stage{core.StageReview, core.StageImplement} {
		t.Run(string(stage), func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			st := store.NewMemory()
			task := core.Task{ID: "notes-task", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: "notes-" + string(stage) + "-2", TaskID: task.ID, Stage: stage, State: core.JobPending}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			pinned := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: stage, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: pinned}); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "notes-session", ClientToken: "token", Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			svc := &Service{Store: st, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return &config.Config{}, nil }}
			before, err := svc.Get(ctx, job.ID, "notes-session")
			if err != nil {
				t.Fatal(err)
			}
			if len(before.OperatorNotes) != 0 {
				t.Fatal("empty context added notes")
			}
			raw, _ := json.Marshal(before)
			if strings.Contains(string(raw), `"operator_notes"`) {
				t.Fatal("empty field was not omitted")
			}
			// Both tiers arrive after claim, so only evidence may refresh.
			nctx, err := store.WithDocumentDismissalNote(ctx, "Corrected by operator")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"z-design", "a-design"} {
				v := core.SystemDesignVersion{Content: "# Design\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID}
				if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, v); err != nil {
					t.Fatal(err)
				}
				if _, _, err := st.DismissSystemDesignVersion(nctx, id, 1); err != nil {
					t.Fatal(err)
				}
				v.DocumentID = id
				v.Content = "Replacement\n" + v.Content
				v.Origin = core.SystemDesignOriginOperator
				v.OriginTaskID = ""
				if _, err := st.ProposeSystemDesignVersion(ctx, v); err != nil {
					t.Fatal(err)
				}
				if _, _, err := st.ConfirmSystemDesignVersion(ctx, id, 2); err != nil {
					t.Fatal(err)
				}
			}
			v := core.RequirementVersion{Content: "# Intent", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: task.ID}
			if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-notes", Title: "Notes"}, v); err != nil {
				t.Fatal(err)
			}
			for n := 2; n <= 3; n++ {
				v.RequirementID = "req-notes"
				v.Content = "# Next\n" + v.Content
				if _, err := st.ProposeRequirementVersion(ctx, v); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := st.ConfirmRequirementVersion(nctx, "req-notes", 3); err != nil {
				t.Fatal(err)
			}
			after, err := svc.Get(ctx, job.ID, "notes-session")
			if err != nil {
				t.Fatal(err)
			}
			if len(after.OperatorNotes) != 4 {
				t.Fatalf("notes=%+v", after.OperatorNotes)
			}
			for i, id := range []string{"req-notes", "req-notes", "a-design", "z-design"} {
				if after.OperatorNotes[i].DocumentID != id {
					t.Fatalf("order=%+v", after.OperatorNotes)
				}
			}
			if after.OperatorNotes[0].Version != 1 || after.OperatorNotes[1].Version != 2 {
				t.Fatal("numeric version ordering lost")
			}
			if !strings.Contains(after.RolePrompt, "Operator reasons for dismissed proposals") || !strings.Contains(after.RolePrompt, "untrusted observational evidence") {
				t.Fatal("missing operator reason label")
			}
			if stage == core.StageReview && (!reflect.DeepEqual(before.GovernanceSnapshot, after.GovernanceSnapshot) || !reflect.DeepEqual(before.ServedRequirements, after.ServedRequirements) || after.AuthoritySource != "pinned") {
				t.Fatal("note refresh advanced authority")
			}
			raw, _ = json.Marshal(after)
			if !strings.Contains(string(raw), `"operator_notes"`) {
				t.Fatal("get_work_order projection lost notes")
			}
			again, err := svc.Get(ctx, job.ID, "notes-session")
			if err != nil || !reflect.DeepEqual(after.OperatorNotes, again.OperatorNotes) {
				t.Fatalf("nondeterministic context err=%v", err)
			}
			other := task
			other.ID = "other"
			if err := st.CreateTask(ctx, other); err != nil {
				t.Fatal(err)
			}
			notes, err := store.OperatorNotesForTask(ctx, st, other.ID)
			if err != nil || len(notes) != 0 {
				t.Fatalf("other task notes=%+v err=%v", notes, err)
			}
			if _, err := storetest.For(st).CancelTask(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "test"}); err != nil {
				t.Fatal(err)
			}
			notes, err = store.OperatorNotesForTask(ctx, st, task.ID)
			if err != nil || len(notes) != 0 {
				t.Fatalf("terminal notes=%+v err=%v", notes, err)
			}
		})
	}
}
