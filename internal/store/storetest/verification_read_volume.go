package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// verificationReadProbe collects the verification reads a conformance case
// measures. Every backend reports the same operation names through the shared
// probe, so a case asserts a read's volume in one place for memory, PostgreSQL
// and SingleStore (component-persistence, Database read accounting).
type verificationReadProbe struct {
	mu      sync.Mutex
	samples []store.VerificationReadSample
}

func probeVerificationReads(t *testing.T) (*verificationReadProbe, func()) {
	t.Helper()
	probe := &verificationReadProbe{}
	restore := store.ProbeVerificationReadsForTest(func(sample store.VerificationReadSample) {
		probe.mu.Lock()
		probe.samples = append(probe.samples, sample)
		probe.mu.Unlock()
	})
	return probe, restore
}

func (p *verificationReadProbe) reset() {
	p.mu.Lock()
	p.samples = p.samples[:0]
	p.mu.Unlock()
}

func (p *verificationReadProbe) take(workspace string) []store.VerificationReadSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]store.VerificationReadSample, 0, len(p.samples))
	for _, sample := range p.samples {
		if sample.Workspace == workspace {
			out = append(out, sample)
		}
	}
	return out
}

// totals is the read volume of one measured step: the rows decoded, the rows a
// workspace-wide lookup returned, and the number of reads issued.
func (p *verificationReadProbe) totals(workspace string) (rows, workspaceRows, reads int) {
	for _, sample := range p.take(workspace) {
		reads++
		if sample.Operation == store.VerificationReadWorkspace {
			workspaceRows += sample.Rows
			continue
		}
		rows += sample.Rows
	}
	return rows, workspaceRows, reads
}

// count is how many reads of one operation a measured step issued.
func (p *verificationReadProbe) count(workspace, operation string) int {
	n := 0
	for _, sample := range p.take(workspace) {
		if sample.Operation == operation {
			n++
		}
	}
	return n
}

// rows is how many rows one operation read, summed over the measured step.
func (p *verificationReadProbe) rows(workspace, operation string) int {
	n := 0
	for _, sample := range p.take(workspace) {
		if sample.Operation == operation {
			n += sample.Rows
		}
	}
	return n
}

// foreign is how many measured reads decoded another task's rows.
func (p *verificationReadProbe) foreign(workspace, taskID string) []store.VerificationReadSample {
	var out []store.VerificationReadSample
	for _, sample := range p.take(workspace) {
		if sample.TaskID != "" && sample.TaskID != taskID {
			out = append(out, sample)
		}
	}
	return out
}

// runVerificationOperationKeyScope proves the two workspace-wide registration
// rules survive task-scoped reads: another task's logical key is refused with
// the access error, and a changed definition of the task's own key is refused
// with the conflict error (component-verification-service;
// req-verification-evidence).
func runVerificationOperationKeyScope(t *testing.T, x Fixture) {
	t.Run("KeyScope", func(t *testing.T) {
		owner := newVerificationFixture(t, x)
		key := "shared-key-" + owner.access.TaskID
		operation := func(digest string) *store.VerificationOperation {
			return &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte(digest))}
		}
		op := owner.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: operation("{}")})
		other := newVerificationFixture(t, x)
		if _, err := x.Backend.ApplyVerification(other.ctx, other.command(store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "different-" + other.access.TaskID, Operation: operation("{}")})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("another task's unresolved operation on the same subject: %v", err)
		}
		if _, err := x.Backend.ApplyVerification(other.ctx, other.command(store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "different-" + other.access.TaskID, Operation: operation(`{"changed":true}`)})); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("another task's unresolved operation with changed input: %v", err)
		}
		// Resolve the operation so only the key rule can refuse the other task:
		// the key lookup runs before the same-subject rule, but an unresolved
		// operation of the same subject would still refuse with the state error
		// when the other task's key differs.
		owner.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}})
		owner.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "completed", Source: "fixture", CapturedAt: time.Now().UTC()}})

		if _, err := x.Backend.ApplyVerification(other.ctx, other.command(store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: operation("{}")})); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("another task's key: %v", err)
		}
		if _, err := x.Backend.ApplyVerification(other.ctx, other.command(store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: operation(`{"changed":true}`)})); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("another task's key with changed definition: %v", err)
		}
		// The refusal is about the key, not about registration: the other task
		// still registers its own key. Resolving it keeps this fixture from
		// blocking a later case's registration for the same subject.
		own := other.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "own-key-" + other.access.TaskID, Operation: operation("{}")})
		other.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: own.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}})
		other.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: own.ID}, Observation: &store.VerificationOperationObservation{State: "completed", Source: "fixture", CapturedAt: time.Now().UTC()}})
		// A changed definition of the same key in its own task stays a conflict.
		if _, err := x.Backend.ApplyVerification(owner.ctx, owner.command(store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: operation(`{"changed":true}`)})); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("changed definition: %v", err)
		}
		// The unchanged definition still replays the original receipt.
		replay := owner.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: operation("{}")})
		if replay.ID != op.ID {
			t.Fatal("replay changed operation identity")
		}
	})
}

