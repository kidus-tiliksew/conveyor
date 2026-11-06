package singlestore

// Artifact repair replay under the cross-partition visibility window
// (component-artifacts, Audit and replay; component-persistence, SingleStore
// visibility). Removing a committed ledger row stands for a predecessor whose
// claim row was released before its ledger row became visible.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestArtifactRepairDryRunDoesNotPersistLocksIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "repair-preview-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.Actor{ID: "user:preview", Role: core.ActorUser})
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy", ContentType: "application/binary"}, testimage.JPEG("preview"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conveyor_locks`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RepairArtifactMetadata(ctx, store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: a.ContentType, NewContentType: "image/jpeg", RequestID: "preview", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conveyor_locks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("dry-run persisted locks: %d -> %d", before, after)
	}
}

const repairGapRequestID = "replay-request-id"

// seedRepairableArtifact stores JPEG bytes under the type stored, as a
// historical row does.
func seedRepairableArtifact(t *testing.T, st *Store, prefix, stored string) (context.Context, string, core.Artifact) {
	t.Helper()
	ws := prefix + "-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.Actor{ID: "user:repair", Role: core.ActorUser})
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy", ContentType: "image/jpeg"}, testimage.JPEG(prefix))
	if err != nil {
		t.Fatal(err)
	}
	setArtifactType(t, st, ctx, ws, a.ID, stored)
	a.ContentType = stored
	return ctx, ws, a
}

func setArtifactType(t *testing.T, st *Store, ctx context.Context, ws, id, contentType string) {
	t.Helper()
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET content_type=? WHERE workspace_id=? AND id=?`, contentType, ws, id); err != nil {
		t.Fatal(err)
	}
}

func artifactType(t *testing.T, st *Store, ctx context.Context, ws, id string) string {
	t.Helper()
	var contentType string
	if err := st.db.QueryRowContext(ctx, `SELECT content_type FROM artifacts WHERE workspace_id=? AND id=?`, ws, id).Scan(&contentType); err != nil {
		t.Fatal(err)
	}
	return contentType
}

func repairEventCount(t *testing.T, st *Store, ctx context.Context, ws string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND kind='artifact.metadata_repaired'`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func repairLedgerCount(t *testing.T, st *Store, ctx context.Context, ws string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_metadata_repairs WHERE workspace_id=?`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// hiddenRepairLedgerRow is a ledger row a test removed to stand for a
// predecessor's committed row that is not yet visible on its partition.
type hiddenRepairLedgerRow struct {
	workspace, requestID, artifactID, expected, newType, actor string
	result                                                     []byte
	createdAt                                                  time.Time
}

func hideRepairLedgerRow(t *testing.T, st *Store, ctx context.Context, ws, requestID string) hiddenRepairLedgerRow {
	t.Helper()
	h := hiddenRepairLedgerRow{workspace: ws, requestID: requestID}
	if err := st.db.QueryRowContext(ctx, `SELECT artifact_id,expected_old_content_type,new_content_type,result,actor,created_at FROM artifact_metadata_repairs WHERE workspace_id=? AND request_id=?`, ws, requestID).
		Scan(&h.artifactID, &h.expected, &h.newType, &h.result, &h.actor, &h.createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM artifact_metadata_repairs WHERE workspace_id=? AND request_id=?`, ws, requestID); err != nil {
		t.Fatal(err)
	}
	return h
}

// restore inserts the hidden row again with its original identity.
func (h hiddenRepairLedgerRow) restore(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO artifact_metadata_repairs(workspace_id,request_id,artifact_id,expected_old_content_type,new_content_type,result,actor,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		h.workspace, h.requestID, h.artifactID, h.expected, h.newType, json.RawMessage(h.result), h.actor, h.createdAt); err != nil {
		t.Fatal(err)
	}
}

