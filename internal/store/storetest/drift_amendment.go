package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runDriftAmendmentConformance covers the requirements_amended lifecycle on
// every backend: the outcome proposes a requirement revision and leaves the
// drift open, and only confirmation of that exact linked version closes it
// inside the confirmation transaction (DEC-46; req-delivery-and-forge AC-4.2,
// AC-4.3; component-monitor-drift).
func runDriftAmendmentConformance(t *testing.T, factory RequirementFactory) {
	t.Run("drift amendment stays open until its linked version is confirmed", func(t *testing.T) {
		for _, documentDrift := range []bool{false, true} {
			t.Run(fmt.Sprintf("document=%t", documentDrift), func(t *testing.T) {
				x := newDriftAmendmentFixture(t, factory)
				drift := x.recordDrift(t, documentDrift)
				other := x.confirmedRequirement(t, "Unrelated intent")

				proposed, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", x.requirement.ID)
				if err != nil || proposed.RequirementID != x.requirement.ID || proposed.Outcome != "" || !proposed.ResolvedAt.IsZero() {
					t.Fatalf("proposal drift=%+v err=%v", proposed, err)
				}
				x.assertOpen(t, drift.ID, x.requirement.ID)
				versions := x.versions(t)
				if len(versions) != 2 || versions[1].OriginDriftID != drift.ID || versions[1].Origin != core.RequirementOriginDriftAmendment || versions[1].Confirmed {
					t.Fatalf("amendment versions=%+v", versions)
				}
				// A retry reuses the live pending amendment.
				if retried, retryErr := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", ""); retryErr != nil || retried.Outcome != "" || retried.RequirementID != x.requirement.ID {
					t.Fatalf("retry drift=%+v err=%v", retried, retryErr)
				}
				if len(x.versions(t)) != 2 {
					t.Fatalf("retry inserted another amendment: %+v", x.versions(t))
				}
				// A retry naming a different requirement is refused.
				if _, retryErr := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", other.ID); !errors.Is(retryErr, monitor.ErrRequirementIDInvalid) {
					t.Fatalf("relink error=%v", retryErr)
				}
				if count := x.reconciledEvents(t, drift.TaskID); count != 0 {
					t.Fatalf("open amendment emitted %d reconciliation events", count)
				}

				if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, versions[1].Version); err != nil {
					t.Fatal(err)
				}
				closed := x.assertClosed(t, drift, "requirements_amended")
				if count := x.reconciledEvents(t, drift.TaskID); count != 1 {
					t.Fatalf("reconciliation events=%d, want 1", count)
				}
				x.assertReconciledPayload(t, drift.TaskID, versions[1].Version)
				if documentDrift {
					x.assertDocumentResolved(t, drift.SystemDesignID, versions[1].Version, 1)
				}
				// Confirmation replay and resolution replay record nothing new.
				if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, versions[1].Version); err != nil {
					t.Fatal(err)
				}
				replayed, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", "")
				if err != nil || replayed.Outcome != "requirements_amended" || !sameInstant(replayed.ResolvedAt, closed.ResolvedAt) {
					t.Fatalf("replayed drift=%+v err=%v", replayed, err)
				}
				if count := x.reconciledEvents(t, drift.TaskID); count != 1 || len(x.versions(t)) != 2 {
					t.Fatalf("replay duplicated closure: events=%d versions=%+v", count, x.versions(t))
				}
				if documentDrift {
					x.assertDocumentResolved(t, drift.SystemDesignID, versions[1].Version, 1)
				}
			})
		}
	})

	t.Run("dismissed or superseded drift amendment leaves drift open", func(t *testing.T) {
		x := newDriftAmendmentFixture(t, factory)
		drift := x.recordDrift(t, false)
		if _, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", x.requirement.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := x.st.DismissRequirementVersion(x.ctx, x.requirement.ID, 2); err != nil {
			t.Fatal(err)
		}
		x.assertOpen(t, drift.ID, x.requirement.ID)
		// Another proposal is possible after dismissal.
		if _, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", ""); err != nil {
			t.Fatal(err)
		}
		versions := x.versions(t)
		if len(versions) != 3 || versions[2].OriginDriftID != drift.ID || versions[2].Confirmed || versions[2].Retired {
			t.Fatalf("reproposal versions=%+v", versions)
		}
		// An unrelated confirmed revision supersedes the pending amendment and
		// does not satisfy the matching-revision rule.
		unrelated, err := x.st.ProposeRequirementVersion(x.ctx, core.RequirementVersion{
			RequirementID: x.requirement.ID, Content: "# Operator revision.", Statements: x.statements,
			Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, unrelated.Version); err != nil {
			t.Fatal(err)
		}
		x.assertOpen(t, drift.ID, x.requirement.ID)
		if versions = x.versions(t); !versions[2].Retired {
			t.Fatalf("pending amendment was not superseded: %+v", versions[2])
		}
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, versions[2].Version); err == nil {
			t.Fatal("superseded amendment was confirmed")
		}
		x.assertOpen(t, drift.ID, x.requirement.ID)
		if count := x.reconciledEvents(t, drift.TaskID); count != 0 {
			t.Fatalf("dismissal or supersession emitted %d reconciliation events", count)
		}
		// A fresh amendment from the new current version closes the drift.
		if _, err = x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", ""); err != nil {
			t.Fatal(err)
		}
		versions = x.versions(t)
		latest := versions[len(versions)-1]
		if latest.OriginDriftID != drift.ID || latest.Confirmed || latest.Retired || latest.Version != unrelated.Version+1 {
			t.Fatalf("fresh amendment=%+v", latest)
		}
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, latest.Version); err != nil {
			t.Fatal(err)
		}
		x.assertClosed(t, drift, "requirements_amended")
		if count := x.reconciledEvents(t, drift.TaskID); count != 1 {
			t.Fatalf("reconciliation events=%d, want 1", count)
		}
		x.assertReconciledPayload(t, drift.TaskID, latest.Version)
	})

	t.Run("another audited outcome closes amendment drift once", func(t *testing.T) {
		x := newDriftAmendmentFixture(t, factory)
		drift := x.recordDrift(t, true)
		if _, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", x.requirement.ID); err != nil {
			t.Fatal(err)
		}
		resolved, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "conflict_resolved", "")
		if err != nil || resolved.Outcome != "conflict_resolved" || resolved.ResolvedAt.IsZero() {
			t.Fatalf("conflict resolution=%+v err=%v", resolved, err)
		}
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, 2); err != nil {
			t.Fatal(err)
		}
		closed := x.assertClosed(t, drift, "conflict_resolved")
		if !sameInstant(closed.ResolvedAt, resolved.ResolvedAt) {
			t.Fatalf("confirmation rewrote the audited outcome: %+v", closed)
		}
		if count := x.reconciledEvents(t, drift.TaskID); count != 1 {
			t.Fatalf("reconciliation events=%d, want 1", count)
		}
		x.assertDocumentResolved(t, drift.SystemDesignID, 0, 1)
	})

	t.Run("confirmation closes only the linked drift in its own workspace and requirement", func(t *testing.T) {
		x := newDriftAmendmentFixture(t, factory)
		drift := x.recordDrift(t, false)
		// A drift-amendment version on another requirement that names this
		// drift is not its matching revision.
		other := x.confirmedRequirement(t, "Other intent")
		if _, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", x.requirement.ID); err != nil {
			t.Fatal(err)
		}
		mismatched, err := x.st.ProposeRequirementVersion(x.ctx, core.RequirementVersion{
			RequirementID: other.ID, Content: "# Mismatched amendment.", Statements: x.statements,
			Origin: core.RequirementOriginDriftAmendment, OriginDriftID: drift.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, other.ID, mismatched.Version); err != nil {
			t.Fatal(err)
		}
		x.assertOpen(t, drift.ID, x.requirement.ID)

		// The same drift and requirement identifiers in another workspace
		// stay open when this workspace confirms its amendment.
		foreign := x.foreignWorkspace(t)
		foreignRequirement, foreignVersion, err := x.st.CreateRequirement(foreign, core.Requirement{ID: x.requirement.ID, Title: "Foreign intent"},
			core.RequirementVersion{Content: "# Foreign intent.", Statements: x.statements, Origin: core.RequirementOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = x.st.ConfirmRequirementVersion(foreign, foreignRequirement.ID, foreignVersion.Version); err != nil {
			t.Fatal(err)
		}
		foreignTask := createDriftTask(t, x.st, foreign, x.foreign, "foreign drift")
		foreignDrift := drift
		foreignDrift.WorkspaceID, foreignDrift.TaskID, foreignDrift.SystemDesignID, foreignDrift.SystemDesignVersion = x.foreign, foreignTask.ID, "", 0
		if _, fresh, recordErr := x.monitor.RecordDrift(foreign, foreignDrift); recordErr != nil || !fresh {
			t.Fatalf("foreign drift fresh=%t err=%v", fresh, recordErr)
		}
		if _, err = x.monitor.ResolveDrift(foreign, foreignDrift.ID, "requirements_amended", foreignRequirement.ID); err != nil {
			t.Fatal(err)
		}

		versions := x.versions(t)
		if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, versions[len(versions)-1].Version); err != nil {
			t.Fatal(err)
		}
		x.assertClosed(t, drift, "requirements_amended")
		foreignState, _, err := x.monitor.RecordDrift(foreign, foreignDrift)
		if err != nil || foreignState.Outcome != "" || !foreignState.ResolvedAt.IsZero() {
			t.Fatalf("foreign drift changed: %+v err=%v", foreignState, err)
		}
	})

	t.Run("concurrent amendment proposals, retries and confirmations close drift once", func(t *testing.T) {
		x := newDriftAmendmentFixture(t, factory)
		drift := x.recordDrift(t, true)
		run := func(n int, fn func() error) []error {
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = fn()
				}()
			}
			wg.Wait()
			return errs
		}
		for _, err := range run(4, func() error {
			_, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", x.requirement.ID)
			return err
		}) {
			if err != nil {
				t.Fatalf("concurrent proposal: %v", err)
			}
		}
		versions := x.versions(t)
		if len(versions) != 2 || versions[1].OriginDriftID != drift.ID {
			t.Fatalf("concurrent proposals versions=%+v", versions)
		}
		var calls int
		var mu sync.Mutex
		for _, err := range run(6, func() error {
			mu.Lock()
			calls++
			confirm := calls%2 == 0
			mu.Unlock()
			if confirm {
				_, _, err := x.st.ConfirmRequirementVersion(x.ctx, x.requirement.ID, versions[1].Version)
				return err
			}
			_, err := x.monitor.ResolveDrift(x.ctx, drift.ID, "requirements_amended", "")
			return err
		}) {
			if err != nil {
				t.Fatalf("concurrent retry or confirmation: %v", err)
			}
		}
		x.assertClosed(t, drift, "requirements_amended")
		if count := x.reconciledEvents(t, drift.TaskID); count != 1 || len(x.versions(t)) != 2 {
			t.Fatalf("concurrent closure events=%d versions=%+v", count, x.versions(t))
		}
		x.assertDocumentResolved(t, drift.SystemDesignID, versions[1].Version, 1)
	})
}

