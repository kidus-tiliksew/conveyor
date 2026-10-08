package singlestore

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Durable evidence for listed titles on confirmation
// (req-document-operating-surfaces AC-6.1; component-document-corpus): a
// failing write inside the confirmation rolls the title back with it, and
// concurrent confirmations read the previous title from the locked row. The
// faults rename tables in this test binary's owned _test database and are
// reverted before the next fixture reset.

const documentTitleGoverns = "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```"

func newDocumentTitleStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st := integrationStore(t)
	workspace := "document-title-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "user:title-operator", Role: core.ActorHuman})
	if _, err := st.CreateWorkspace(ctx, workspace, "Document titles", &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

type documentTitleRow struct {
	title     string
	current   int
	confirmed bool
	events    int
}

func readDocumentTitleRow(t *testing.T, ctx context.Context, st *Store, table, versions, idColumn, id string, version int, events func() ([]core.Event, error)) documentTitleRow {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	var row documentTitleRow
	var current *int
	if err := st.db.QueryRowContext(ctx, `SELECT title, current_version FROM `+table+` WHERE workspace_id=? AND id=?`, workspace, id).Scan(&row.title, &current); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		row.current = *current
	}
	if err := st.db.QueryRowContext(ctx, `SELECT confirmed FROM `+versions+` WHERE workspace_id=? AND `+idColumn+`=? AND version=?`, workspace, id, version).Scan(&row.confirmed); err != nil {
		t.Fatal(err)
	}
	recorded, err := events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range recorded {
		if strings.HasSuffix(event.Kind, ".title_changed") {
			row.events++
		}
	}
	return row
}

// renameTable injects a fault and registers its reversal, so a failed
// assertion cannot leave the shared database without the table.
func renameTable(t *testing.T, ctx context.Context, st *Store, from, to string) func() {
	t.Helper()
	if _, err := st.db.ExecContext(ctx, "ALTER TABLE `"+from+"` RENAME `"+to+"`"); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if _, err := st.db.ExecContext(context.Background(), "ALTER TABLE `"+to+"` RENAME `"+from+"`"); err != nil {
			t.Errorf("restore table %s: %v", from, err)
		}
	}
	t.Cleanup(restore)
	return restore
}

func TestSingleStoreDocumentTitleRollsBackWithConfirmationIntegration(t *testing.T) {
	st, ctx := newDocumentTitleStore(t)
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Roll back together."}}
	tiers := []struct {
		name, table, versions, idColumn, id string
		create                              func() error
		propose                             func(string) (int, error)
		confirm                             func(int) error
		events                              func() ([]core.Event, error)
	}{
		{
			name: "requirement", table: "requirements", versions: "requirement_versions", idColumn: "requirement_id", id: "req-title-fault",
			create: func() error {
				_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-title-fault", Title: "Before fault"}, core.RequirementVersion{Content: "# Before fault", Origin: core.RequirementOriginOperator, Statements: statements})
				return err
			},
			propose: func(heading string) (int, error) {
				version, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: "req-title-fault", Content: "# " + heading, Origin: core.RequirementOriginOperator, Statements: statements})
				return version.Version, err
			},
			confirm: func(version int) error {
				_, _, err := st.ConfirmRequirementVersion(ctx, "req-title-fault", version)
				return err
			},
			events: func() ([]core.Event, error) { return st.ListRequirementEvents(ctx, "req-title-fault") },
		},
		{
			name: "system design", table: "system_designs", versions: "system_design_versions", idColumn: "document_id", id: "design-title-fault",
			create: func() error {
				_, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-title-fault", Title: "Before fault", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Before fault" + documentTitleGoverns, Origin: core.SystemDesignOriginOperator})
				return err
			},
			propose: func(heading string) (int, error) {
				version, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: "design-title-fault", Content: "# " + heading + documentTitleGoverns, Origin: core.SystemDesignOriginOperator})
				return version.Version, err
			},
			confirm: func(version int) error {
				_, _, err := st.ConfirmSystemDesignVersion(ctx, "design-title-fault", version)
				return err
			},
			events: func() ([]core.Event, error) { return st.ListSystemDesignEvents(ctx, "design-title-fault") },
		},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			if err := tier.create(); err != nil {
				t.Fatal(err)
			}
			if err := tier.confirm(1); err != nil {
				t.Fatal(err)
			}
			version, err := tier.propose("After fault")
			if err != nil {
				t.Fatal(err)
			}
			// SingleStore enforces no CHECK constraint, so each fault removes
			// a table the confirmation writes or reads after the title UPDATE:
			// the event table, then the decisions table that the
			// decision-sweep hook reads after the title event.
			for _, fault := range []struct{ name, table string }{{"event write", "events"}, {"later decision-sweep hook", "decisions"}} {
				restore := renameTable(t, ctx, st, fault.table, fault.table+"_title_fault")
				confirmErr := tier.confirm(version)
				restore()
				if confirmErr == nil {
					t.Fatalf("%s: confirmation succeeded despite the injected fault", fault.name)
				}
				if row := readDocumentTitleRow(t, ctx, st, tier.table, tier.versions, tier.idColumn, tier.id, version, tier.events); row != (documentTitleRow{title: "Before fault", current: 1}) {
					t.Fatalf("%s left partial state: %+v", fault.name, row)
				}
			}
			if err = tier.confirm(version); err != nil {
				t.Fatal(err)
			}
			if row := readDocumentTitleRow(t, ctx, st, tier.table, tier.versions, tier.idColumn, tier.id, version, tier.events); row != (documentTitleRow{title: "After fault", current: version, confirmed: true, events: 1}) {
				t.Fatalf("confirmation after clearing faults: %+v", row)
			}
		})
	}
}

