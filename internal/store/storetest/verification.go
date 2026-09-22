package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type verificationFixture struct {
	x                        Fixture
	ctx                      context.Context
	access                   store.VerificationAccess
	contextID, runID, digest string
	subject                  core.VerificationSubject
	revisions                []core.VerificationRevision
	pins                     []core.VerificationPin
}

func newVerificationFixture(t *testing.T, x Fixture, empty ...bool) verificationFixture {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: core.StageVerify, State: core.JobPending}
	o := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	_, err := CreateStageWorkOrder(x.Context, x.Backend, job, o)
	requireOK(t, err)
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verification", ClientToken: "verification-fixture-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
	claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	o, err = ClaimWorkOrder(x.Context, x.Backend, o.ID, claim)
	requireOK(t, err)
	v := verificationFixture{x: x, ctx: store.WithActor(x.Context, store.Actor{ID: "worker:worker", Role: core.ActorWorker}), access: store.VerificationAccess{TaskID: o.TaskID, WorkOrderID: o.ID, WorkOrderAttemptID: o.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}}, revisions: []core.VerificationRevision{{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: strings.Repeat("a", 40)}}, pins: []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}}
	c := store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "prepare", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": strings.Repeat("a", 40), "state": "no_manifest"})}}}
	result := v.apply(t, c)
	v.contextID = result.ID
	replay := v.apply(t, c)
	if replay.ID != result.ID {
		t.Fatal("context replay changed identity")
	}

	selection := store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, ManifestRevision: strings.Repeat("a", 40), SourceRevision: strings.Repeat("a", 40), ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Stage: "verify", Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{}}, Subjects: []store.VerificationSubjectContract{}}
	selected := v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})
	replayed := v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})
	if selected.ID != replayed.ID {
		t.Fatal("selection replay changed identity")
	}
	selection.Receipt.Diagnostics = append(selection.Receipt.Diagnostics, verification.Diagnostic{Path: "manifest", Message: "changed"})
	if _, err = x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})); !errors.Is(err, store.ErrVerificationConflict) {
		t.Fatalf("selection mutation: %v", err)
	}
	if len(empty) > 0 && empty[0] {
		return v
	}
	obligation := store.VerificationObligation{ID: "ordinary", Description: "Observe the test state", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "ordinary", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	result = v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.digest = result.Digest
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: result.Digest}
	v.start(t, "start-1")
	return v
}
func (v *verificationFixture) command(c store.VerificationCommand) store.VerificationCommand {
	if c.Kind == store.VerificationStartAttempt && c.Coverage == nil {
		snapshot, err := v.x.Backend.ReadVerification(v.ctx, v.access, v.contextID)
		if err == nil {
			if c.Attempt != nil && c.Attempt.GrantID == "" {
				for _, g := range snapshot.PermissionGrants {
					if g.Subject == c.Attempt.Subject {
						c.Attempt.GrantID = g.ID
						c.Attempt.EffectiveActions = []core.VerificationPermission{}
						break
					}
				}
			}
			coverage := verificationFixtureCoverage(snapshot)
			c.Coverage = &coverage
		}
	}
	c.Access = v.access
	c.ContextID = v.contextID
	c.RunID = v.runID
	return c
}
func (v *verificationFixture) apply(t *testing.T, c store.VerificationCommand) store.VerificationReceipt {
	t.Helper()
	if c.Kind == store.VerificationStartAttempt && c.Attempt != nil && c.Attempt.GrantID == "" {
		c.Attempt.GrantID = GrantVerificationFixture(t, v.x.Backend, v.x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "grant-"+c.Key, c.Attempt.Subject, nil)
		c.Attempt.EffectiveActions = []core.VerificationPermission{}
	}
	r, err := v.x.Backend.ApplyVerification(v.ctx, v.command(c))
	requireOK(t, err)
	return r
}
func (v *verificationFixture) start(t *testing.T, key string) {
	t.Helper()
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: key, Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}})
	v.runID = r.ID
}
func (v *verificationFixture) envelope(id, key, value string) core.VerificationEvidence {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	return core.VerificationEvidence{SchemaVersion: 1, ID: id, SubmissionKey: key, Type: "state_observation", CapturedAt: at, ReceivedAt: at, SubmittedBy: "worker:worker", CapturedBy: core.VerificationCaptureActor{Identity: "fixture-tool", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: v.x.Workspace, TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID, WorkOrderAttemptID: v.access.WorkOrderAttemptID, ContextID: v.contextID, RunID: v.runID, Subject: v.subject, Revisions: v.revisions, GoverningPins: v.pins, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, Payload: core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: at, Value: core.JSONPayload(value)})}
}
func (v *verificationFixture) snapshot(t *testing.T) store.VerificationSnapshot {
	t.Helper()
	s, err := v.x.Backend.ReadVerification(v.ctx, v.access, v.contextID)
	requireOK(t, err)
	return s
}
func verificationBytes(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func verificationSHA(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func runVerification(t *testing.T, x Fixture) {
	runVerificationPublicationDelivery(t, x)
	runVerificationPublicationLegacyAndRace(t, x)
	runVerificationOperations(t, x)
	runVerificationScope(t, x)
	runVerificationFinalization(t, x)
	runVerificationClaimLoss(t, x)
	runVerificationPermissions(t, x)
	runVerificationLimits(t, x)
	runVerificationTerminalRace(t, x)
	v := newVerificationFixture(t, x)
	t.Run("NestedTaskLock", func(t *testing.T) {
		if !x.Backend.IsDurable() {
			return
		}
		ctx, cancel := context.WithTimeout(v.ctx, 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		command := v.command(store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "nested-lock", Content: []byte("same")}})
		err := x.Backend.WithTaskSideEffectLock(ctx, v.access.TaskID, func(locked context.Context) error {
			go func() { _, err := x.Backend.ApplyVerification(ctx, command); done <- err }()
			select {
			case err := <-done:
				return fmt.Errorf("write crossed held task lock: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			_, err := x.Backend.ApplyVerification(locked, command)
			return err
		})
		requireOK(t, err)
		select {
		case err := <-done:
			requireOK(t, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})
	t.Run("PublicationIdentity", func(t *testing.T) {
		p := store.VerificationPublication{PullRequestNumber: 42, HeadSHA: strings.Repeat("a", 40), BodyDigest: verificationSHA([]byte("same summary"))}
		first := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &p})
		again := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &p})
		if first.PublicationID != again.PublicationID {
			t.Fatal("publication replay changed identity")
		}
		p.HeadSHA = strings.Repeat("b", 40)
		next := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &p})
		if next.PublicationID == first.PublicationID {
			t.Fatal("publication head was not part of identity")
		}
	})
	t.Run("ReplayRedactionAndScopes", func(t *testing.T) {
		e := v.envelope("evidence-one", "batch-one", "sk-abcdefghijklmnopqrstuvwxyz012345")
		c := store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}}
		first := v.apply(t, c)
		events, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		again := v.apply(t, c)
		if !reflect.DeepEqual(first, again) {
			t.Fatal("uncertain response replay changed receipt")
		}
		after, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		if len(after) != len(events) {
			t.Fatal("replay duplicated audit event")
		}
		s := v.snapshot(t)
		if len(s.Evidence) != 1 || strings.Contains(string(s.Evidence[0].Envelope.Payload), "sk-abcdefghijklmnopqrstuvwxyz") {
			t.Fatal("unsanitized evidence retained")
		}
		if store.VerificationEvidenceDigest(s.Evidence[0].Envelope) != s.Evidence[0].Digest {
			t.Fatal("digest does not cover retained sanitized envelope")
		}

		same := e
		same.Payload = core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: e.CapturedAt, Value: core.JSONPayload("sk-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ")})
		sanitizedReplay := v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: same.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(same)}})
		if !reflect.DeepEqual(first, sanitizedReplay) {
			t.Fatal("canonical sanitized replay changed receipt")
		}
		e.Payload = core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: e.CapturedAt, Value: core.JSONPayload("changed")})
		c.Evidence = []json.RawMessage{verificationBytes(e)}
		if _, err = x.Backend.ApplyVerification(v.ctx, v.command(c)); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("changed content: %v", err)
		}
		for _, access := range []store.VerificationAccess{func() store.VerificationAccess { a := v.access; a.TaskID = "foreign"; return a }(), func() store.VerificationAccess { a := v.access; a.WorkOrderID = "foreign"; return a }(), func() store.VerificationAccess { a := v.access; a.Claim.SessionID = "foreign"; return a }()} {
			if _, err = x.Backend.ReadVerification(v.ctx, access, v.contextID); !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("foreign scope: %v", err)
			}
		}
		if _, err = x.Backend.ReadVerification(store.WithWorkspace(v.ctx, "foreign"), v.access, v.contextID); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("foreign workspace: %v", err)
		}
	})
	t.Run("ChunkArtifactAtomicity", func(t *testing.T) {
		data := []byte("sanitized fixture artifact")
		chunk := store.VerificationUploadChunk{UploadID: "upload", Index: 0, Content: data[:10]}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})
		conflict := chunk
		conflict.Content = []byte("changed")
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &conflict})); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("chunk conflict: %v", err)
		}
		chunk.Index = 1
		chunk.Content = data[10:]
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})
		id := verificationSHA(data)
		e := v.envelope("artifact-evidence", "artifact-batch", "observed")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: id, SHA256: id, MediaType: "text/plain"}}
		c := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "upload", Name: "capture.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: id}}, Publication: &store.VerificationPublication{PullRequestNumber: 42, HeadSHA: strings.Repeat("a", 40), BodyDigest: verificationSHA([]byte("summary"))}})
		before := v.snapshot(t)
		events, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		queue, err := x.Backend.Log().Tail(v.ctx, x.Workspace, 0, 10000)
		requireOK(t, err)
		for _, point := range []string{"artifact", "evidence", "event", "queue"} {
			injected := errors.New("injected " + point)
			_, err = x.Backend.ApplyVerification(store.WithVerificationFault(v.ctx, func(step string) error {
				if step == point {
					return injected
				}
				return nil
			}), c)
			if !errors.Is(err, injected) {
				t.Fatalf("%s: %v", point, err)
			}
			if !reflect.DeepEqual(before, v.snapshot(t)) {
				t.Fatalf("%s left evidence state", point)
			}
			after, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
			requireOK(t, err)
			if len(after) != len(events) {
				t.Fatalf("%s left audit event", point)
			}
			q, err := x.Backend.Log().Tail(v.ctx, x.Workspace, 0, 10000)
			requireOK(t, err)
			if len(q) != len(queue) {
				t.Fatalf("%s left queue event", point)
			}
			artifacts, err := x.Backend.ListArtifacts(v.ctx)
			requireOK(t, err)
			for _, a := range artifacts {
				if a.ID == id {
					t.Fatalf("%s left artifact", point)
				}
			}
		}
		receipt, err := x.Backend.ApplyVerification(v.ctx, c)
		requireOK(t, err)
		if receipt.PublicationID == "" {
			t.Fatal("publication not atomic with evidence")
		}
		again, err := x.Backend.ApplyVerification(v.ctx, c)
		requireOK(t, err)
		if !reflect.DeepEqual(receipt, again) {
			t.Fatal("finalized upload replay required consumed chunks")
		}
		a, b, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, id)
		requireOK(t, err)
		if string(b) != string(data) || a.Role.ModelInputEligible() || a.EligibleVerificationEvidence() {
			t.Fatal("typed artifact crossed legacy/model-input boundary")
		}
		if _, _, err = x.Backend.ReadVerificationArtifact(v.ctx, v.access, "absent", id); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("hash-only retrieval: %v", err)
		}
	})
	runVerificationEdges(t, &v)
	t.Run("OperationHistoryAndTransitions", func(t *testing.T) {
		op := store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}
		c := store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "logical-operation", Operation: &op}
		first := v.apply(t, c)
		again := v.apply(t, c)
		if first.ID != again.ID {
			t.Fatal("operation replay identity")
		}
		c.Key = "duplicate-operation"
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(c)); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("unresolved operation uniqueness: %v", err)
		}
		obs := store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}
		c = store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: first.ID}, Observation: &obs}
		v.apply(t, c)
		v.apply(t, c)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "uncertain response"}})
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "blocked-retry", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("uncertain operation replay: %v", err)
		}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "succeeded"}})); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("terminal result rewrite: %v", err)
		}
		s := v.snapshot(t)
		if len(s.Operations) != 1 || len(s.Operations[0].History) != 3 || s.Operations[0].History[2].State != "outcome_unknown" {
			t.Fatal("operation history lost")
		}
	})
	runVerificationRetryAndSeal(t, &v)
	t.Run("RestartRetrieval", func(t *testing.T) {
		before := v.snapshot(t)
		if x.ReopenVerification != nil {
			fresh := x.ReopenVerification(t)
			after, err := fresh.ReadVerification(v.ctx, v.access, v.contextID)
			requireOK(t, err)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("restart changed persisted records")
			}
		} else if x.Backend.IsDurable() {
			t.Fatal("durable backend lacks restart fixture")
		} else {
			if !reflect.DeepEqual(before, v.snapshot(t)) {
				t.Fatal("volatile retrieval changed records")
			}
		}
	})
}

