package workorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type freshnessFixtureStore struct {
	store.Store
	failFetch, failObservation, removeOnFetch bool
	hidden                                    string
	afterFetch                                func(core.WorkOrder) core.WorkOrder
	fetched                                   bool
}

func (s *freshnessFixtureStore) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	o, e := s.Store.GetWorkOrder(ctx, id)
	if e == nil && s.fetched && s.afterFetch != nil {
		o = s.afterFetch(o)
	}
	return o, e
}

func (s *freshnessFixtureStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	if s.failFetch {
		return core.Artifact{}, nil, errors.New("private-storage-token=never-return-this")
	}
	a, b, e := s.Store.GetArtifact(ctx, id)
	s.fetched = true
	if s.removeOnFetch {
		s.hidden = id
	}
	return a, b, e
}

func TestArtifactFetchRechecksAttemptAndDeadline(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(core.WorkOrder) core.WorkOrder
	}{
		{"attempt", func(o core.WorkOrder) core.WorkOrder { o.AttemptID = "replacement"; return o }},
		{"deadline", func(o core.WorkOrder) core.WorkOrder { o.ExecutionDeadline = time.Now().Add(-time.Minute); return o }},
		{"lease", func(o core.WorkOrder) core.WorkOrder { o.LeaseExpiresAt = time.Now().Add(-time.Minute); return o }},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx, s, st, order := freshnessService(t)
			a, e := st.CreateArtifact(ctx, core.Artifact{Name: "context.md", ContentType: "text/markdown", TaskID: order.TaskID}, []byte("context"))
			if e != nil {
				t.Fatal(e)
			}
			st.afterFetch = change.mutate
			r, e := s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
			if e == nil || r.Data != "" || r.FetchReceipt != nil {
				t.Fatal("stale read delivered content")
			}
			events, _ := st.ListEvents(ctx, order.TaskID)
			for _, event := range events {
				if event.Kind == "context.artifact_fetch_observed" {
					t.Fatal("stale read recorded fetch")
				}
			}
		})
	}
}
func (s *freshnessFixtureStore) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	artifacts, e := s.Store.ListArtifactsForLineage(ctx, nodes)
	if s.hidden == "" {
		return artifacts, e
	}
	out := artifacts[:0]
	for _, a := range artifacts {
		if a.ID != s.hidden {
			out = append(out, a)
		}
	}
	return out, e
}
func (s *freshnessFixtureStore) RecordContextObservation(ctx context.Context, l taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	if s.failObservation {
		return core.ContextObservation{}, errors.New("private observation exception")
	}
	return s.Store.(store.ContextObservationStore).RecordContextObservation(ctx, l, o)
}
func (s *freshnessFixtureStore) RecordInProcessContextObservation(ctx context.Context, l taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	return s.Store.(store.ContextObservationStore).RecordInProcessContextObservation(ctx, l, o)
}
func freshnessService(t *testing.T) (context.Context, *Service, *freshnessFixtureStore, core.WorkOrder) {
	t.Helper()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: store.UserActorID("owner"), Role: core.ActorUser})
	st := store.NewMemory()
	task := core.Task{ID: "fresh-task", Workspace: "demo", State: core.TaskRunning, Repo: "conveyor", Branch: "task-branch"}
	if e := st.CreateTask(ctx, task); e != nil {
		t.Fatal(e)
	}
	job := core.Job{ID: "fresh-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if e := st.CreateJob(ctx, job); e != nil {
		t.Fatal(e)
	}
	if e := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued}); e != nil {
		t.Fatal(e)
	}
	order, e := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("owner"), SessionID: "session", ClientToken: "secret", Lease: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	wrapper := &freshnessFixtureStore{Store: st}
	p, e := pack.Load("")
	if e != nil {
		t.Fatal(e)
	}
	return ctx, &Service{Store: wrapper, Pack: p}, wrapper, order
}
func TestContextRefreshCorrectionFetchFailureReplayAndAttemptIsolation(t *testing.T) {
	ctx, s, st, order := freshnessService(t)
	initial, e := s.Get(ctx, order.ID, order.SessionID)
	first := initial.ContextFreshness
	if e != nil || !first.ObservationRecorded || first.ComparisonStatus != "unavailable" {
		t.Fatalf("first=%+v error=%v", first, e)
	}
	a, e := st.CreateArtifact(ctx, core.Artifact{Name: "correction.md", ContentType: "text/markdown", TaskID: order.TaskID}, []byte("later correction"))
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.RefreshContext(ctx, order.ID, order.SessionID, first.SelectionRevision)
	if e != nil || next.ComparisonStatus != "changed" || next.Additions.Count != 1 || next.UnfetchedAdditions != 1 || next.Additions.Items[0].ArtifactID != a.ID {
		t.Fatalf("next=%+v error=%v", next, e)
	}
	st.failFetch = true
	failed, e := s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
	if e != nil || failed.Data != "" || failed.FetchReceipt == nil || failed.FetchReceipt.Outcome != "fetch_failed" || failed.FetchReceipt.ContentDigest != "" {
		t.Fatalf("failure=%+v error=%v", failed, e)
	}
	st.failFetch = false
	read, e := s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
	if e != nil || read.FetchReceipt == nil || read.FetchReceipt.Outcome != "fetched" || read.FetchReceipt.ContentDigest != a.ID || read.FetchReceipt.ReturnedBytes != a.SizeBytes {
		t.Fatalf("read=%+v err=%v", read, e)
	}
	again, e := s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
	if e != nil || again.FetchReceipt.ID != read.FetchReceipt.ID || !again.FetchReceipt.ObservedAt.Equal(read.FetchReceipt.ObservedAt) {
		t.Fatal("fetch replay changed receipt")
	}
	refreshed, e := s.RefreshContext(ctx, order.ID, order.SessionID, next.SelectionRevision)
	if e != nil || refreshed.SelectionRevision != next.SelectionRevision || refreshed.ComparisonStatus != "unchanged" || !refreshed.Deliveries[0].Fetched || !refreshed.Deliveries[0].Failed || refreshed.AcknowledgementSupported || refreshed.Deliveries[0].Acknowledgement != "not_recorded" {
		t.Fatalf("refresh=%+v error=%v", refreshed, e)
	}
	events, _ := st.ListEvents(ctx, order.TaskID)
	fetches := 0
	for _, ev := range events {
		if ev.Kind == "context.artifact_fetch_observed" {
			fetches++
			if strings.Contains(string(ev.Payload), "later correction") || strings.Contains(string(ev.Payload), "never-return") {
				t.Fatal("event leaked bytes or exception")
			}
		}
	}
	if fetches != 2 {
		t.Fatalf("fetch count %d", fetches)
	}
	successor := order
	successor.AttemptID = "successor"
	if len(store.ContextHistory(events, successor)) != 0 {
		t.Fatal("prior attempt became fetched")
	}
	st.failObservation = true
	read, e = s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
	if e != nil || read.ContextFreshness.ObservationRecorded || read.FetchReceipt != nil || read.ContextFreshness.Diagnostic != "observation_unavailable" {
		t.Fatal("unavailable observation presented as receipt")
	}
	for _, test := range []struct {
		ctx         context.Context
		id, session string
	}{{store.WithWorkspace(ctx, "foreign"), order.ID, order.SessionID}, {store.WithActor(ctx, store.Actor{ID: "user:foreign", Role: core.ActorUser}), order.ID, order.SessionID}, {ctx, order.ID, "other"}, {ctx, "missing", order.SessionID}} {
		if _, err := s.RefreshContext(test.ctx, test.id, test.session, first.SelectionRevision); err == nil {
			t.Fatal("foreign scope admitted")
		}
	}
	unknown, e := s.RefreshContext(ctx, order.ID, order.SessionID, strings.Repeat("f", 64))
	if e != nil || unknown.ComparisonStatus != "unavailable" || unknown.Additions.Count != 0 {
		t.Fatal("unknown baseline treated as known")
	}
}

