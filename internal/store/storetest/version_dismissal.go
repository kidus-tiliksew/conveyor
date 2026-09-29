package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	t.Run("dismissal archives", func(t *testing.T) { runDismissalArchiveConformance(t, factory) })
	t.Run("operator dismissal notes", func(t *testing.T) { runDismissalNotesConformance(t, factory) })
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
		if _, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Another pending proposal", Origin: core.RequirementOriginOperator, Statements: pending.Statements}); err != nil {
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
			RequirementID: requirement.ID, Content: "# Superseded pending requirement", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep dismissed history current."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		newerRequirement, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: "# Newer requirement", Origin: core.RequirementOriginOperator,
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
		if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: strings.Replace(designContent, "Direct dismissal", "Another pending proposal", 1), Origin: core.SystemDesignOriginOperator}); err != nil {
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

// Every scenario runs on all registered backends, with no table-specific reads.
func runDismissalArchiveConformance(t *testing.T, factory RequirementFactory) {
	for _, tier := range []string{"requirement", "system_design"} {
		for _, scenario := range []string{"only", "two", "confirmed"} {
			t.Run(tier+"/"+scenario, func(t *testing.T) {
				f := factory(t, requirementConformanceRepos)
				ctx := store.WithActor(f.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
				noteCtx, err := store.WithDocumentDismissalNote(ctx, "Archive this proposal é日")
				if err != nil {
					t.Fatal(err)
				}
				st, id := f.Store, "archive-"+core.NewTaskID()
				var propose func(int) error
				var dismiss func(context.Context, int) (map[string]any, error)
				var confirm func(int) error
				var get func() (map[string]any, error)
				var events func() ([]core.Event, error)
				var archive, restore func() error
				var list func(bool) (bool, error)
				projection := func(v any, err error) (map[string]any, error) {
					if err != nil {
						return nil, err
					}
					raw, err := json.Marshal(v)
					if err != nil {
						return nil, err
					}
					var out map[string]any
					err = json.Unmarshal(raw, &out)
					return out, err
				}
				if tier == "requirement" {
					version := func(n int) core.RequirementVersion {
						return core.RequirementVersion{RequirementID: id, Content: fmt.Sprintf("# Proposal %d", n), Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retain history."}}}
					}
					_, _, err = st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, version(1))
					propose = func(n int) error { _, e := st.ProposeRequirementVersion(ctx, version(n)); return e }
					dismiss = func(c context.Context, n int) (map[string]any, error) {
						d, _, e := st.DismissRequirementVersion(c, id, n)
						return projection(d, e)
					}
					confirm = func(n int) error { _, _, e := st.ConfirmRequirementVersion(ctx, id, n); return e }
					get = func() (map[string]any, error) { d, e := st.GetRequirement(ctx, id); return projection(d, e) }
					events = func() ([]core.Event, error) { return st.ListRequirementEvents(ctx, id) }
					restore = func() error { return st.RestoreRequirement(ctx, id, requirementConformanceActor) }
					archive = func() error { return st.ArchiveRequirement(ctx, id, requirementConformanceActor, nil) }
					list = func(include bool) (bool, error) {
						ds, e := st.ListRequirements(ctx, include)
						for _, d := range ds {
							if d.ID == id {
								return true, e
							}
						}
						return false, e
					}
				} else {
					version := func(n int) core.SystemDesignVersion {
						return core.SystemDesignVersion{DocumentID: id, Content: fmt.Sprintf("# Proposal %d\n\n```conveyor:governs\n- repo: conveyor\n  paths: [internal/**]\n```", n), Origin: core.SystemDesignOriginOperator}
					}
					_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, version(1))
					propose = func(n int) error { _, e := st.ProposeSystemDesignVersion(ctx, version(n)); return e }
					dismiss = func(c context.Context, n int) (map[string]any, error) {
						d, _, e := st.DismissSystemDesignVersion(c, id, n)
						return projection(d, e)
					}
					confirm = func(n int) error { _, _, e := st.ConfirmSystemDesignVersion(ctx, id, n); return e }
					get = func() (map[string]any, error) { d, e := st.GetSystemDesign(ctx, id); return projection(d, e) }
					events = func() ([]core.Event, error) { return st.ListSystemDesignEvents(ctx, id) }
					restore = func() error { return st.RestoreSystemDesign(ctx, id, requirementConformanceActor) }
					archive = func() error { return st.ArchiveSystemDesign(ctx, id, requirementConformanceActor, nil) }
					list = func(include bool) (bool, error) {
						ds, e := st.ListSystemDesigns(ctx, include)
						for _, d := range ds {
							if d.ID == id {
								return true, e
							}
						}
						return false, e
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				n := 1
				if scenario == "confirmed" {
					if err := confirm(1); err != nil {
						t.Fatal(err)
					}
				}
				if scenario != "only" {
					if err := propose(2); err != nil {
						t.Fatal(err)
					}
					n = 2
				}
				if scenario == "two" {
					d, e := dismiss(ctx, 1)
					if e != nil || d["archived"] != false {
						t.Fatalf("first dismissal=%v err=%v", d, e)
					}
					es, e := events()
					if e != nil || es[len(es)-1].Kind != tier+".version_dismissed" {
						t.Fatalf("first events=%v err=%v", es, e)
					}
					if found, e := list(false); e != nil || !found {
						t.Fatalf("pending document omitted: %v", e)
					}
				}
				d, err := dismiss(noteCtx, n)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "confirmed" {
					if d["archived"] != false || d["archive_reason"] != nil || d["current_version"] != float64(1) {
						t.Fatalf("confirmed document changed=%v", d)
					}
					if err := archive(); err != nil {
						t.Fatal(err)
					}
					d, err = get()
					if err != nil || d["archive_reason"] != nil || d["archive_note"] != nil {
						t.Fatalf("operator archive=%v err=%v", d, err)
					}
					es, e := events()
					if e != nil {
						t.Fatal(e)
					}
					var payload map[string]any
					if e = json.Unmarshal(es[len(es)-1].Payload, &payload); e != nil {
						t.Fatal(e)
					}
					if payload["reason"] != nil || payload["note"] != nil {
						t.Fatalf("operator payload=%v", payload)
					}
					if err := restore(); err != nil {
						t.Fatal(err)
					}
					d, err = get()
					if err != nil || d["archived"] != false || d["current_version"] != float64(1) {
						t.Fatalf("normal restore=%v err=%v", d, err)
					}
					return
				}
				check := func(d map[string]any) {
					t.Helper()
					if d["archived"] != true || d["archive_reason"] != core.ArchiveReasonOnlyProposalDismissed || d["archive_note"] != "Archive this proposal é日" || d["archived_by"] != requirementConformanceActor || d["current_version"] != nil {
						t.Fatalf("archive projection=%v", d)
					}
					if ids, ok := d["superseded_by"].([]any); ok && len(ids) != 0 {
						t.Fatalf("successors=%v", ids)
					}
				}
				check(d)
				d, err = get()
				if err != nil {
					t.Fatal(err)
				}
				check(d)
				for _, include := range []bool{false, true} {
					found, e := list(include)
					if e != nil || found != include {
						t.Fatalf("list include=%v found=%v err=%v", include, found, e)
					}
				}
				es, err := events()
				if err != nil {
					t.Fatal(err)
				}
				if len(es) < 2 || es[len(es)-2].Kind != tier+".version_dismissed" || es[len(es)-1].Kind != tier+".archived" || es[len(es)-2].ID >= es[len(es)-1].ID {
					t.Fatalf("event order=%v", es)
				}
				e := es[len(es)-1]
				var payload map[string]any
				if err = json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if e.ActorID != requirementConformanceActor || payload["actor"] != requirementConformanceActor || payload["reason"] != core.ArchiveReasonOnlyProposalDismissed || payload["note"] != "Archive this proposal é日" || len(payload["superseded_by"].([]any)) != 0 {
					t.Fatalf("archive event=%+v", e)
				}
				var conflict *store.DocumentRestoreConflict
				if err = restore(); !errors.As(err, &conflict) || conflict.DocumentID != id || !strings.Contains(err.Error(), "no confirmed version to restore to") {
					t.Fatalf("restore=%v", err)
				}
				after, err := events()
				if err != nil || len(after) != len(es) {
					t.Fatalf("restore mutated events: %v", err)
				}
				d, err = get()
				if err != nil {
					t.Fatal(err)
				}
				check(d)
				if err = propose(3); err == nil {
					t.Fatal("archived document accepted proposal")
				}
			})
		}
	}
}
