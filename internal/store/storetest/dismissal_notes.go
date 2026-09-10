package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runDismissalNotesConformance is registered under VersionDismissal for every
// backend. It proves direct and multi-version history, events and provenance.
func runDismissalNotesConformance(t *testing.T, factory RequirementFactory) {
	for _, tier := range []string{"requirement", "system_design"} {
		for _, note := range []string{"", "  Operator reason é日  "} {
			t.Run(tier+fmt.Sprintf("/note=%t", note != ""), func(t *testing.T) {
				f := factory(t, requirementConformanceRepos)
				st := f.Store
				ctx := store.WithActor(f.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
				noteCtx, err := store.WithDocumentDismissalNote(ctx, note)
				if err != nil {
					t.Fatal(err)
				}
				want := store.DocumentDismissalNote(noteCtx)
				taskID := core.NewTaskID()
				if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: f.Workspace, Repo: "conveyor", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				id := "doc-" + core.NewTaskID()
				var propose func(int) error
				var dismiss func(context.Context, int) error
				var confirm func(context.Context, int) error
				var check func(int, string)
				var events func() ([]core.Event, error)
				if tier == "requirement" {
					content := "# Reason\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Preserve intent.\n```"
					v := core.RequirementVersion{Content: content, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: taskID, DismissalNote: "forged"}
					if _, created, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, v); err != nil {
						t.Fatal(err)
					} else if created.DismissalNote != "" {
						t.Fatal("proposal injected note")
					}
					propose = func(n int) error {
						v.RequirementID = id
						v.Content = fmt.Sprintf("# Revision %d\n%s", n, content)
						_, err := st.ProposeRequirementVersion(ctx, v)
						return err
					}
					dismiss = func(c context.Context, n int) error {
						_, v, err := st.DismissRequirementVersion(c, id, n)
						if err == nil && v.DismissalNote != store.DocumentDismissalNote(c) {
							t.Fatal("direct response lost note")
						}
						return err
					}
					confirm = func(c context.Context, n int) error { _, _, err := st.ConfirmRequirementVersion(c, id, n); return err }
					check = func(n int, want string) {
						versions, err := st.ListRequirementVersions(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						v, err := st.GetRequirementVersion(ctx, id, n)
						if err != nil {
							t.Fatal(err)
						}
						if v.DismissalNote != want || versions[n-1].DismissalNote != want || !v.Retired || v.RetiredAt.IsZero() || v.RetiredBy != requirementConformanceActor || !reflect.DeepEqual(v.Statements, []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}}) {
							t.Fatalf("history=%+v", v)
						}
						expected := content
						if n > 1 {
							expected = fmt.Sprintf("# Revision %d\n%s", n, content)
						}
						if v.Content != expected {
							t.Fatal("content changed")
						}
						raw, _ := json.Marshal(v)
						var obj map[string]any
						_ = json.Unmarshal(raw, &obj)
						_, present := obj["dismissal_note"]
						if present != (want != "") {
							t.Fatalf("JSON note presence=%v", present)
						}
					}
					events = func() ([]core.Event, error) { return st.ListRequirementEvents(ctx, id) }
				} else {
					content := "# Reason\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
					v := core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID, DismissalNote: "forged"}
					if _, created, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, v); err != nil {
						t.Fatal(err)
					} else if created.DismissalNote != "" {
						t.Fatal("proposal injected note")
					}
					propose = func(n int) error {
						v.DocumentID = id
						v.Content = fmt.Sprintf("Revision %d\n%s", n, content)
						_, err := st.ProposeSystemDesignVersion(ctx, v)
						return err
					}
					dismiss = func(c context.Context, n int) error {
						_, v, err := st.DismissSystemDesignVersion(c, id, n)
						if err == nil && v.DismissalNote != store.DocumentDismissalNote(c) {
							t.Fatal("direct response lost note")
						}
						return err
					}
					confirm = func(c context.Context, n int) error { _, _, err := st.ConfirmSystemDesignVersion(c, id, n); return err }
					check = func(n int, want string) {
						versions, err := st.ListSystemDesignVersions(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						v, err := st.GetSystemDesignVersion(ctx, id, n)
						if err != nil {
							t.Fatal(err)
						}
						if v.DismissalNote != want || versions[n-1].DismissalNote != want || !v.Dismissed || v.DismissedAt.IsZero() || v.DismissedBy != requirementConformanceActor {
							t.Fatalf("history=%+v", v)
						}
						expected := content
						if n > 1 {
							expected = fmt.Sprintf("Revision %d\n%s", n, content)
						}
						if v.Content != expected {
							t.Fatal("content changed")
						}
						raw, _ := json.Marshal(v)
						var obj map[string]any
						_ = json.Unmarshal(raw, &obj)
						_, present := obj["dismissal_note"]
						if present != (want != "") {
							t.Fatalf("JSON note presence=%v", present)
						}
					}
					events = func() ([]core.Event, error) { return st.ListSystemDesignEvents(ctx, id) }
				}
				if err := dismiss(noteCtx, 1); err != nil {
					t.Fatal(err)
				}
				for n := 2; n <= 4; n++ {
					if err := propose(n); err != nil {
						t.Fatal(err)
					}
				}
				if err := confirm(noteCtx, 4); err != nil {
					t.Fatal(err)
				}
				for n := 1; n <= 3; n++ {
					check(n, want)
				}
				if err := confirm(ctx, 1); err == nil {
					t.Fatal("dismissed version confirmed")
				}
				// Later confirmations must not replace previously recorded reasons.
				if err := propose(5); err != nil {
					t.Fatal(err)
				}
				if err := confirm(ctx, 5); err != nil {
					t.Fatal(err)
				}
				for n := 1; n <= 3; n++ {
					check(n, want)
				}
				es, err := events()
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, e := range es {
					if e.Kind != "requirement.version_dismissed" && e.Kind != "requirement.version_retired" && e.Kind != "system_design.version_dismissed" {
						continue
					}
					count++
					var payload map[string]any
					if err := json.Unmarshal(e.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					value, present := payload["note"]
					if present != (want != "") || (present && value != want) {
						t.Fatalf("event=%s", e.Payload)
					}
				}
				if count != 3 {
					t.Fatalf("dismissal events=%d", count)
				}
				notes, err := st.ListDocumentOperatorNotesForTask(ctx, taskID)
				if err != nil {
					t.Fatal(err)
				}
				expected := 0
				if want != "" {
					expected = 3
				}
				if len(notes) != expected {
					t.Fatalf("operator notes=%+v", notes)
				}
				for i, n := range notes {
					if n.DocumentID != id || n.Version != i+1 || n.Tier != tier || n.Note != want || n.DismissedAt.IsZero() {
						t.Fatalf("note=%+v", n)
					}
				}
				for _, c := range []context.Context{ctx, store.WithWorkspace(ctx, "unrelated")} {
					otherTask := taskID
					if c == ctx {
						otherTask = "unrelated"
					}
					other, err := st.ListDocumentOperatorNotesForTask(c, otherTask)
					if err != nil || len(other) != 0 {
						t.Fatalf("unrelated notes=%+v err=%v", other, err)
					}
				}
				projected, err := store.OperatorNotesForTask(ctx, st, taskID)
				if err != nil || len(projected) != expected {
					t.Fatalf("projected=%+v err=%v", projected, err)
				}
				if _, err := For(st).CancelTask(ctx, core.Intervention{TaskID: taskID, Action: core.InterventionCancel, ReasonCode: "terminal-notes-test"}); err != nil {
					t.Fatal(err)
				}
				projected, err = store.OperatorNotesForTask(ctx, st, taskID)
				if err != nil || len(projected) != 0 {
					t.Fatalf("terminal notes=%+v err=%v", projected, err)
				}
				for n := 1; n <= 3; n++ {
					check(n, want)
				}
			})
		}
	}
}
