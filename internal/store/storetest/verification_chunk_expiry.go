package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runVerificationChunkExpiry pins the internal, bounded expiry of stale upload
// chunks on every backend: reconciler-only authority in an explicit workspace,
// the cutoff boundary, expiry after the claim is gone, a bounded backlog and a
// backlog above the production 100-row limit drained over successive runs,
// no-op replay without a second event, retained finalized evidence, expiry
// and finalization overlapping in both serialized orders and released
// concurrently, and rollback on a failed audit write (req-verification-evidence REQ-2/AC-2.1,
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
	deletedTotal := func(t *testing.T, events []core.Event) int {
		t.Helper()
		total := 0
		for _, e := range events {
			var payload struct {
				DeletedCount int `json:"deleted_count"`
			}
			requireOK(t, json.Unmarshal(e.Payload, &payload))
			total += payload.DeletedCount
		}
		return total
	}
	// upload stages n chunks of one upload at the fixture clock and returns
	// the finalization command for the assembled bytes.
	upload := func(t *testing.T, v *verificationFixture, id string, n int) (store.VerificationCommand, []byte) {
		t.Helper()
		var data []byte
		for index := 0; index < n; index++ {
			stage(t, v, id, index)
			data = append(data, []byte(id+" staged bytes")...)
		}
		input := store.VerificationArtifactInput{UploadID: id, Name: id + ".txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: verificationSHA(data)}
		return store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: id, Artifacts: []store.VerificationArtifactInput{input}}, data
	}
	// retained requires finalized bytes that accepted evidence can link and
	// read back, and a finalization receipt that replays unchanged.
	retained := func(t *testing.T, v *verificationFixture, finalize store.VerificationCommand, receipt store.VerificationReceipt, data []byte) {
		t.Helper()
		if len(receipt.ArtifactIDs) != 1 {
			t.Fatalf("finalization receipt = %+v", receipt)
		}
		e := v.envelope(finalize.Key+"-evidence", finalize.Key+"-evidence", "capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: receipt.ArtifactIDs[0], SHA256: receipt.ArtifactIDs[0], MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		_, content, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, receipt.ArtifactIDs[0])
		requireOK(t, err)
		if string(content) != string(data) {
			t.Fatal("finalized bytes changed")
		}
		if replay := v.apply(t, finalize); replay.ID != receipt.ID {
			t.Fatal("finalization receipt was not retained")
		}
	}
	// notRetained requires that a refused finalization left no artifact that
	// evidence can link.
	notRetained := func(t *testing.T, v *verificationFixture, finalize store.VerificationCommand, data []byte) {
		t.Helper()
		hash := verificationSHA(data)
		e := v.envelope(finalize.Key+"-orphan", finalize.Key+"-orphan", "capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})); err == nil {
			t.Fatal("evidence linked bytes of a refused finalization")
		}
	}
	// ProductionLimitBacklog drains more than one production-sized run of
	// valid uploads over successive ticks at store.VerificationChunkExpiryLimit.
	t.Run("ProductionLimitBacklog", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		total := 0
		for _, u := range []struct {
			id string
			n  int
		}{{"backlog-a", 50}, {"backlog-b", 50}, {"backlog-c", 7}} {
			upload(t, &v, u.id, u.n)
			total += u.n
		}
		if total <= store.VerificationChunkExpiryLimit {
			t.Fatalf("backlog of %d does not exceed the production limit", total)
		}
		tick := at(reconciler, 25*time.Hour)
		if n := expire(t, tick, 10*store.VerificationChunkExpiryLimit); n != store.VerificationChunkExpiryLimit {
			t.Fatalf("first tick deleted %d chunk(s), want the %d-row bound", n, store.VerificationChunkExpiryLimit)
		}
		events := expiryEvents(t, v.access.TaskID)
		if len(events) != 1 || deletedTotal(t, events) != store.VerificationChunkExpiryLimit {
			t.Fatalf("first tick audit: %d event(s), %d deleted", len(events), deletedTotal(t, events))
		}
		if n := expire(t, tick, store.VerificationChunkExpiryLimit); n != total-store.VerificationChunkExpiryLimit {
			t.Fatalf("second tick deleted %d chunk(s), want the remaining %d", n, total-store.VerificationChunkExpiryLimit)
		}
		if n := expire(t, tick, store.VerificationChunkExpiryLimit); n != 0 {
			t.Fatalf("drained backlog replay deleted %d chunk(s)", n)
		}
		events = expiryEvents(t, v.access.TaskID)
		if len(events) != 2 || deletedTotal(t, events) != total {
			t.Fatalf("backlog audit: %d event(s), %d deleted, want 2 and %d", len(events), deletedTotal(t, events), total)
		}
	})
	// FinalizationWinsOverlappingExpiry starts expiry while finalization holds
	// its transaction, after expiry can select the staging rows as candidates.
	// The locked recheck finds them consumed, so expiry deletes nothing and
	// writes no event, and the finalized evidence, bytes and receipt survive.
	t.Run("FinalizationWinsOverlappingExpiry", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		finalize, data := upload(t, &v, "race-final", 3)
		type result struct {
			n   int
			err error
		}
		expired := make(chan result, 1)
		var once sync.Once
		inside := store.WithVerificationFault(v.ctx, func(step string) error {
			once.Do(func() {
				go func() {
					n, err := x.Backend.ExpireVerificationChunks(at(reconciler, 25*time.Hour), store.VerificationChunkExpiryLimit)
					expired <- result{n, err}
				}()
				// Hold the finalization transaction open while expiry selects
				// candidates and waits for the task lock.
				time.Sleep(300 * time.Millisecond)
			})
			return nil
		})
		receipt, err := x.Backend.ApplyVerification(inside, v.command(finalize))
		requireOK(t, err)
		r := <-expired
		requireOK(t, r.err)
		if r.n != 0 {
			t.Fatalf("expiry deleted %d consumed chunk(s)", r.n)
		}
		if events := expiryEvents(t, v.access.TaskID); len(events) != 0 {
			t.Fatalf("expiry recorded %d event(s) for consumed rows", len(events))
		}
		retained(t, &v, finalize, receipt, data)
		if n := expire(t, at(reconciler, 25*time.Hour), store.VerificationChunkExpiryLimit); n != 0 || len(expiryEvents(t, v.access.TaskID)) != 0 {
			t.Fatalf("replayed expiry after finalization deleted %d chunk(s)", n)
		}
	})
	// ExpiryWinsOverlappingFinalization starts finalization while expiry holds
	// its transaction after the deletion. Finalization waits for the task lock,
	// finds no chunks, and retains nothing; expiry records exactly the rows it
	// deleted, once.
	t.Run("ExpiryWinsOverlappingFinalization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		finalize, data := upload(t, &v, "race-expire", 3)
		finalized := make(chan error, 1)
		var once sync.Once
		inside := store.WithVerificationFault(at(reconciler, 25*time.Hour), func(step string) error {
			once.Do(func() {
				go func() {
					_, err := x.Backend.ApplyVerification(v.ctx, v.command(finalize))
					finalized <- err
				}()
				time.Sleep(300 * time.Millisecond)
			})
			return nil
		})
		n, err := x.Backend.ExpireVerificationChunks(inside, store.VerificationChunkExpiryLimit)
		requireOK(t, err)
		if n != 3 {
			t.Fatalf("expiry deleted %d chunk(s), want 3", n)
		}
		if err := <-finalized; err == nil {
			t.Fatal("finalization of an expired upload succeeded")
		}
		notRetained(t, &v, finalize, data)
		events := expiryEvents(t, v.access.TaskID)
		if len(events) != 1 || deletedTotal(t, events) != 3 {
			t.Fatalf("expiry audit: %d event(s), %d deleted, want one event for 3", len(events), deletedTotal(t, events))
		}
	})
	// PartialExpiryBlocksIncompleteFinalization deletes only the first chunk of
	// an upload under a one-row bound. Finalization then refuses the
	// non-contiguous remainder and retains no truncated bytes.
	t.Run("PartialExpiryBlocksIncompleteFinalization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		finalize, data := upload(t, &v, "race-partial", 3)
		if n := expire(t, at(reconciler, 25*time.Hour), 1); n != 1 {
			t.Fatalf("bounded expiry deleted %d chunk(s), want 1", n)
		}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(finalize)); err == nil {
			t.Fatal("finalization retained an incomplete upload")
		}
		notRetained(t, &v, finalize, data)
		if n := expire(t, at(reconciler, 25*time.Hour), store.VerificationChunkExpiryLimit); n != 2 {
			t.Fatalf("remaining chunks expired %d, want 2", n)
		}
		if events := expiryEvents(t, v.access.TaskID); len(events) != 2 || deletedTotal(t, events) != 3 {
			t.Fatalf("partial expiry audit: %d event(s), %d deleted", len(events), deletedTotal(t, events))
		}
	})
	// ConcurrentExpiryAndFinalization releases both commands together several
	// times. Whichever wins, exactly one outcome holds: finalized bytes and
	// evidence survive with no deletion event, or every chunk is deleted once,
	// one event records them, and nothing is retained.
	t.Run("ConcurrentExpiryAndFinalization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		drain(t)
		for round := 0; round < 4; round++ {
			finalize, data := upload(t, &v, fmt.Sprintf("race-concurrent-%d", round), 2)
			before := expiryEvents(t, v.access.TaskID)
			start := make(chan struct{})
			var wg sync.WaitGroup
			var receipt store.VerificationReceipt
			var finalizeErr, expireErr error
			var deleted int
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				receipt, finalizeErr = x.Backend.ApplyVerification(v.ctx, v.command(finalize))
			}()
			go func() {
				defer wg.Done()
				<-start
				deleted, expireErr = x.Backend.ExpireVerificationChunks(at(reconciler, 25*time.Hour), store.VerificationChunkExpiryLimit)
			}()
			close(start)
			wg.Wait()
			requireOK(t, expireErr)
			added := expiryEvents(t, v.access.TaskID)[len(before):]
			switch {
			case finalizeErr == nil && deleted == 0 && len(added) == 0:
				retained(t, &v, finalize, receipt, data)
			case finalizeErr != nil && deleted == 2 && len(added) == 1 && deletedTotal(t, added) == 2:
				notRetained(t, &v, finalize, data)
			default:
				t.Fatalf("round %d: finalize err=%v deleted=%d new events=%d", round, finalizeErr, deleted, len(added))
			}
			if n := expire(t, at(reconciler, 25*time.Hour), store.VerificationChunkExpiryLimit); n != 0 {
				t.Fatalf("round %d replay deleted %d chunk(s)", round, n)
			}
			if after := expiryEvents(t, v.access.TaskID); len(after) != len(before)+len(added) {
				t.Fatalf("round %d replay wrote a duplicate expiry event", round)
			}
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