// The failure scenario of the task: the winner changed the type and committed,
// and its claim row released before its ledger row is visible. Every request
// under that ID waits for the row and only then decides. Before the fix, the
// exact replay answered the conflict `expected old type "image/png", current
// type "image/jpeg"`, which is the recurring ArtifactRepair conformance failure.
func TestArtifactRepairWaitsForUnseenLedgerRowIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, tc := range []struct {
		name string
		// stored and newType set the first repair: equal types make it a no-op.
		stored, newType string
		events          int
		request         func(store.ArtifactRepairRequest) store.ArtifactRepairRequest
		// conflict states the decision after the wait: 409 rather than the result.
		conflict bool
	}{
		{name: "exact replay of a change returns the recorded result", stored: "image/png", newType: "image/jpeg", events: 1,
			request: func(r store.ArtifactRepairRequest) store.ArtifactRepairRequest { return r }},
		{name: "exact replay of a no-op returns the recorded result", stored: "image/jpeg", newType: "image/jpeg",
			request: func(r store.ArtifactRepairRequest) store.ArtifactRepairRequest { return r }},
		{name: "a different new type answers 409", stored: "image/png", newType: "image/jpeg", events: 1, conflict: true,
			request: func(r store.ArtifactRepairRequest) store.ArtifactRepairRequest {
				r.NewContentType = "image/png"
				return r
			}},
		{name: "a different artifact answers 409", stored: "image/png", newType: "image/jpeg", events: 1, conflict: true,
			request: func(r store.ArtifactRepairRequest) store.ArtifactRepairRequest {
				r.ArtifactID = "another-artifact"
				return r
			}},
		{name: "a matching dry run validates current data and answers 409", stored: "image/png", newType: "image/jpeg", events: 1, conflict: true,
			request: func(r store.ArtifactRepairRequest) store.ArtifactRepairRequest {
				r.DryRun = true
				return r
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ws, a := seedRepairableArtifact(t, st, "repair-window", tc.stored)
			first := store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: tc.stored, NewContentType: tc.newType, RequestID: repairGapRequestID}
			recorded, err := st.RepairArtifactMetadata(ctx, first)
			if err != nil {
				t.Fatal(err)
			}
			key := artifactRepairClaimKey(ws, first.RequestID)
			if got := claimHolder(t, st, key); got != artifactRepairLedgerHolder {
				t.Fatalf("holder=%q, want %q", got, artifactRepairLedgerHolder)
			}
			hidden := hideRepairLedgerRow(t, st, ctx, ws, first.RequestID)

			waitCtx, obs := observeClaimedRecordWait(ctx)
			type outcome struct {
				result store.ArtifactRepairResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := st.RepairArtifactMetadata(waitCtx, tc.request(first))
				done <- outcome{result, err}
			}()
			select {
			case <-obs.waits:
			case o := <-done:
				t.Fatalf("request decided while the ledger row was unseen: result=%+v err=%v", o.result, o.err)
			case <-time.After(time.Minute):
				t.Fatal("request never waited for the ledger row")
			}
			select {
			case o := <-done:
				t.Fatalf("request decided while the ledger row was unseen: result=%+v err=%v", o.result, o.err)
			default:
			}
			hidden.restore(t, st, ctx)

			got := <-done
			if tc.conflict {
				if !errors.Is(got.err, store.ErrArtifactRepairConflict) {
					t.Fatalf("err=%v, want %v", got.err, store.ErrArtifactRepairConflict)
				}
			} else if got.err != nil || got.result != recorded {
				t.Fatalf("result=%+v err=%v, want recorded %+v", got.result, got.err, recorded)
			}
			if n := repairEventCount(t, st, ctx, ws); n != tc.events {
				t.Fatalf("change events=%d, want %d", n, tc.events)
			}
			if n := repairLedgerCount(t, st, ctx, ws); n != 1 {
				t.Fatalf("ledger rows=%d, want 1", n)
			}
			if got := artifactType(t, st, ctx, ws, a.ID); got != tc.newType {
				t.Fatalf("stored type=%q, want %q", got, tc.newType)
			}
		})
	}
}

