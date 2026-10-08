package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Durable evidence for listed titles on confirmation
// (req-document-operating-surfaces AC-6.1; component-document-corpus): a
// failing write inside the confirmation rolls the title back with it, and
// concurrent confirmations read the previous title from the locked row.

const documentTitleGoverns = "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```"

// newDocumentTitleStore migrates a private schema so fault injection never
// touches the shared integration schema, and drops it on cleanup.
func newDocumentTitleStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "document_title_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, dropErr := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); dropErr != nil {
			t.Errorf("drop schema %s: %v", schema, dropErr)
		}
	})
	if err = migrateControlPlaneToVersion(t.Context(), pool, 0); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "document-title-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "user:title-operator", Role: core.ActorHuman})
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
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

func readDocumentTitleRow(t *testing.T, ctx context.Context, st *Store, table, versions, idColumn, id string, version int) documentTitleRow {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	var row documentTitleRow
	var current *int
	if err := st.pool.QueryRow(ctx, `SELECT title, current_version FROM `+table+` WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&row.title, &current); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		row.current = *current
	}
	if err := st.pool.QueryRow(ctx, `SELECT confirmed FROM `+versions+` WHERE workspace_id=$1 AND `+idColumn+`=$2 AND version=$3`, workspace, id, version).Scan(&row.confirmed); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind LIKE '%.title_changed' AND (payload_json->>'requirement_id'=$2 OR payload_json->>'document_id'=$2)`, workspace, id).Scan(&row.events); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDocumentTitleRollsBackWithConfirmationIntegration(t *testing.T) {
	st, ctx := newDocumentTitleStore(t)
	tiers := []struct {
		name, table, versions, idColumn, id string
		create                              func() error
		propose                             func(string) (int, error)
		confirm                             func(int) error
	}{
		{
			name: "requirement", table: "requirements", versions: "requirement_versions", idColumn: "requirement_id", id: "req-title-fault",
			create: func() error {
				_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-title-fault", Title: "Before fault"}, core.RequirementVersion{Content: "# Before fault", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Roll back together."}}})
				return err
			},
			propose: func(heading string) (int, error) {
				version, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: "req-title-fault", Content: "# " + heading, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Roll back together."}}})
				return version.Version, err
			},
			confirm: func(version int) error {
				_, _, err := st.ConfirmRequirementVersion(ctx, "req-title-fault", version)
				return err
			},
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
			for _, fault := range []struct{ name, inject, clear string }{
				// The title-change event write itself is refused.
				{"title event write", `ALTER TABLE events ADD CONSTRAINT document_title_fault CHECK (kind NOT LIKE '%.title_changed') NOT VALID`, `ALTER TABLE events DROP CONSTRAINT document_title_fault`},
				// A hook that runs after the title event fails.
				{"later decision-sweep hook", `ALTER TABLE decisions RENAME TO decisions_title_fault`, `ALTER TABLE decisions_title_fault RENAME TO decisions`},
			} {
				if _, err = st.pool.Exec(ctx, fault.inject); err != nil {
					t.Fatal(err)
				}
				confirmErr := tier.confirm(version)
				if _, err = st.pool.Exec(ctx, fault.clear); err != nil {
					t.Fatal(err)
				}
				if confirmErr == nil {
					t.Fatalf("%s: confirmation succeeded despite the injected fault", fault.name)
				}
				if row := readDocumentTitleRow(t, ctx, st, tier.table, tier.versions, tier.idColumn, tier.id, version); row != (documentTitleRow{title: "Before fault", current: 1}) {
					t.Fatalf("%s left partial state: %+v", fault.name, row)
				}
			}
			if err = tier.confirm(version); err != nil {
				t.Fatal(err)
			}
			if row := readDocumentTitleRow(t, ctx, st, tier.table, tier.versions, tier.idColumn, tier.id, version); row != (documentTitleRow{title: "After fault", current: version, confirmed: true, events: 1}) {
				t.Fatalf("confirmation after clearing faults: %+v", row)
			}
		})
	}
}

