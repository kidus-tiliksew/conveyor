package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runVerificationChunkExpiry pins the internal, bounded expiry of stale upload
// chunks on every backend: reconciler-only authority in an explicit workspace,
// the cutoff boundary, expiry after the claim is gone, a bounded backlog,
// no-op replay without a second event, retained finalized evidence, and
// rollback on a failed audit write (req-verification-evidence REQ-2/AC-2.1,
// AC-2.4; component-verification-evidence).
func runVerificationChunkExpiry(t *testing.T, x Fixture) {
	reconciler := store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
	staged := time.Now().UTC().Truncate(time.Microsecond)
	at := func(ctx context.Context, d time.Duration) context.Context {
		return store.WithVerificationClockForTest(ctx, func() time.Time { return staged.Add(d) })
	}
	// The Verification pack shares one workspace, so earlier cases may leave
	// staged chunks. drain removes every row expiring by the latest cutoff a
	// case uses before that case stages its own rows.
	drain := func(t *testing.T) {
		t.Helper()
		for {
			n, err := x.Backend.ExpireVerificationChunks(at(reconciler, 72*time.Hour), 100)
			requireOK(t, err)
			if n == 0 {
				return
			}
		}
	}
	stage := func(t *testing.T, v *verificationFixture, upload string, index int) {
		t.Helper()
		_, err := x.Backend.ApplyVerification(at(v.ctx, 0), v.command(store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: upload, Index: index, Content: []byte(upload + " staged bytes")}}))
		requireOK(t, err)
	}
	expire := func(t *testing.T, ctx context.Context, limit int) int {
		t.Helper()
		n, err := x.Backend.ExpireVerificationChunks(ctx, limit)
		requireOK(t, err)
		return n
	}
	expiryEvents := func(t *testing.T, taskID string) []core.Event {
		t.Helper()
		events, err := x.Backend.ListEvents(x.Context, taskID)
		requireOK(t, err)
		var out []core.Event
		for _, e := range events {
			if e.Kind == "verification.chunk.expire" {
				out = append(out, e)
			}
		}
		return out
	}
	t.Run("ReconcilerOnlyAndWorkspaceIsolation", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		stage(t, &v, "authority", 0)
		for name, ctx := range map[string]context.Context{
			"claim actor":     at(v.ctx, 25*time.Hour),
			"other system":    at(store.WithActor(x.Context, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), 25*time.Hour),
			"user":            at(store.WithActor(x.Context, store.Actor{ID: "owner", Role: core.ActorUser}), 25*time.Hour),
			"reconciler role": at(store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorWorker}), 25*time.Hour),
		} {
			if _, err := x.Backend.ExpireVerificationChunks(ctx, 100); !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("%s expired chunks: %v", name, err)
			}
		}
		if n := expire(t, at(store.WithWorkspace(reconciler, x.Workspace+"-other"), 25*time.Hour), 100); n != 0 {
			t.Fatalf("another workspace expired %d chunk(s)", n)
		}
		if n := expire(t, at(reconciler, 25*time.Hour), 100); n != 1 {
			t.Fatalf("reconciler expired %d chunk(s), want 1", n)
		}
	})
	t.Run("CutoffAndReplay", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		stage(t, &v, "cutoff", 0)
		if n := expire(t, at(reconciler, 24*time.Hour-time.Microsecond), 100); n != 0 {
			t.Fatalf("unexpired chunk deleted: %d", n)
		}
		if n := expire(t, at(reconciler, 24*time.Hour), 100); n != 1 {
			t.Fatalf("chunk at the cutoff not deleted: %d", n)
		}
		events := expiryEvents(t, v.access.TaskID)
		if len(events) != 1 || events[0].ActorID != "verification-reconciler" || events[0].ActorRole != core.ActorSystem {
			t.Fatalf("expiry audit = %+v", events)
		}
		var payload struct {
			DeletedCount int      `json:"deleted_count"`
			ChunkIDs     []string `json:"chunk_ids"`
			UploadIDs    []string `json:"upload_ids"`
		}
		requireOK(t, json.Unmarshal(events[0].Payload, &payload))
		if payload.DeletedCount != 1 || len(payload.ChunkIDs) != 1 || len(payload.UploadIDs) != 1 || payload.UploadIDs[0] != "cutoff" {
			t.Fatalf("expiry payload = %+v", payload)
		}
		if n := expire(t, at(reconciler, 48*time.Hour), 100); n != 0 {
			t.Fatalf("replay deleted %d chunk(s)", n)
		}
		if len(expiryEvents(t, v.access.TaskID)) != 1 {
			t.Fatal("no-op replay appended an expiry event")
		}
	})
	t.Run("AfterClaimReleased", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		stage(t, &v, "released", 0)
		_, err := ReleaseWorkerClaim(x.Context, x.Backend, v.access.WorkOrderID, v.access.Claim.WorkerID, core.WorkOrderRelease{SessionID: v.access.Claim.SessionID, Reason: "fixture handoff"})
		requireOK(t, err)
		if n := expire(t, at(reconciler, 25*time.Hour), 100); n != 1 {
			t.Fatalf("expiry after release deleted %d chunk(s), want 1", n)
		}
	})
	t.Run("BoundedBacklog", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		for index := 0; index < 3; index++ {
			stage(t, &v, "backlog", index)
		}
		for _, want := range []int{2, 1, 0} {
			if n := expire(t, at(reconciler, 25*time.Hour), 2); n != want {
				t.Fatalf("bounded run deleted %d chunk(s), want %d", n, want)
			}
		}
		if len(expiryEvents(t, v.access.TaskID)) != 2 {
			t.Fatal("each committed run needs exactly one expiry event")
		}
	})
	t.Run("FinalizedEvidenceRetained", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		data := []byte("finalized capture")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "kept", Content: data}})
		input := store.VerificationArtifactInput{UploadID: "kept", Name: "capture.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: verificationSHA(data)}
		finalize := store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: "kept", Artifacts: []store.VerificationArtifactInput{input}}
		receipt := v.apply(t, finalize)
		e := v.envelope("kept-evidence", "kept-evidence", "capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: receipt.ArtifactIDs[0], SHA256: receipt.ArtifactIDs[0], MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		stage(t, &v, "stale", 0)
		before := len(v.snapshot(t).Evidence)
		if n := expire(t, at(reconciler, 25*time.Hour), 100); n != 1 {
			t.Fatalf("expiry deleted %d chunk(s), want only the stale upload", n)
		}
		if len(v.snapshot(t).Evidence) != before {
			t.Fatal("expiry removed accepted evidence")
		}
		_, content, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, receipt.ArtifactIDs[0])
		requireOK(t, err)
		if string(content) != string(data) {
			t.Fatal("expiry changed finalized bytes")
		}
		if replay := v.apply(t, finalize); replay.ID != receipt.ID {
			t.Fatal("expiry removed the finalization receipt")
		}
	})
	t.Run("EventFailureRollsBack", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		stage(t, &v, "rollback", 0)
		for _, step := range []string{"evidence", "event"} {
			failing := store.WithVerificationFault(at(reconciler, 25*time.Hour), func(at string) error {
				if at == step {
					return store.ErrVerificationState
				}
				return nil
			})
			if _, err := x.Backend.ExpireVerificationChunks(failing, 100); err == nil {
				t.Fatalf("%s fault committed", step)
			}
			if len(expiryEvents(t, v.access.TaskID)) != 0 {
				t.Fatalf("%s fault appended an expiry event", step)
			}
		}
		if n := expire(t, at(reconciler, 25*time.Hour), 100); n != 1 {
			t.Fatalf("rolled-back chunk was not retained: %d", n)
		}
	})
}