type driftAmendmentFixture struct {
	st          store.Store
	monitor     monitor.Store
	ctx         context.Context
	workspace   string
	foreign     string
	requirement core.Requirement
	statements  []core.RequirementStatement
}

func newDriftAmendmentFixture(t *testing.T, factory RequirementFactory) *driftAmendmentFixture {
	t.Helper()
	st, ctx, workspace := newRequirementFixture(t, factory)
	monitorStore, ok := st.(monitor.Store)
	if !ok {
		t.Fatal("store does not implement monitor.Store")
	}
	x := &driftAmendmentFixture{st: st, monitor: monitorStore, ctx: ctx, workspace: workspace,
		statements: []core.RequirementStatement{requirementStatement("REQ-1", "External changes remain traceable.")}}
	x.requirement = x.confirmedRequirement(t, "Amended intent")
	return x
}

func (x *driftAmendmentFixture) confirmedRequirement(t *testing.T, title string) core.Requirement {
	t.Helper()
	requirement, version, err := x.st.CreateRequirement(x.ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: title},
		core.RequirementVersion{Content: "# " + title + ".", Statements: x.statements, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = x.st.ConfirmRequirementVersion(x.ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	return requirement
}

func (x *driftAmendmentFixture) foreignWorkspace(t *testing.T) context.Context {
	t.Helper()
	x.foreign = x.workspace + "-foreign"
	ctx := store.WithWorkspace(x.ctx, x.foreign)
	bootstrap, ok := x.st.(interface {
		BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	})
	if !ok {
		t.Fatal("store cannot bootstrap a foreign workspace")
	}
	if _, err := bootstrap.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: x.foreign, Repos: requirementConformanceRepos}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func (x *driftAmendmentFixture) recordDrift(t *testing.T, document bool) monitor.Drift {
	t.Helper()
	task := createDriftTask(t, x.st, x.ctx, x.workspace, "amendment drift")
	drift := monitor.Drift{
		ID: "drift-" + core.NewTaskID(), WorkspaceID: x.workspace, Repository: "conveyor",
		Kind: monitor.ExternalPRMerge, SourceURL: "https://example.test/pull/8594", CommitSHA: "abc8594",
		TaskID: task.ID, DetectedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if document {
		design := createConfirmedDesign(t, x.st, x.ctx, "DESIGN-amendment-"+core.NewTaskID(), "internal/monitor/**")
		drift.SystemDesignID, drift.SystemDesignVersion, drift.MatchingPaths = design.ID, 1, []string{"internal/monitor/types.go"}
	}
	if _, fresh, err := x.monitor.RecordDrift(x.ctx, drift); err != nil || !fresh {
		t.Fatalf("record drift fresh=%t err=%v", fresh, err)
	}
	return drift
}

func (x *driftAmendmentFixture) versions(t *testing.T) []core.RequirementVersion {
	t.Helper()
	versions, err := x.st.ListRequirementVersions(x.ctx, x.requirement.ID)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func (x *driftAmendmentFixture) unresolved(t *testing.T, id string) (monitor.Drift, bool) {
	t.Helper()
	drifts, err := x.monitor.ListUnresolvedDrift(x.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range drifts {
		if drift.ID == id {
			return drift, true
		}
	}
	return monitor.Drift{}, false
}

func (x *driftAmendmentFixture) assertOpen(t *testing.T, id, requirementID string) {
	t.Helper()
	drift, open := x.unresolved(t, id)
	if !open || drift.RequirementID != requirementID || drift.Outcome != "" || !drift.ResolvedAt.IsZero() {
		t.Fatalf("drift %s open=%t drift=%+v, want open and linked to %s", id, open, drift, requirementID)
	}
}

func (x *driftAmendmentFixture) assertClosed(t *testing.T, recorded monitor.Drift, outcome string) monitor.Drift {
	t.Helper()
	id := recorded.ID
	if drift, open := x.unresolved(t, id); open {
		t.Fatalf("drift remains unresolved: %+v", drift)
	}
	status, err := x.monitor.MonitorStatus(x.ctx, true, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range status.Drift {
		if drift.ID == id {
			t.Fatalf("closed drift still counted: %+v", drift)
		}
	}
	// RecordDrift on an existing identity reports the stored record.
	drift, fresh, err := x.monitor.RecordDrift(x.ctx, recorded)
	if err != nil || fresh || drift.Outcome != outcome || drift.ResolvedAt.IsZero() {
		t.Fatalf("closed drift=%+v fresh=%t err=%v, want outcome %s", drift, fresh, err, outcome)
	}
	return drift
}

func (x *driftAmendmentFixture) reconciledEvents(t *testing.T, taskID string) int {
	t.Helper()
	events, err := x.st.ListEvents(x.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "monitor.drift_reconciled" {
			count++
		}
	}
	return count
}

func (x *driftAmendmentFixture) assertReconciledPayload(t *testing.T, taskID string, version int) {
	t.Helper()
	events, err := x.st.ListEvents(x.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != "monitor.drift_reconciled" {
			continue
		}
		var payload map[string]any
		if err = json.Unmarshal(event.Payload, &payload); err != nil || payload["outcome"] != "requirements_amended" ||
			payload["requirement_id"] != x.requirement.ID || fmt.Sprint(payload["confirmed_version"]) != fmt.Sprint(version) ||
			payload["confirmed_by"] != requirementConformanceActor {
			t.Fatalf("reconciliation payload=%s err=%v", event.Payload, err)
		}
		return
	}
	t.Fatal("reconciliation event missing")
}

// assertDocumentResolved checks the design timeline. A positive version
// requires the requirement provenance on the resolution event.
func (x *driftAmendmentFixture) assertDocumentResolved(t *testing.T, documentID string, version, want int) {
	t.Helper()
	events, err := x.st.ListSystemDesignEvents(x.ctx, documentID)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, event := range events {
		if event.Kind != "system_design.drift_resolved" {
			continue
		}
		resolved++
		if version == 0 {
			continue
		}
		var payload map[string]any
		if err = json.Unmarshal(event.Payload, &payload); err != nil || payload["outcome"] != "requirements_amended" ||
			payload["requirement_id"] != x.requirement.ID || fmt.Sprint(payload["confirmed_version"]) != fmt.Sprint(version) {
			t.Fatalf("document resolution payload=%s err=%v", event.Payload, err)
		}
	}
	if resolved != want {
		t.Fatalf("document resolution events=%d, want %d: %+v", resolved, want, events)
	}
}