func TestSingleStoreConcurrentConfirmationsChainTitlesIntegration(t *testing.T) {
	st, ctx := newDocumentTitleStore(t)
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Chain renames."}}
	for round := range 4 {
		for _, tier := range []string{"requirement", "system design"} {
			id := "title-race-" + core.NewTaskID()
			var confirm func(int) error
			var events func() ([]core.Event, error)
			var current func() (string, error)
			switch tier {
			case "requirement":
				id = "req-" + id
				if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Race zero " + id}, core.RequirementVersion{Content: "# Race zero " + id, Origin: core.RequirementOriginOperator, Statements: statements}); err != nil {
					t.Fatal(err)
				}
				for _, heading := range []string{"Race one", "Race two"} {
					if _, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: id, Content: "# " + heading, Origin: core.RequirementOriginOperator, Statements: statements}); err != nil {
						t.Fatal(err)
					}
				}
				confirm = func(version int) error { _, _, err := st.ConfirmRequirementVersion(ctx, id, version); return err }
				events = func() ([]core.Event, error) { return st.ListRequirementEvents(ctx, id) }
				current = func() (string, error) { r, err := st.GetRequirement(ctx, id); return r.Title, err }
			default:
				id = "design-" + id
				if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Race zero " + id, Category: "Architecture"}, core.SystemDesignVersion{Content: "# Race zero " + id + documentTitleGoverns, Origin: core.SystemDesignOriginOperator}); err != nil {
					t.Fatal(err)
				}
				for _, heading := range []string{"Race one", "Race two"} {
					if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: "# " + heading + documentTitleGoverns, Origin: core.SystemDesignOriginOperator}); err != nil {
						t.Fatal(err)
					}
				}
				confirm = func(version int) error { _, _, err := st.ConfirmSystemDesignVersion(ctx, id, version); return err }
				events = func() ([]core.Event, error) { return st.ListSystemDesignEvents(ctx, id) }
				current = func() (string, error) { d, err := st.GetSystemDesign(ctx, id); return d.Title, err }
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			for version := 1; version <= 3; version++ {
				wg.Add(1)
				go func(version int) {
					defer wg.Done()
					<-start
					_ = confirm(version)
				}(version)
			}
			close(start)
			wg.Wait()
			recorded, err := events()
			if err != nil {
				t.Fatal(err)
			}
			previous := "Race zero " + id
			for _, event := range recorded {
				if !strings.HasSuffix(event.Kind, ".title_changed") {
					continue
				}
				var payload struct {
					Old string `json:"old_title"`
					New string `json:"new_title"`
				}
				if err = json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Old != previous {
					t.Fatalf("round %d %s: title event old=%q, want %q; events=%+v", round, tier, payload.Old, previous, recorded)
				}
				previous = payload.New
			}
			title, err := current()
			if err != nil {
				t.Fatal(err)
			}
			if title != previous || title != "Race two" {
				t.Fatalf("round %d %s: title=%q chain end=%q", round, tier, title, previous)
			}
		}
	}
}
