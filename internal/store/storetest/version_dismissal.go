package storetest

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunVersionDismissalConformance verifies the direct-dismissal contract against
// both store implementations, including immutable history, terminal conflicts,
// actor attribution, audit events, and post-dismissal proposal identity.
func RunVersionDismissalConformance(t *testing.T, factory RequirementFactory) {
	t.Helper()
	t.Run("requirement and system design versions dismiss directly", func(t *testing.T) {
		fixture := factory(t, requirementConformanceRepos)
		ctx := store.WithActor(fixture.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
		st := fixture.Store

		requirementContent := "# Direct dismissal\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep dismissed history.\n```"
		requirement, pending, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Direct requirement dismissal"},
			core.RequirementVersion{Content: requirementContent, Origin: core.RequirementOriginImplementation, OriginTaskID: core.NewTaskID(), Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep dismissed history."}}})
		if err != nil {
			t.Fatal(err)
		}
		unchanged, dismissed, err := st.DismissRequirementVersion(ctx, requirement.ID, pending.Version)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.CurrentVersion != 0 || !dismissed.Retired || dismissed.RetiredBy != requirementConformanceActor || dismissed.RetiredAt.IsZero() || dismissed.RetiredByVersion != 0 || dismissed.Content != requirementContent {
			t.Fatalf("requirement=%+v dismissed=%+v", unchanged, dismissed)
		}
		events, err := st.ListRequirementEvents(ctx, requirement.ID)
		if err != nil {
			t.Fatal(err)
		}
		if event := events[len(events)-1]; event.Kind != "requirement.version_dismissed" || !strings.Contains(string(event.Payload), requirementConformanceActor) {
			t.Fatalf("dismiss event=%+v", event)
		}
		reproposed, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: requirementContent, Origin: core.RequirementOriginImplementation,
			OriginTaskID: pending.OriginTaskID, Statements: pending.Statements,
		})
		if err != nil || reproposed.Version == pending.Version || reproposed.Deduplicated {
			t.Fatalf("reproposal=%+v err=%v", reproposed, err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, reproposed.Version); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.DismissRequirementVersion(ctx, requirement.ID, reproposed.Version); err == nil {
			t.Fatal("confirmed requirement version was dismissed")
		} else {
			var conflict *store.RequirementVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalConfirmed {
				t.Fatalf("confirmed conflict=%T %v", err, err)
			}
		}
		if _, _, err = st.DismissRequirementVersion(ctx, requirement.ID, pending.Version); err == nil {
			t.Fatal("dismissed requirement version was dismissed twice")
		} else {
			var conflict *store.RequirementVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalDismissed {
				t.Fatalf("dismissed conflict=%T %v", err, err)
			}
		}
		supersededRequirement, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: "Superseded pending requirement", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep dismissed history current."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		newerRequirement, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: "Newer requirement", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep dismissed history current and explicit."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, newerRequirement.Version); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.DismissRequirementVersion(ctx, requirement.ID, supersededRequirement.Version); err == nil {
			t.Fatal("superseded requirement version was dismissed")
		} else {
			var conflict *store.RequirementVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalSuperseded || conflict.SupersededBy != newerRequirement.Version {
				t.Fatalf("superseded conflict=%T %+v", err, err)
			}
		}

		designContent := "# Direct dismissal\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
		design, designPending, err := st.CreateSystemDesign(ctx,
			core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Direct design dismissal", Category: "Architecture"},
			core.SystemDesignVersion{Content: designContent, Origin: core.SystemDesignOriginImplementation, OriginTaskID: core.NewTaskID()})
		if err != nil {
			t.Fatal(err)
		}
		designUnchanged, designDismissed, err := st.DismissSystemDesignVersion(ctx, design.ID, designPending.Version)
		if err != nil {
			t.Fatal(err)
		}
		if designUnchanged.CurrentVersion != 0 || !designDismissed.Dismissed || designDismissed.DismissedBy != requirementConformanceActor || designDismissed.DismissedAt.IsZero() || designDismissed.Content != designContent {
			t.Fatalf("design=%+v dismissed=%+v", designUnchanged, designDismissed)
		}
		designEvents, err := st.ListSystemDesignEvents(ctx, design.ID)
		if err != nil {
			t.Fatal(err)
		}
		if event := designEvents[len(designEvents)-1]; event.Kind != "system_design.version_dismissed" || !strings.Contains(string(event.Payload), requirementConformanceActor) || strings.Contains(string(event.Payload), "confirmed_version") {
			t.Fatalf("design dismiss event=%+v", event)
		}
		if _, _, err = st.DismissSystemDesignVersion(ctx, design.ID, designPending.Version); err == nil {
			t.Fatal("dismissed system design version was dismissed twice")
		} else {
			var conflict *store.SystemDesignVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalDismissed {
				t.Fatalf("dismissed design conflict=%T %v", err, err)
			}
		}
		designReproposal, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
			DocumentID: design.ID, Content: designContent, Origin: core.SystemDesignOriginImplementation, OriginTaskID: designPending.OriginTaskID,
		})
		if err != nil || designReproposal.Version == designPending.Version || designReproposal.Deduplicated {
			t.Fatalf("design reproposal=%+v err=%v", designReproposal, err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designReproposal.Version); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.DismissSystemDesignVersion(ctx, design.ID, designReproposal.Version); err == nil {
			t.Fatal("confirmed system design version was dismissed")
		} else {
			var conflict *store.SystemDesignVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalConfirmed {
				t.Fatalf("confirmed design conflict=%T %v", err, err)
			}
		}
		supersededDesign, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
			DocumentID: design.ID, Content: strings.Replace(designContent, "# Direct dismissal", "# Superseded design", 1), Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		newerDesign, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
			DocumentID: design.ID, Content: strings.Replace(designContent, "# Direct dismissal", "# Newer design", 1), Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, newerDesign.Version); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.DismissSystemDesignVersion(ctx, design.ID, supersededDesign.Version); err == nil {
			t.Fatal("superseded system design version was dismissed")
		} else {
			var conflict *store.SystemDesignVersionDismissalConflict
			if !errors.As(err, &conflict) || conflict.Reason != store.VersionDismissalSuperseded || conflict.SupersededBy != newerDesign.Version {
				t.Fatalf("superseded design conflict=%T %+v", err, err)
			}
		}
	})
}