// runVerificationReadVolume proves the read volume of verification work grows
// with the rows a command uses and never with rows it never looks at
// (component-persistence, Database read accounting; req-verification-evidence).
func runVerificationReadVolume(t *testing.T, x Fixture) {
	t.Run("SnapshotStatementsFixed", func(t *testing.T) { runVerificationSnapshotStatements(t, x) })
	t.Run("PublicationCreateSummary", func(t *testing.T) { runVerificationPublicationCreateSummary(t, x) })
	t.Run("DeliveryReadsNewestGeneration", func(t *testing.T) { runVerificationDeliveryGeneration(t, x) })
	t.Run("ReconcilerReadsActedRows", func(t *testing.T) { runVerificationReconcilerReads(t, x) })
	t.Run("OneTaskIndependentOfOthers", func(t *testing.T) { runVerificationTaskIndependence(t, x) })
}

// runVerificationSnapshotStatements proves a snapshot read issues the same
// number of statements for one publication as for six, and that the metadata
// read is one statement over every publication (component-persistence,
// Database read accounting).
func runVerificationSnapshotStatements(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	probe, stop := probeVerificationReads(t)
	defer stop()
	created := 0
	publish := func(n int) {
		for range n {
			created++
			head := fmt.Sprintf("%040d", created)
			p := store.VerificationPublication{PullRequestNumber: 4400 + created, HeadSHA: head, BodyDigest: verificationSHA([]byte(head))}
			pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/volume", "number": p.PullRequestNumber, "head_sha": head})}
			requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
			v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &p})
		}
	}
	measure := func(publications int) (reads, metadata, metadataRows int, queries uint64) {
		probe.reset()
		original := v.ctx
		if x.DrainReadQueries != nil {
			x.DrainReadQueries()
			v.ctx = dbmetrics.WithReadLabel(v.ctx, "verification-snapshot")
		}
		snapshot := v.snapshot(t)
		v.ctx = original
		if x.DrainReadQueries != nil {
			queries = x.DrainReadQueries()["verification-snapshot"]
			if queries == 0 {
				t.Fatal("snapshot recorded no database queries")
			}
		}
		if len(snapshot.Publications) != publications {
			t.Fatalf("publications = %d, want %d", len(snapshot.Publications), publications)
		}
		_, workspaceRows, reads := probe.totals(x.Workspace)
		if workspaceRows != 0 {
			t.Fatalf("a snapshot read issued %d workspace rows", workspaceRows)
		}
		return reads, probe.count(x.Workspace, store.VerificationReadMetadata), probe.rows(x.Workspace, store.VerificationReadMetadata), queries
	}
	publish(1)
	oneReads, oneMetadata, oneRows, oneQueries := measure(1)
	publish(5)
	sixReads, sixMetadata, sixRows, sixQueries := measure(6)
	if oneMetadata != 1 || sixMetadata != 1 {
		t.Fatalf("metadata statements: one publication %d, six publications %d", oneMetadata, sixMetadata)
	}
	if oneRows != 1 || sixRows != 6 {
		t.Fatalf("metadata rows: one publication %d, six publications %d; want 1 and 6", oneRows, sixRows)
	}
	if oneReads != sixReads {
		t.Fatalf("snapshot statements grew with publications: %d then %d", oneReads, sixReads)
	}
	if x.DrainReadQueries != nil && oneQueries != sixQueries {
		t.Fatalf("snapshot database statements grew with publications: %d then %d", oneQueries, sixQueries)
	}
}