func runVerificationPublicationDelivery(t *testing.T, x Fixture) {
	for index, evidenceFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("PublicationDelivery/evidenceFirst=%t", evidenceFirst), func(t *testing.T) {
			v := newVerificationFixture(t, x)
			number := 4201 + index
			pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/publication", "number": number, "head_sha": strings.Repeat("a", 40)})}
			e := v.envelope("publication-evidence-"+fmt.Sprint(index), "publication-evidence", "private-provider-response https://private.example.test/artifact?token=secret")
			cmd := store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}}
			if evidenceFirst {
				v.apply(t, cmd)
			} else {
				requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
			}
			if len(v.snapshot(t).Publications) != 0 {
				t.Fatal("published without both PR and evidence")
			}
			if evidenceFirst {
				requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
			} else {
				v.apply(t, cmd)
			}
			first := v.snapshot(t)
			if len(first.Publications) != 1 || first.Publications[0].Delivery == nil {
				t.Fatalf("intent missing: %+v", first.Publications)
			}
			original := first.Publications[0]
			original.Delivery = nil
			beforeEvidence := verificationBytes(first.Evidence)
			beforeAttempts := verificationBytes(first.Attempts)
			args := store.VerificationDeliveryArgs(*first.Publications[0].Delivery)
			var current core.VerificationDelivery
			read := func() {
				t.Helper()
				requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, _ func(string, core.VerificationDelivery) error) error {
					current = *d
					return nil
				}))
			}
			read()
			if current.State != "pending" || current.Generation != 1 || strings.Contains(current.Summary, "private-provider") || strings.Contains(current.Summary, "private.example") {
				t.Fatalf("unsafe intent: %+v", current)
			}
			requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
			v.apply(t, cmd)
			requireOK(t, x.Backend.TranslateVerificationPublication(v.ctx, original))
			requireOK(t, x.Backend.ReconcileVerificationDeliveries(v.ctx))
			read()
			if current.Generation != 1 || len(v.snapshot(t).Publications) != 1 {
				t.Fatal("replay advanced generation")
			}
			interrupted := errors.New("simulated crash before forge")
			err := x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				if err := save("attempt", *d); err != nil {
					return err
				}
				return interrupted
			})
			if !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			read()
			if current.State != "retrying" || current.Attempts != 1 {
				t.Fatal("attempt did not survive interrupted delivery")
			}

			requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				if err := save("attempt", *d); err != nil {
					return err
				}
				next := *d
				next.ErrorClass = "forge"
				next.ErrorMessage = "private-provider-response"
				return save("fail", next)
			}))
			read()
			if current.State != "failed" || current.Attempts != 2 || strings.Contains(current.ErrorMessage, "private-provider") {
				t.Fatalf("failure metadata: %+v", current)
			}
			failed := v.snapshot(t)
			if string(verificationBytes(failed.Evidence)) != string(beforeEvidence) || string(verificationBytes(failed.Attempts)) != string(beforeAttempts) {
				t.Fatal("publication changed exercise/evidence")
			}
			retained := failed.Publications[0]
			retained.Delivery = nil
			if string(verificationBytes(retained)) != string(verificationBytes(original)) {
				t.Fatal("publication ledger changed")
			}
			ctx, owner := bootstrapOwner(t, x)
			page, err := x.Backend.(store.VerificationReader).ReadVerificationPage(ctx, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, store.VerificationPageRequest{Kind: "publications", ContextID: v.contextID, Limit: 1})
			requireOK(t, err)
			if len(page.Items) != 1 || page.Items[0].ID != original.ID || page.Items[0].State != "failed" || strings.Contains(string(page.Items[0].Metadata), "private-provider") {
				t.Fatalf("delivery read: %+v", page)
			}
			_, err = x.Backend.(store.VerificationReader).ReadVerificationPage(ctx, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, store.VerificationPageRequest{Kind: "publications", Limit: 1})
			if !errors.Is(err, store.ErrVerificationInvalid) {
				t.Fatalf("publication context boundary changed: %v", err)
			}
			requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				if err := save("retry", *d); err != nil {
					return err
				}
				if err := save("attempt", *d); err != nil {
					return err
				}
				next := *d
				next.ObservedHead = d.TargetHead
				next.ObservedDigest = d.TargetDigest
				next.ErrorClass = ""
				return save("publish", next)
			}))
			e2 := v.envelope("publication-new-"+fmt.Sprint(index), "publication-new", "new evidence")
			v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e2.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e2)}})
			read()
			if current.Generation != 2 || current.State != "pending" {
				t.Fatalf("new generation: %+v", current)
			}
			requireOK(t, x.Backend.TranslateVerificationPublication(v.ctx, original))
			read()
			if current.Generation != 2 {
				t.Fatal("legacy retry replaced latest")
			}
			snapshot := v.snapshot(t)
			if len(snapshot.Publications) != 2 {
				t.Fatal("source generation missing")
			}
			for _, p := range snapshot.Publications {
				if p.ID == original.ID && (p.Delivery == nil || p.Delivery.State != "superseded") {
					t.Fatal("old source not superseded")
				}
			}
			if err := x.Backend.RunVerificationDelivery(store.WithWorkspace(v.ctx, "foreign"), args, func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error { return nil }); err == nil {
				t.Fatal("foreign workspace accepted")
			}
			// Callback mutation cannot smuggle a different immutable identity into a save.
			if err := x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				d.TaskID = "foreign"
				return save("attempt", *d)
			}); err == nil {
				t.Fatal("forged delivery identity accepted")
			}
			read()
			if current.TaskID != v.access.TaskID || current.Attempts != 0 {
				t.Fatal("refused write leaked")
			}
			requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				if err := save("attempt", *d); err != nil {
					return err
				}
				next := *d
				next.ErrorClass = "forge"
				past := time.Now().UTC().Add(-time.Minute)
				next.NextAttemptAt = &past
				return save("fail", next)
			}))
			requireOK(t, x.Backend.ReconcileVerificationDeliveries(v.ctx))
			read()
			if current.State != "pending" || current.Attempts != 1 || current.CycleAttempts != 0 {
				t.Fatal("failed delivery did not retry independently")
			}

		})
	}
}

