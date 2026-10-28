package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func TestVerificationReadCorruptionIntegration(t *testing.T) {
	s := conformanceStore(t)
	ws := "verification-read-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunVerificationReadCorruption(t, storetest.Fixture{Backend: s, Context: ctx, Workspace: ws, Config: cfg}, func(ctx context.Context, id string, body []byte) error {
		// Simulate retained storage corruption below the application boundary.
		// The immutable guard correctly rejects normal updates, so this fixture
		// changes only its isolated database transaction's trigger mode.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE verification_evidence SET body=$3 WHERE workspace_id=$1 AND id=$2`, ws, id, body); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// verificationReadFixture is one verify claim with a context, selection,
// obligation, running attempt, plain evidence, and evidence with a linked
// artifact, all committed before a test takes any lock.
type verificationReadFixture struct {
	s                       *Store
	ws                      string
	system, member, worker  context.Context
	user, claim             store.VerificationAccess
	contextID, runID        string
	evidenceID, artifactEID string
	artifactID              string
	artifact                []byte
	subject                 core.VerificationSubject
	revisions               []core.VerificationRevision
	pins                    []core.VerificationPin
}

func newVerificationReadFixture(t *testing.T) *verificationReadFixture {
	t.Helper()
	s := conformanceStore(t)
	ws := "verification-read-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	if _, err := s.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// The owner matches the shared conformance identity, because
	// storetest.GrantVerificationFixture bootstraps that same identity.
	if _, err := s.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Conformance", Email: "owner@example.test", DisplayName: "Owner"}, "conformance-bootstrap"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.VerifyPersonalAccessToken(ctx, "conformance-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	member := store.WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	member = store.WithActor(member, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if caller, err := s.GetCallerIdentity(member, owner.ID, ws); err != nil || caller.Role != core.WorkspaceRoleOperator {
		if _, err = s.GrantWorkspaceRole(member, owner.Email, ws, core.WorkspaceRoleOperator); err != nil {
			t.Fatal(err)
		}
	}

	id := core.NewTaskID()
	head := strings.Repeat("a", 40)
	now := time.Now().UTC()
	task := core.Task{ID: id, Workspace: ws, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: head, CreatedAt: now}
	task.SetupContract.VerifyStage = true
	if err = s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: core.StageVerify, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if _, err = storetest.CreateStageWorkOrder(ctx, s, job, order); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verification", ClientToken: "verification-read-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
	claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	if order, err = storetest.ClaimWorkOrder(ctx, s, order.ID, claim); err != nil {
		t.Fatal(err)
	}
	f := &verificationReadFixture{
		s: s, ws: ws, system: ctx, member: member,
		worker:    store.WithActor(ctx, store.Actor{ID: "worker:worker", Role: core.ActorWorker}),
		user:      store.VerificationAccess{TaskID: id, UserID: owner.ID},
		claim:     store.VerificationAccess{TaskID: id, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}},
		revisions: []core.VerificationRevision{{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: head}},
		pins:      []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}},
	}
	f.contextID = f.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "prepare", Context: &store.VerificationContext{Revisions: f.revisions, GoverningPins: f.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": head, "state": "no_manifest"})}}}).ID
	selection := store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, ManifestRevision: head, SourceRevision: head, ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Stage: "verify", Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{}}, Subjects: []store.VerificationSubjectContract{}}
	f.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})
	obligation := store.VerificationObligation{ID: "ordinary", Description: "Observe the test state", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "ordinary", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
	digest := f.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation}).Digest
	f.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: digest}
	grant := storetest.GrantVerificationFixture(t, s, ctx, id, order.ID, f.contextID, "grant-start", f.subject, nil)
	coverage := store.VerificationCoverage{ObligationIDs: []string{"ordinary"}, Justification: "Assessment of pinned fixture source.", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Disposition: "covered", Explanation: "The registered fixture check covers this source.", Subjects: []core.VerificationSubject{f.subject}}}}
	f.runID = f.apply(t, store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "start-1", Coverage: &coverage, Attempt: &store.VerificationAttempt{Subject: f.subject, GrantID: grant, EffectiveActions: []core.VerificationPermission{}, SafeInputs: map[string]json.RawMessage{}, Environment: verificationReadEnvironment()}}).ID
	f.evidenceID = f.writeEvidence(t, "read-plain", nil)
	f.artifact = []byte("read lock artifact")
	f.artifactID = fmt.Sprintf("%x", sha256.Sum256(f.artifact))
	f.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "read-upload", Content: f.artifact}})
	f.artifactEID = f.writeEvidence(t, "read-artifact", []store.VerificationArtifactInput{{UploadID: "read-upload", Name: "capture.txt", ContentType: "text/plain", SizeBytes: int64(len(f.artifact)), SHA256: f.artifactID}})
	return f
}