func TestConcurrentConfirmationsChainTitlesIntegration(t *testing.T) {
	st, ctx := newDocumentTitleStore(t)
	for round := range 4 {
		for _, tier := range []string{"requirement", "system design"} {
			id := "title-race-" + core.NewTaskID()
			var confirm func(int) error
			var events func() ([]core.Event, error)
			var current func() (string, error)
			switch tier {
			case "requirement":
				id = "req-" + id
				statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Chain renames."}}
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
			// Versions 1, 2, and 3 race; the row lock serializes them, and
			// a version below the winner's is refused as superseded.
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
			assertTitleChain(t, round, tier, "Race zero "+id, events, current)
		}
	}
}

// assertTitleChain requires every title event to start from the title the
// previous one ended with, and the final title to equal the last new title.
func assertTitleChain(t *testing.T, round int, tier, created string, events func() ([]core.Event, error), current func() (string, error)) {
	t.Helper()
	recorded, err := events()
	if err != nil {
		t.Fatal(err)
	}
	previous := created
	confirmedVersions := 0
	for _, event := range recorded {
		switch {
		case strings.HasSuffix(event.Kind, ".version_confirmed"):
			confirmedVersions++
		case strings.HasSuffix(event.Kind, ".title_changed"):
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
	}
	title, err := current()
	if err != nil {
		t.Fatal(err)
	}
	if title != previous || title != "Race two" || confirmedVersions == 0 {
		t.Fatalf("round %d %s: title=%q chain end=%q confirmations=%d", round, tier, title, previous, confirmedVersions)
	}
}

// TestDriftAmendmentTitleAndClosureCommitTogetherIntegration composes this
// task's title update with 261007-8594fb's requirements_amended closure in one
// requirement confirmation: a failing title event or a failing later drift
// event leaves neither effect, and the clean confirmation commits both once
// (req-document-operating-surfaces AC-6.1; DEC-46; component-monitor-drift).
func TestDriftAmendmentTitleAndClosureCommitTogetherIntegration(t *testing.T) {
	st, ctx := newDocumentTitleStore(t)
	x := newDriftTitleFixture(t, ctx, st)
	for _, fault := range []struct{ name, inject, clear string }{
		{"title event write", `ALTER TABLE events ADD CONSTRAINT drift_title_fault CHECK (kind NOT LIKE '%.title_changed') NOT VALID`, `ALTER TABLE events DROP CONSTRAINT drift_title_fault`},
		{"later drift reconciliation event", `ALTER TABLE events ADD CONSTRAINT drift_title_fault CHECK (kind <> 'monitor.drift_reconciled') NOT VALID`, `ALTER TABLE events DROP CONSTRAINT drift_title_fault`},
	} {
		if _, err := st.pool.Exec(ctx, fault.inject); err != nil {
			t.Fatal(err)
		}
		_, _, confirmErr := st.ConfirmRequirementVersion(ctx, x.requirementID, x.version)
		if _, err := st.pool.Exec(ctx, fault.clear); err != nil {
			t.Fatal(err)
		}
		if confirmErr == nil {
			t.Fatalf("%s: confirmation succeeded despite the injected fault", fault.name)
		}
		x.assertUnchanged(t, ctx, st, fault.name)
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, x.requirementID, x.version); err != nil {
		t.Fatal(err)
	}
	x.assertCommitted(t, ctx, st)
	if _, _, err := st.ConfirmRequirementVersion(ctx, x.requirementID, x.version); err != nil {
		t.Fatal(err)
	}
	x.assertCommitted(t, ctx, st)
}

// driftTitleFixture is a requirement whose linked requirements_amended drift
// has a pending renaming amendment, built through public store methods.
type driftTitleFixture struct {
	requirementID, driftID, taskID string
	version                        int
}

func newDriftTitleFixture(t *testing.T, ctx context.Context, st *Store) driftTitleFixture {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, Title: "Drift title task", State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "External changes remain traceable."}}
	requirementID := "req-drift-title-" + core.NewTaskID()
	if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Drift title " + requirementID}, core.RequirementVersion{Content: "# Drift title " + requirementID, Origin: core.RequirementOriginOperator, Statements: statements}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, requirementID, 1); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{ID: "drift-" + core.NewTaskID(), WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.ExternalPRMerge,
		SourceURL: "https://example.test/pull/70", CommitSHA: "abc70db", TaskID: taskID, DetectedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if _, fresh, err := st.RecordDrift(ctx, drift); err != nil || !fresh {
		t.Fatalf("record drift fresh=%t err=%v", fresh, err)
	}
	if _, err := st.ResolveDrift(ctx, drift.ID, "requirements_amended", requirementID); err != nil {
		t.Fatal(err)
	}
	renamed, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirementID, Content: "# Renamed by drift amendment", Statements: statements,
		Origin: core.RequirementOriginDriftAmendment, OriginDriftID: drift.ID})
	if err != nil {
		t.Fatal(err)
	}
	return driftTitleFixture{requirementID: requirementID, driftID: drift.ID, taskID: taskID, version: renamed.Version}
}

// driftTitleState reports the listed title, current version, whether the
// drift is still unresolved, and the title-change and reconciliation events.
func (x driftTitleFixture) state(t *testing.T, ctx context.Context, st *Store) (string, int, bool, int, int) {
	t.Helper()
	requirement, err := st.GetRequirement(ctx, x.requirementID)
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := st.ListUnresolvedDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := false
	for _, drift := range unresolved {
		open = open || drift.ID == x.driftID
	}
	documentEvents, err := st.ListRequirementEvents(ctx, x.requirementID)
	if err != nil {
		t.Fatal(err)
	}
	titleEvents := 0
	for _, event := range documentEvents {
		if event.Kind == store.RequirementTitleChangedEvent && strings.Contains(string(event.Payload), "Renamed by drift amendment") {
			titleEvents++
		}
	}
	taskEvents, err := st.ListEvents(ctx, x.taskID)
	if err != nil {
		t.Fatal(err)
	}
	reconciled := 0
	for _, event := range taskEvents {
		if event.Kind == "monitor.drift_reconciled" {
			reconciled++
		}
	}
	return requirement.Title, requirement.CurrentVersion, open, titleEvents, reconciled
}

func (x driftTitleFixture) assertUnchanged(t *testing.T, ctx context.Context, st *Store, fault string) {
	t.Helper()
	title, current, open, titleEvents, reconciled := x.state(t, ctx, st)
	if title != "Drift title "+x.requirementID || current != 1 || !open || titleEvents != 0 || reconciled != 0 {
		t.Fatalf("%s left partial state: title=%q current=%d open=%t title_events=%d reconciled=%d", fault, title, current, open, titleEvents, reconciled)
	}
}

func (x driftTitleFixture) assertCommitted(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	title, current, open, titleEvents, reconciled := x.state(t, ctx, st)
	if title != "Renamed by drift amendment" || current != x.version || open || titleEvents != 1 || reconciled != 1 {
		t.Fatalf("composed confirmation: title=%q current=%d open=%t title_events=%d reconciled=%d", title, current, open, titleEvents, reconciled)
	}
}