func TestArtifactRemovalDuringFetchDoesNotReturnBytesOrReceipt(t *testing.T) {
	ctx, s, st, order := freshnessService(t)
	a, e := st.CreateArtifact(ctx, core.Artifact{Name: "removed.md", ContentType: "text/markdown", TaskID: order.TaskID}, []byte("removed"))
	if e != nil {
		t.Fatal(e)
	}
	st.removeOnFetch = true
	read, e := s.ReadArtifact(ctx, order.ID, order.SessionID, a.ID)
	if e == nil || read.Data != "" || read.FetchReceipt != nil {
		t.Fatal("removed attachment retained capability")
	}
	events, _ := st.ListEvents(ctx, order.TaskID)
	for _, e := range events {
		if e.Kind == "context.artifact_fetch_observed" {
			t.Fatal("unauthorized target-specific receipt")
		}
	}
}
func TestContextRefreshPreservesPinnedAuthorityAndReportsReferenceChanges(t *testing.T) {
	ctx, s, st, order := freshnessService(t)
	// Pure authority decoration retains exact provided pins, independent of delivery receipts.
	lineage, e := s.lineageForOrder(ctx, order)
	if e != nil {
		t.Fatal(e)
	}
	req := []core.ServedRequirementContext{{ID: "req-pin", Version: 2}}
	gov := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "design-pin", Version: 3}}}
	first := core.WithContextAuthority(lineage.Snapshot, "pinned", req, gov)
	order.Stage = core.StageReview
	order.ServedRequirementSnapshot = req
	order.GovernanceSnapshot = gov
	pinned, e := s.freshnessSnapshot(ctx, order, lineage.Snapshot)
	if e != nil || pinned.Revision != first.Revision {
		t.Fatalf("pin changed: %v", e)
	}
	changed := core.WithContextAuthority(lineage.Snapshot, "live", []core.ServedRequirementContext{{ID: "req-pin", Version: 4}}, gov)
	f := core.CompareContext(changed, &pinned)
	if !f.AuthorityReferenceChanged || order.ServedRequirementSnapshot[0].Version != 2 || order.GovernanceSnapshot.Designs[0].Version != 3 {
		t.Fatal("authority drift changed pins")
	}
	_ = st
}