// A wait that runs out returns ErrRetryable with nothing written, and the same
// request returns the recorded result once the row is visible. With the
// artifact's update unseen as well, a request that decided anyway would repair
// a second time, so that state proves the wait is what stops the write.
func TestArtifactRepairWaitTimeoutWritesNothingIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	for _, tc := range []struct {
		name          string
		artifactStale bool
		dryRun        bool
	}{
		{name: "replay with the artifact update visible"},
		{name: "replay with the artifact update unseen", artifactStale: true},
		{name: "dry run with the artifact update unseen", artifactStale: true, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ws, a := seedRepairableArtifact(t, st, "repair-timeout", "image/png")
			first := store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: "image/png", NewContentType: "image/jpeg", RequestID: repairGapRequestID}
			recorded, err := st.RepairArtifactMetadata(ctx, first)
			if err != nil {
				t.Fatal(err)
			}
			key := artifactRepairClaimKey(ws, first.RequestID)
			hidden := hideRepairLedgerRow(t, st, ctx, ws, first.RequestID)
			visibleType := "image/jpeg"
			if tc.artifactStale {
				visibleType = "image/png"
				setArtifactType(t, st, ctx, ws, a.ID, visibleType)
			}

			retry := first
			retry.DryRun = tc.dryRun
			_, err = st.RepairArtifactMetadata(ctx, retry)
			if !errors.Is(err, store.ErrRetryable) {
				t.Fatalf("err=%v, want %v", err, store.ErrRetryable)
			}
			if strings.Contains(err.Error(), first.RequestID) || strings.Contains(err.Error(), a.ID) {
				t.Fatalf("error %q echoes caller strings", err)
			}
			if n := repairLedgerCount(t, st, ctx, ws); n != 0 {
				t.Fatalf("ledger rows=%d after the timed-out request, want 0", n)
			}
			if n := repairEventCount(t, st, ctx, ws); n != 1 {
				t.Fatalf("change events=%d after the timed-out request, want 1", n)
			}
			if got := artifactType(t, st, ctx, ws, a.ID); got != visibleType {
				t.Fatalf("stored type=%q after the timed-out request, want %q", got, visibleType)
			}
			if got := claimHolder(t, st, key); got != artifactRepairLedgerHolder {
				t.Fatalf("a timed-out request moved the holder to %q", got)
			}

			setArtifactType(t, st, ctx, ws, a.ID, "image/jpeg")
			hidden.restore(t, st, ctx)
			if got, err := st.RepairArtifactMetadata(ctx, first); err != nil || got != recorded {
				t.Fatalf("result=%+v err=%v, want recorded %+v", got, err, recorded)
			}
			if n := repairEventCount(t, st, ctx, ws); n != 1 {
				t.Fatalf("change events=%d after the replay, want 1", n)
			}
		})
	}
}

// Only a completed repair records a holder, so a request that wrote no ledger
// row never leaves a later request waiting for one.
func TestArtifactRepairRefusalLeavesNoHolderIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, a := seedRepairableArtifact(t, st, "repair-refused", "image/png")
	stale := store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: "image/gif", NewContentType: "image/jpeg", RequestID: repairGapRequestID}
	if _, err := st.RepairArtifactMetadata(ctx, stale); !errors.Is(err, store.ErrArtifactRepairConflict) {
		t.Fatalf("stale expected type err=%v, want %v", err, store.ErrArtifactRepairConflict)
	}
	var rows int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_locks WHERE `key`=?", lockRowKey(artifactRepairClaimKey(ws, stale.RequestID))).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("claim rows=%d err=%v after a refusal, want none", rows, err)
	}

	waitCtx, obs := observeClaimedRecordWait(ctx)
	corrected := stale
	corrected.ExpectedOldContentType = "image/png"
	if _, err := st.RepairArtifactMetadata(waitCtx, corrected); err != nil {
		t.Fatal(err)
	}
	select {
	case <-obs.waits:
		t.Fatal("a first repair under the request ID waited")
	default:
	}
}

// A ledger row committed before repairs recorded a holder replays from one read.
func TestArtifactRepairLedgerRowWithoutHolderReplaysIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, a := seedRepairableArtifact(t, st, "repair-preclaim", "image/png")
	first := store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: "image/png", NewContentType: "image/jpeg", RequestID: repairGapRequestID}
	recorded, err := st.RepairArtifactMetadata(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	key := artifactRepairClaimKey(ws, first.RequestID)
	nameClaim(t, st, ctx, key, "")

	waitCtx, obs := observeClaimedRecordWait(ctx)
	if got, err := st.RepairArtifactMetadata(waitCtx, first); err != nil || got != recorded {
		t.Fatalf("result=%+v err=%v, want recorded %+v", got, err, recorded)
	}
	select {
	case <-obs.waits:
		t.Fatal("a visible row without a holder waited")
	default:
	}
	if got := claimHolder(t, st, key); got != "" {
		t.Fatalf("a replay moved the holder to %q", got)
	}
}