func runVerificationPublicationCreateSummary(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	const number = 4498
	head := strings.Repeat("a", 40)
	e := v.envelope("summary-evidence", "summary-evidence-key", "observed state")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
	pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/volume", "number": number, "head_sha": head})}
	requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &store.VerificationPublication{PullRequestNumber: number, HeadSHA: head, BodyDigest: verificationSHA([]byte("summary"))}})
	var delivery *core.VerificationDelivery
	for _, p := range v.snapshot(t).Publications {
		if p.Delivery != nil && (delivery == nil || p.Delivery.Generation > delivery.Generation) {
			d := *p.Delivery
			delivery = &d
		}
	}
	if delivery == nil {
		t.Fatal("publication did not create a delivery")
	}
	requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, store.VerificationDeliveryArgs(*delivery), func(d *core.VerificationDelivery, _ func(string, core.VerificationDelivery) error) error {
		if !strings.Contains(d.Summary, "Records: 2\n") {
			t.Fatalf("summary missing attempt and evidence records: %q", d.Summary)
		}
		if want := verificationSHA([]byte(d.Summary)); d.TargetDigest != want {
			t.Fatalf("target digest = %q, want %q", d.TargetDigest, want)
		}
		return nil
	}))
}

// runVerificationDeliveryGeneration proves a delivery run decodes only the
// newest generation of its pull-request key (component-verification-publication;
// component-persistence, Database read accounting).
func runVerificationDeliveryGeneration(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	const number = 4477
	pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/volume", "number": number, "head_sha": strings.Repeat("a", 40)})}
	requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
	publish := func(name, head string) {
		e := v.envelope(name, "volume-evidence-"+name, "observed state")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Publication: &store.VerificationPublication{PullRequestNumber: number, HeadSHA: head, BodyDigest: verificationSHA([]byte(name))}})
	}
	publish("volume-first", strings.Repeat("a", 40))
	publish("volume-second", strings.Repeat("b", 40))
	var delivery *core.VerificationDelivery
	for _, p := range v.snapshot(t).Publications {
		if p.Delivery != nil {
			d := *p.Delivery
			if delivery == nil || d.Generation > delivery.Generation {
				delivery = &d
			}
		}
	}
	if delivery == nil || delivery.Generation < 2 {
		t.Fatalf("fixture did not retain a superseded generation: %+v", delivery)
	}
	probe, stop := probeVerificationReads(t)
	defer stop()
	args := store.VerificationDeliveryArgs(*delivery)
	var seen core.VerificationDelivery
	requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, _ func(string, core.VerificationDelivery) error) error {
		seen = *d
		return nil
	}))
	if reads := probe.count(x.Workspace, store.VerificationReadDelivery); reads == 0 {
		t.Fatal("delivery run issued no measured read")
	}
	if rows := probe.rows(x.Workspace, store.VerificationReadDelivery); rows != probe.count(x.Workspace, store.VerificationReadDelivery) {
		t.Fatalf("delivery run decoded %d rows over %d reads, want one generation each", rows, probe.count(x.Workspace, store.VerificationReadDelivery))
	}
	if seen.Generation != delivery.Generation {
		t.Fatalf("delivery run acted on generation %d, want the newest %d", seen.Generation, delivery.Generation)
	}
}