func verificationReadEnvironment() core.VerificationEnvironment {
	return core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}
}

func (f *verificationReadFixture) apply(t *testing.T, c store.VerificationCommand) store.VerificationReceipt {
	t.Helper()
	c.Access, c.ContextID, c.RunID = f.claim, f.contextID, f.runID
	r, err := f.s.ApplyVerification(f.worker, c)
	if err != nil {
		t.Fatalf("apply %s: %v", c.Kind, err)
	}
	return r
}

// writeEvidence commits one state observation, with linked artifacts when
// inputs are given, and returns its evidence ID.
func (f *verificationReadFixture) writeEvidence(t *testing.T, id string, artifacts []store.VerificationArtifactInput) string {
	t.Helper()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	e := core.VerificationEvidence{SchemaVersion: 1, ID: id, SubmissionKey: id, Type: "state_observation", CapturedAt: at, ReceivedAt: at, SubmittedBy: "worker:worker", CapturedBy: core.VerificationCaptureActor{Identity: "fixture-tool", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: f.ws, TaskID: f.claim.TaskID, WorkOrderID: f.claim.WorkOrderID, WorkOrderAttemptID: f.claim.WorkOrderAttemptID, ContextID: f.contextID, RunID: f.runID, Subject: f.subject, Revisions: f.revisions, GoverningPins: f.pins, SafeInputs: map[string]json.RawMessage{}, Environment: verificationReadEnvironment(), Payload: core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: at, Value: core.JSONPayload("observed")})}
	for _, a := range artifacts {
		e.Artifacts = append(e.Artifacts, core.VerificationArtifactReference{ArtifactID: a.SHA256, SHA256: a.SHA256, MediaType: a.ContentType})
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	f.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: id, Evidence: []json.RawMessage{body}, Artifacts: artifacts})
	return id
}

// holdVerificationLocks takes the named locks in a separate committed-data
// transaction and returns only after PostgreSQL granted every one. The holder
// stays open until the test ends, after its reads are observed.
func (f *verificationReadFixture) holdVerificationLocks(t *testing.T, locks ...string) {
	t.Helper()
	tx, err := f.s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	advisory := func(key string) {
		if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
	}
	for _, lock := range locks {
		var found string
		switch lock {
		case "verification":
			advisory("conveyor:verification:" + f.ws)
		case "task-operation":
			advisory("conveyor:task-operation:" + f.ws + ":" + f.claim.TaskID)
		case "work-order-claim":
			advisory(fmt.Sprintf("conveyor:work-order-claim:%s:%s", f.ws, f.claim.TaskID))
		case "task-row":
			err = tx.QueryRow(t.Context(), "SELECT id FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE", f.ws, f.claim.TaskID).Scan(&found)
		case "order-row":
			err = tx.QueryRow(t.Context(), "SELECT id FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", f.ws, f.claim.WorkOrderID).Scan(&found)
		default:
			t.Fatalf("unknown lock %q", lock)
		}
		if err != nil {
			t.Fatalf("hold %s: %v", lock, err)
		}
	}
}