func runVerificationPublicationLegacyAndRace(t *testing.T, x Fixture) {
	t.Run("PublicationLegacyAndRace", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		p := store.VerificationPublication{PullRequestNumber: 4301, HeadSHA: strings.Repeat("a", 40), BodyDigest: verificationSHA([]byte("legacy digest"))}
		receipt := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreatePublication, Publication: &p})
		original := v.snapshot(t).Publications[0]
		if original.ID != receipt.PublicationID {
			t.Fatal("source identity missing")
		}
		_, err := logqueue.Enqueue(v.ctx, x.Backend.Log(), x.Workspace, "verification_publication", original.ID, original, 5, time.Now())
		requireOK(t, err)
		legacyBefore, err := logqueue.Load(v.ctx, x.Backend.Log(), x.Workspace, logqueue.StreamFor("verification_publication", original.ID))
		requireOK(t, err)
		e := v.envelope("legacy-race-evidence", "legacy-race-evidence", "private payload")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		pr := core.Event{TaskID: v.access.TaskID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "fixture/publication", "number": 4301, "head_sha": p.HeadSHA})}
		injected := errors.New("queue rollback")
		err = x.Backend.RecordVerificationPullRequest(store.WithVerificationFault(v.ctx, func(point string) error {
			if point == "queue" {
				return injected
			}
			return nil
		}), pr)
		if !errors.Is(err, injected) {
			t.Fatal("PR transaction ignored injected failure")
		}
		if len(v.snapshot(t).Publications) != 1 {
			t.Fatal("failed PR transaction left source rows")
		}
		requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
		requireOK(t, x.Backend.TranslateVerificationPublication(v.ctx, original))
		legacyAfter, err := logqueue.Load(v.ctx, x.Backend.Log(), x.Workspace, logqueue.StreamFor("verification_publication", original.ID))
		requireOK(t, err)
		if !reflect.DeepEqual(legacyBefore, legacyAfter) {
			t.Fatal("adapter rewrote legacy queue history")
		}
		args := queue.VerificationPublicationArgs{WorkspaceID: x.Workspace, Repository: "fixture/publication", PullRequestNumber: 4301}
		locked := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				if err := save("attempt", *d); err != nil {
					return err
				}
				close(locked)
				<-release
				if err := save("check", *d); err != nil {
					return err
				}
				next := *d
				next.ObservedHead = d.TargetHead
				next.ObservedDigest = d.TargetDigest
				return save("publish", next)
			})
		}()
		select {
		case <-locked:
		case err := <-done:
			t.Fatalf("worker failed before lock: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not acquire lock")
		}
		newer := v.envelope("race-new-evidence", "race-new-evidence", "newer payload")
		evidenceDone := make(chan error, 1)
		go func() {
			_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: newer.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(newer)}}))
			evidenceDone <- err
		}()
		select {
		case err := <-evidenceDone:
			close(release)
			t.Fatalf("evidence crossed held delivery lock: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		requireOK(t, <-done)
		requireOK(t, <-evidenceDone)
		var latest core.VerificationDelivery
		requireOK(t, x.Backend.RunVerificationDelivery(v.ctx, args, func(d *core.VerificationDelivery, _ func(string, core.VerificationDelivery) error) error {
			latest = *d
			return nil
		}))
		if latest.State != "pending" || latest.SourceGeneration != 3 {
			t.Fatalf("concurrent evidence acknowledged as published: %+v", latest)
		}
		for _, p := range v.snapshot(t).Publications {
			if p.ID == original.ID {
				p.Delivery = nil
				if !reflect.DeepEqual(p, original) {
					t.Fatal("legacy ledger bytes changed")
				}
			}
		}
	})
}
