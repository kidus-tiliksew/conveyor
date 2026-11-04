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

// A dismissal note stays on the dismissed version's history and never reaches a
// work-order context or prompt, on a read before or after the dismissal
// (req-260810-23b69f REQ-5, AC-5.1).
func TestDismissalNotesStayOutOfWorkOrderContext(t *testing.T) {
	for _, stage := range []core.Stage{core.StageReview, core.StageImplement} {
		t.Run(string(stage), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
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
			// Both tiers are decided after claim.
			nctx, err := store.WithDocumentDismissalNote(ctx, "Corrected by operator")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"z-design", "a-design"} {
				v := core.SystemDesignVersion{Content: "# Design\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID}
				if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, v); err != nil {
					t.Fatal(err)
				}
				v.DocumentID = id
				v.Content = "Replacement\n" + v.Content
				v.Origin = core.SystemDesignOriginOperator
				v.OriginTaskID = ""
				if _, err := st.ProposeSystemDesignVersion(ctx, v); err != nil {
					t.Fatal(err)
				}
				if _, _, err := st.DismissSystemDesignVersion(nctx, id, 1); err != nil {
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
			raw, err := json.Marshal(after)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"operator_notes", "Corrected by operator"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("work-order context carries %q: %s", forbidden, raw)
				}
			}
			if strings.Contains(after.RolePrompt, "Operator reasons for dismissed proposals") {
				t.Fatal("role prompt carries the retired dismissal-note block")
			}
			if stage == core.StageReview && (!reflect.DeepEqual(before.GovernanceSnapshot, after.GovernanceSnapshot) || !reflect.DeepEqual(before.ServedRequirements, after.ServedRequirements) || after.AuthoritySource != "pinned") {
				t.Fatal("a dismissal advanced pinned authority")
			}
			// The note stays on each dismissed version's history, for a live and
			// a terminal task alike.
			requireHistory := func() {
				t.Helper()
				for _, id := range []string{"z-design", "a-design"} {
					version, err := st.GetSystemDesignVersion(ctx, id, 1)
					if err != nil || !version.Dismissed || version.DismissalNote != "Corrected by operator" || version.OriginTaskID != task.ID {
						t.Fatalf("%s history=%+v err=%v", id, version, err)
					}
				}
				for _, n := range []int{1, 2} {
					version, err := st.GetRequirementVersion(ctx, "req-notes", n)
					if err != nil || !version.Retired || version.DismissalNote != "Corrected by operator" || version.OriginTaskID != task.ID {
						t.Fatalf("req-notes v%d history=%+v err=%v", n, version, err)
					}
				}
			}
			requireHistory()
			if _, err := storetest.For(st).CancelTask(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "test"}); err != nil {
				t.Fatal(err)
			}
			requireHistory()
		})
	}
}

// A review or verify order pinned before DEC-102 may hold proposal evidence and
// resolution notes. Reading it presents neither, and claiming it leaves the
// stored pin and its digest exactly as recorded (DEC-102(6)).
func TestOlderPinKeepsItsDigestAndPresentsNoRetiredEvidence(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "older-pin", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	older := &core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "design-older", Title: "Older", Version: 3, Category: "Architecture", Content: "# Older"}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "design-evidence", Version: 4, ProposalEventID: 42, OriginTaskID: task.ID}},
		ResolutionNotes:        []string{"retained resolution note"},
	}
	digest, _ := store.EncodeGovernanceSnapshot(older)
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: older}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}}
	svc := &Service{Store: st, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	claimed, err := svc.Claim(ctx, job.ID, core.WorkOrderClaim{SessionID: "older-session", ClientToken: "older-token", ClaimantID: "reviewer", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.GovernanceSnapshotDigest != digest || len(claimed.GovernanceSnapshot.PendingDesignProposals) != 1 || len(claimed.GovernanceSnapshot.ResolutionNotes) != 1 {
		t.Fatalf("claim rewrote the older pin: digest=%s want %s snapshot=%+v", claimed.GovernanceSnapshotDigest, digest, claimed.GovernanceSnapshot)
	}
	contract, err := svc.Get(ctx, job.ID, "older-session")
	if err != nil {
		t.Fatal(err)
	}
	if contract.GovernanceSnapshot == nil || len(contract.GovernanceSnapshot.Designs) != 1 || contract.GovernanceSnapshot.Designs[0].ID != "design-older" {
		t.Fatalf("context lost the pinned authority: %+v", contract.GovernanceSnapshot)
	}
	if len(contract.GovernanceSnapshot.PendingDesignProposals) != 0 || len(contract.GovernanceSnapshot.ResolutionNotes) != 0 {
		t.Fatalf("context presents retired evidence: %+v", contract.GovernanceSnapshot)
	}
	raw, err := json.Marshal(contract.Outbound())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"design-evidence", "retained resolution note", "proposal_event_id"} {
		if strings.Contains(string(raw), forbidden) || strings.Contains(contract.RolePrompt, forbidden) {
			t.Fatalf("context carries retired evidence %q: %s", forbidden, raw)
		}
	}
	stored, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || stored.GovernanceSnapshotDigest != digest {
		t.Fatalf("stored digest=%s want %s err=%v", stored.GovernanceSnapshotDigest, digest, err)
	}
}