// bounded caps a read that would wait on a held lock; it never paces a test.
func bounded(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func pageIDs(page store.VerificationReadPage) []string {
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestVerificationReadLockIndependenceIntegration proves every inspection
// entrypoint completes while another transaction holds the locks verification
// commands take (component-verification-evidence "Bounded task inspection reads").
func TestVerificationReadLockIndependenceIntegration(t *testing.T) {
	for _, held := range [][]string{
		{"verification", "task-operation"},
		{"verification", "task-operation", "work-order-claim", "task-row", "order-row"},
		{"verification"},
		{"task-operation"},
		{"work-order-claim"},
		{"task-row"},
		{"order-row"},
	} {
		t.Run(strings.Join(held, "+"), func(t *testing.T) {
			f := newVerificationReadFixture(t)
			f.holdVerificationLocks(t, held...)
			member, worker := bounded(t, f.member), bounded(t, f.worker)
			// The selection names no kit, so its kit page is empty; assertions,
			// operations, and publications have no rows in this fixture.
			want := map[string][]string{
				"contexts":     {f.contextID},
				"selections":   {},
				"obligations":  {f.contextID + ":ordinary"},
				"attempts":     {f.runID},
				"evidence":     {f.artifactEID, f.evidenceID},
				"assertions":   {},
				"operations":   {},
				"publications": {},
			}
			for _, kind := range []string{"contexts", "selections", "obligations", "attempts", "evidence", "assertions", "operations", "publications"} {
				p := store.VerificationPageRequest{Kind: kind, ContextID: f.contextID}
				if kind == "contexts" {
					p.ContextID = ""
				}
				page, err := f.s.ReadVerificationPage(member, f.user, p)
				if err != nil {
					t.Fatalf("%s page under held locks: %v", kind, err)
				}
				if ids := pageIDs(page); !slices.Equal(slices.Sorted(slices.Values(ids)), slices.Sorted(slices.Values(want[kind]))) {
					t.Fatalf("%s page: got %v want %v", kind, ids, want[kind])
				}
			}
			detail, err := f.s.ReadVerificationDetail(member, f.user, f.contextID, f.evidenceID)
			if err != nil || detail.Envelope.ID != f.evidenceID {
				t.Fatalf("detail under held locks: %+v %v", detail.Envelope.ID, err)
			}
			for name, read := range map[string]struct {
				ctx    context.Context
				access store.VerificationAccess
			}{"member": {member, f.user}, "claim": {worker, f.claim}} {
				snapshot, err := f.s.ReadVerification(read.ctx, read.access, f.contextID)
				if err != nil || len(snapshot.Contexts) != 1 || snapshot.Contexts[0].ID != f.contextID || len(snapshot.Evidence) != 2 {
					t.Fatalf("%s overview under held locks: %d evidence %v", name, len(snapshot.Evidence), err)
				}
				meta, content, err := f.s.ReadVerificationArtifact(read.ctx, read.access, f.artifactEID, f.artifactID)
				if err != nil || !bytes.Equal(content, f.artifact) || meta.ID != f.artifactID {
					t.Fatalf("%s artifact under held locks: %v", name, err)
				}
			}
		})
	}
}

// TestVerificationReadSnapshotIntegration pauses a page read after its scope
// check, commits new evidence, and proves the paused read keeps its snapshot
// while the next read sees the commit.
func TestVerificationReadSnapshotIntegration(t *testing.T) {
	f := newVerificationReadFixture(t)
	ctx := bounded(t, f.member)
	paused, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	var page store.VerificationReadPage
	var readErr error
	hooked := context.WithValue(ctx, verificationReadScopedKey{}, func() {
		close(paused)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	go func() {
		defer close(done)
		page, readErr = f.s.ReadVerificationPage(hooked, f.user, store.VerificationPageRequest{Kind: "evidence", ContextID: f.contextID})
	}()
	defer func() {
		release()
		<-done
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("read never reached its scope check")
	}
	added := f.writeEvidence(t, "read-after-scope", nil)
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("paused read did not finish")
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := pageIDs(page); slices.Contains(got, added) || !slices.Contains(got, f.evidenceID) || !slices.Contains(got, f.artifactEID) {
		t.Fatalf("paused read mixed snapshots: %v", got)
	}
	next, err := f.s.ReadVerificationPage(ctx, f.user, store.VerificationPageRequest{Kind: "evidence", ContextID: f.contextID})
	if err != nil || !slices.Contains(pageIDs(next), added) {
		t.Fatalf("next read missed the commit: %v %v", pageIDs(next), err)
	}
}

// TestVerificationReadScopeRefusalIntegration keeps the generic refusal for
// every scope the lock-free check must still deny.
func TestVerificationReadScopeRefusalIntegration(t *testing.T) {
	f := newVerificationReadFixture(t)
	ctx := bounded(t, f.member)
	foreign := core.NewTaskID()
	if err := f.s.CreateTask(f.system, core.Task{ID: foreign, Workspace: f.ws, Repo: "conveyor", Title: foreign, BaseBranch: "main", Branch: "conveyor/task-" + foreign, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	outsider := bounded(t, store.WithActor(f.system, store.Actor{ID: store.UserActorID("outsider"), Role: core.ActorUser}))
	wrongToken := f.claim
	wrongToken.ClientToken = "wrong-token"
	type read struct {
		ctx    context.Context
		access store.VerificationAccess
		ctxID  string
		evID   string
		artID  string
	}
	for name, r := range map[string]read{
		"no membership":       {outsider, store.VerificationAccess{TaskID: f.user.TaskID, UserID: "outsider"}, f.contextID, f.artifactEID, f.artifactID},
		"actor mismatch":      {ctx, store.VerificationAccess{TaskID: f.user.TaskID, UserID: "outsider"}, f.contextID, f.artifactEID, f.artifactID},
		"other workspace":     {store.WithWorkspace(ctx, "other-workspace"), f.user, f.contextID, f.artifactEID, f.artifactID},
		"missing task":        {ctx, store.VerificationAccess{TaskID: "missing-task", UserID: f.user.UserID}, f.contextID, f.artifactEID, f.artifactID},
		"foreign task":        {ctx, store.VerificationAccess{TaskID: foreign, UserID: f.user.UserID}, f.contextID, f.artifactEID, f.artifactID},
		"foreign context":     {ctx, f.user, "foreign-context", f.artifactEID, f.artifactID},
		"foreign evidence":    {ctx, f.user, f.contextID, "foreign-evidence", f.artifactID},
		"foreign artifact":    {ctx, f.user, f.contextID, f.artifactEID, strings.Repeat("0", 64)},
		"wrong claim token":   {bounded(t, f.worker), wrongToken, f.contextID, f.artifactEID, f.artifactID},
		"claim actor differs": {ctx, f.claim, f.contextID, f.artifactEID, f.artifactID},
	} {
		t.Run(name, func(t *testing.T) {
			denied := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, store.ErrVerificationAccess) {
					t.Fatalf("%s: got %v, want generic access refusal", what, err)
				}
			}
			scopeDenied := name != "foreign context" && name != "foreign evidence" && name != "foreign artifact"
			if r.access.UserID != "" {
				_, err := f.s.ReadVerificationPage(r.ctx, r.access, store.VerificationPageRequest{Kind: "evidence", ContextID: r.ctxID})
				if scopeDenied || name == "foreign context" {
					denied("page", err)
				}
				_, err = f.s.ReadVerificationDetail(r.ctx, r.access, r.ctxID, r.evID)
				if name != "foreign artifact" {
					denied("detail", err)
				}
			}
			snapshot, err := f.s.ReadVerification(r.ctx, r.access, r.ctxID)
			if scopeDenied || name == "foreign context" {
				denied("overview", err)
				if len(snapshot.Contexts) != 0 {
					t.Fatal("refused overview carried records")
				}
			}
			_, content, err := f.s.ReadVerificationArtifact(r.ctx, r.access, r.evID, r.artID)
			if name != "foreign context" {
				denied("artifact", err)
				if len(content) != 0 {
					t.Fatal("refused artifact carried bytes")
				}
			}
		})
	}
}