// runVerificationReconcilerReads proves the delivery reconciler reads the due
// window without a retained body and decodes a body only for a row it acts on,
// so a superseded generation's body stays on the server (component-persistence,
// Database read accounting; component-verification-publication).
func runVerificationReconcilerReads(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	probe, stop := probeVerificationReads(t)
	defer stop()
	reconcile := func() (due, bodies int) {
		probe.reset()
		requireOK(t, x.Backend.ReconcileVerificationDeliveries(v.ctx))
		return probe.rows(x.Workspace, store.VerificationReadDue), probe.rows(x.Workspace, store.VerificationReadBodies)
	}
	beforeDue, beforeBodies := reconcile()
	const number = 4488
	pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/volume", "number": number, "head_sha": strings.Repeat("a", 40)})}
	requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
	for index, head := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		e := v.envelope(fmt.Sprintf("reconcile-%d", index), fmt.Sprintf("reconcile-evidence-%d", index), "observed state")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Publication: &store.VerificationPublication{PullRequestNumber: number, HeadSHA: head, BodyDigest: verificationSHA([]byte(head))}})
	}
	// Writing a newer generation leaves one due row for the pull-request key:
	// the pass acts on it and decodes exactly its one body, so a superseded
	// generation's retained payload is never read.
	afterDue, afterBodies := reconcile()
	if due, bodies := afterDue-beforeDue, afterBodies-beforeBodies; due != 1 || bodies != 1 {
		t.Fatalf("a pass over a superseded generation read %d due rows and %d bodies, want one acted row and one body", due, bodies)
	}
	if x.RestoreSupersededDeliveries == nil {
		t.Fatal("fixture cannot restore a historical pending generation")
	}
	x.RestoreSupersededDeliveries(t, v.ctx, v.access.TaskID)
	twoDue, twoBodies := reconcile()
	if due, bodies := twoDue-beforeDue, twoBodies-beforeBodies; due != 2 || bodies != 2 {
		t.Fatalf("two pending generations read %d due rows and %d bodies, want both acted on", due, bodies)
	}
}

// runVerificationTaskIndependence proves reading, registering, applying and
// completing verification for one task reads only that task's rows, and that
// the volume does not grow with the workspace's other tasks
// (req-verification-evidence; component-persistence, Database read accounting).
func runVerificationTaskIndependence(t *testing.T, x Fixture) {
	const otherTasks = 198
	flow := func(v *verificationFixture, key string) {
		t.Helper()
		_, err := x.Backend.ReadVerification(v.ctx, v.access, v.contextID)
		requireOK(t, err)
		op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "completed", Source: "fixture", CapturedAt: time.Now().UTC()}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "read volume fixture"}})
		_, err = x.Backend.ReadVerification(v.ctx, v.access, v.contextID)
		requireOK(t, err)
	}
	measured := func(v *verificationFixture, key string) (int, int, int, uint64) {
		t.Helper()
		probe, stop := probeVerificationReads(t)
		defer stop()
		probe.reset()
		if x.DrainReadBytes != nil {
			x.DrainReadBytes()
			v.ctx = dbmetrics.WithReadLabel(v.ctx, "verification-task-flow")
		}
		flow(v, key)
		if foreign := probe.foreign(x.Workspace, v.access.TaskID); len(foreign) != 0 {
			t.Fatalf("%d reads decoded another task's rows: %+v", len(foreign), foreign)
		}
		var bytes uint64
		if x.DrainReadBytes != nil {
			bytes = x.DrainReadBytes()["verification-task-flow"]
			if bytes == 0 {
				t.Fatal("verification task flow recorded no database read bytes")
			}
		}
		rows, workspaceRows, reads := probe.totals(x.Workspace)
		return rows, workspaceRows, reads, bytes
	}
	alone := newVerificationFixture(t, x)
	aloneRows, aloneWorkspace, aloneReads, aloneBytes := measured(&alone, "alone-key")
	for index := range otherTasks {
		other := newVerificationFixture(t, x)
		op := other.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: fmt.Sprintf("other-key-%d-%s", index, other.access.TaskID), Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		other.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}})
		other.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "completed", Source: "fixture", CapturedAt: time.Now().UTC()}})
	}
	crowded := newVerificationFixture(t, x)
	crowdedRows, crowdedWorkspace, crowdedReads, crowdedBytes := measured(&crowded, "crowded-key")
	if crowdedRows != aloneRows || crowdedReads != aloneReads {
		t.Fatalf("one task's read volume grew with %d other tasks: rows %d then %d, reads %d then %d", otherTasks, aloneRows, crowdedRows, aloneReads, crowdedReads)
	}
	if crowdedWorkspace > 1 || aloneWorkspace > 1 {
		t.Fatalf("workspace lookups returned %d then %d rows, want at most the one keyed row", aloneWorkspace, crowdedWorkspace)
	}
	if x.DrainReadBytes != nil && crowdedBytes > aloneBytes*2+1024 {
		t.Fatalf("one task's database reads grew with %d other tasks: %d then %d bytes", otherTasks, aloneBytes, crowdedBytes)
	}
}
