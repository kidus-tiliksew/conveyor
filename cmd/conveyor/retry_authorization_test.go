package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"gopkg.in/yaml.v3"
)

// retryAuthorizationStore is a volatile backend holding one verify order
// whose first context ended an operator_action_required attempt and three
// safe_to_replay attempts. An operator recovered the gated attempt with a
// disposition, and the order was claimed again with a successor context.
type retryAuthorizationStore struct {
	b                    store.Backend
	service              *workorder.Service
	worker, operator     context.Context
	ownerID              string
	order                core.WorkOrder
	access               store.VerificationAccess
	oldContext, oldRun   string
	contextID            string
	gated                string
	obligations          []store.VerificationObligation
	authorization, token string
}

func newRetryAuthorizationStore(t *testing.T, head string, gated verification.Exercise, safe []verification.Exercise) *retryAuthorizationStore {
	t.Helper()
	b := store.NewVolatileBackend()
	t.Cleanup(b.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err := b.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "retry-authorization", Workspace: "demo", Repo: "repo", Title: "retry authorization", Branch: "conveyor/retry-authorization", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: head, CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	if err := b.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	if _, err := storetest.CreateStageWorkOrder(ctx, b, job, order); err != nil {
		t.Fatal(err)
	}
	requirements := []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	s := &retryAuthorizationStore{b: b, service: &workorder.Service{Store: b, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}, worker: store.WithActor(ctx, store.Actor{ID: store.UserActorID("worker"), Role: core.ActorUser}), authorization: "recovery:runner-recovery"}
	s.claim(t, ctx, order.ID, "first", "first-token", requirements)
	if _, err := b.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}, "retry-operator-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := b.VerifyPersonalAccessToken(ctx, "retry-operator-token")
	if err != nil {
		t.Fatal(err)
	}
	s.ownerID = owner.ID
	s.operator = store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "owner", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator, OwnerUserID: owner.ID}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = b.GrantWorkspaceRole(s.operator, owner.Email, "demo", core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	s.gated = gated.ID
	s.obligations = append(s.obligations, store.VerificationObligation{ID: gated.ID, Description: "Exercise retried only on operator authority", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: gated})
	for _, e := range safe {
		s.obligations = append(s.obligations, store.VerificationObligation{ID: e.ID, Description: "Exercise that is safe to replay", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: e})
	}
	// The first context ends one attempt of every subject.
	s.prepare(t, head, "first")
	for i, subject := range s.subjects(t) {
		start := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: fmt.Sprintf("first-%d", i), Attempt: &store.VerificationAttempt{Subject: subject, GrantID: s.grant(t, subject), EffectiveActions: []core.VerificationPermission{}, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "repo", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}}
		r := s.apply(t, start)
		if subject.ObligationID == s.gated {
			s.oldRun = r.ID
		}
		s.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, RunID: r.ID, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "environment unavailable"}})
	}
	s.oldContext = s.contextID
	// The order is released at a checkpoint and an operator recovers the
	// gated attempt with a disposition.
	order, err = b.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	if err = storetest.UpdateWorkOrder(ctx, b, order, core.WorkOrderCmdRelease); err != nil {
		t.Fatal(err)
	}
	disposition := &store.VerificationRecovery{Reason: "Operator restored the environment and authorizes one retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: s.oldContext, RunID: s.oldRun, OperationIDs: []string{}, InputDigest: store.VerificationSafeInputDigest(map[string]json.RawMessage{}), Disposition: "not_applied"}}}
	if _, err = storetest.RecoverWorkOrder(store.WithVerificationRecovery(s.operator, disposition), b, order.ID, "runner-recovery", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.claim(t, ctx, order.ID, "successor", "successor-token", order.ServedRequirementSnapshot)
	s.prepare(t, head, "successor")
	for _, subject := range s.subjects(t) {
		s.grant(t, subject)
	}
	if a := s.recovered(t); a.Successor != nil {
		t.Fatalf("recovery authorization already consumed: %+v", a.Successor)
	}
	return s
}

func (s *retryAuthorizationStore) claim(t *testing.T, ctx context.Context, orderID, session, token string, requirements []core.ServedRequirementContext) {
	t.Helper()
	order, err := storetest.ClaimWorkOrder(ctx, s.b, orderID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: session, ClientToken: token, Requirements: requirements, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.order, s.token = order, token
	s.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, ClientToken: token, Claim: core.WorkOrderClaimIdentity{ClaimantID: order.ClaimantID, SessionID: session}}
}

// prepare creates one no-manifest context at head with every obligation.
func (s *retryAuthorizationStore) prepare(t *testing.T, head, key string) {
	t.Helper()
	revisions := []core.VerificationRevision{{Repository: "repo", RemoteIdentity: "https://example.test/repo", SHA: head}}
	s.contextID = ""
	s.contextID = s.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: key, Context: &store.VerificationContext{Revisions: revisions, GoverningPins: []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "repo", "revision": head, "state": "no_manifest"})}}}).ID
	s.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, ManifestRevision: head, SourceRevision: head, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{}}, Subjects: []store.VerificationSubjectContract{}}})
	for i := range s.obligations {
		s.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &s.obligations[i]})
	}
}

func (s *retryAuthorizationStore) apply(t *testing.T, c store.VerificationCommand) store.VerificationReceipt {
	t.Helper()
	c.Access = s.access
	if c.ContextID == "" {
		c.ContextID = s.contextID
	}
	if c.Kind == store.VerificationStartAttempt {
		coverage := s.coverage(t)
		c.Coverage = &coverage
	}
	r, err := s.b.ApplyVerification(s.worker, c)
	if err != nil {
		t.Fatalf("%s: %v", c.Kind, err)
	}
	return r
}

func (s *retryAuthorizationStore) snapshot(t *testing.T) store.VerificationSnapshot {
	t.Helper()
	snapshot, err := s.b.ReadVerification(s.worker, s.access, s.contextID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (s *retryAuthorizationStore) subjects(t *testing.T) []core.VerificationSubject {
	t.Helper()
	var out []core.VerificationSubject
	for _, o := range s.snapshot(t).Obligations {
		out = append(out, core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest})
	}
	return out
}

// coverage maps the context's one requirement pin to every obligation.
func (s *retryAuthorizationStore) coverage(t *testing.T) store.VerificationCoverage {
	t.Helper()
	subjects := s.subjects(t)
	c := store.VerificationCoverage{ObligationIDs: []string{}, Justification: "Every registered fixture obligation covers the pinned source.", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Disposition: "covered", Explanation: "The registered fixture checks cover this source.", Subjects: subjects}}}
	for _, subject := range subjects {
		c.ObligationIDs = append(c.ObligationIDs, subject.ObligationID)
	}
	return c
}

// grant opens an operator permission grant for subject in the current
// context and returns its ID.
func (s *retryAuthorizationStore) grant(t *testing.T, subject core.VerificationSubject) string {
	t.Helper()
	key := "grant-" + s.contextID + "-" + subject.ObligationID
	request := store.VerificationPermissionRequest{ContextID: s.contextID, RequestKey: key, Subject: subject, Actions: []core.VerificationPermission{}}
	r, err := s.b.ApplyVerification(s.operator, store.VerificationCommand{Access: store.VerificationAccess{UserID: s.ownerID, TaskID: s.access.TaskID, WorkOrderID: s.access.WorkOrderID}, Kind: store.VerificationGrantPermissions, ContextID: s.contextID, Key: key, Permissions: &request})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// recovered returns the one authorization on the recovered gated attempt.
func (s *retryAuthorizationStore) recovered(t *testing.T) store.VerificationReplayAuthorization {
	t.Helper()
	snapshot, err := s.b.ReadVerification(s.operator, store.VerificationAccess{TaskID: s.access.TaskID, UserID: s.ownerID}, s.oldContext)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range snapshot.Attempts {
		if attempt.ID == s.oldRun {
			if len(attempt.Recovery) != 1 || attempt.Recovery[0].ID != s.authorization {
				t.Fatalf("recovered attempt authorizations = %+v", attempt.Recovery)
			}
			return attempt.Recovery[0]
		}
	}
	t.Fatalf("recovered attempt %s is missing", s.oldRun)
	return store.VerificationReplayAuthorization{}
}

// retryAuthorizationStart is one start_verification_attempt call that the
// real store answered.
type retryAuthorizationStart struct {
	request workorder.VerificationStartRequest
	receipt store.VerificationReceipt
	err     error
}

// serveRealStarts answers start_verification_attempt through the real
// verification service over s and forwards every other runner call to the
// kit execution fixture f, whose context snapshot then lists each admitted
// attempt as its own start handler would.
func serveRealStarts(t *testing.T, s *retryAuthorizationStore, f *kitExecutionFixture) (string, func() []retryAuthorizationStart) {
	fixtureURL := f.v.rpc.client.base
	t.Helper()
	var mu sync.Mutex
	var starts []retryAuthorizationStart
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var call struct {
			Params struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err = json.Unmarshal(body, &call); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if call.Params.Name != "start_verification_attempt" {
			response, err := http.Post(fixtureURL+r.URL.Path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer response.Body.Close()
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		identity := map[string]string{}
		for _, name := range []string{"workspace_id", "work_order_id", "session_id", "client_token"} {
			var value string
			_ = json.Unmarshal(call.Params.Arguments[name], &value)
			identity[name] = value
			delete(call.Params.Arguments, name)
		}
		raw, _ := json.Marshal(call.Params.Arguments)
		var request workorder.VerificationStartRequest
		_ = json.Unmarshal(raw, &request)
		result, err := s.service.Verification(s.worker, identity["work_order_id"], identity["session_id"], identity["client_token"], call.Params.Name, raw)
		start := retryAuthorizationStart{request: request, err: err}
		text := ""
		if err != nil {
			text = err.Error()
		} else {
			start.receipt = result.(store.VerificationReceipt)
			text = string(core.JSONPayload(result))
			f.mu.Lock()
			f.snapshot.Attempts = append(f.snapshot.Attempts, store.VerificationAttempt{ID: start.receipt.ID, ContextID: request.ContextID, StartKey: request.StartKey, Subject: request.Subject, GrantID: request.GrantID, State: "running"})
			f.mu.Unlock()
		}
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": err != nil, "content": []map[string]string{{"type": "text", "text": text}}}})
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []retryAuthorizationStart {
		mu.Lock()
		defer mu.Unlock()
		return append([]retryAuthorizationStart(nil), starts...)
	}
}

// TestKitVerifyReplayAuthorizationStartsEverySubject is the runner
// regression for one operator retry authorization: a single
// `conveyor kit verify --replay-authorization recovery:<request_id>` run
// sends the ID on every subject. The safe_to_replay subjects start first and
// consume nothing, and the operator_action_required subject is then admitted
// through the real store (component-verification-service invariant 19;
// req-verification-kits AC-3.4).
func TestKitVerifyReplayAuthorizationStartsEverySubject(t *testing.T) {
	exercise := func(id, policy string) verification.Exercise {
		e := verification.Exercise{ID: id, Kind: "script", Argv: []string{"true"}, Cwd: ".", TimeoutSeconds: 5, RequiredAssertions: []verification.Assertion{}, Operations: []verification.Operation{}, RetryPolicy: policy}
		if policy == "safe_to_replay" {
			e.SafetyBasis = "read-only observation"
		}
		return e
	}
	// Obligations are listed by ID, so the gated subject starts last.
	gated := exercise("z-gated", "operator_action_required")
	safe := []verification.Exercise{exercise("a-safe-0", "safe_to_replay"), exercise("a-safe-1", "safe_to_replay"), exercise("a-safe-2", "safe_to_replay")}
	f := newKitExecutionFixture(t, gated)
	options := ordinaryKitVerifyOptions(t, f, "no_manifest")
	s := newRetryAuthorizationStore(t, f.order.HeadSHA, gated, safe)

	// The fixture serves the runner's other calls for the real claim.
	f.mu.Lock()
	f.order.ID, f.order.SessionID, f.order.AttemptID = s.order.ID, s.order.SessionID, s.order.AttemptID
	f.snapshot = s.snapshot(t)
	f.mu.Unlock()
	proxy, starts := serveRealStarts(t, s, f)
	rpc := kitRPC{client: &client{base: proxy, token: "factory-secret-fixture", workspace: "demo"}, order: s.order.ID, session: s.order.SessionID, claimToken: s.token}

	dir := t.TempDir()
	cfg := config.Config{KitPermissions: []config.KitPermissionGrant{{Server: proxy, Workspace: "demo", Repository: "repo", Binding: "repo", Actions: []verification.VerificationPermission{}}}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	options.configPath = filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(options.configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	options.coveragePath = filepath.Join(dir, "coverage.json")
	if err = os.WriteFile(options.coveragePath, core.JSONPayload(s.coverage(t)), 0600); err != nil {
		t.Fatal(err)
	}
	options.replayAuthorization = s.authorization

	if err = verifyKits(t.Context(), rpc, f.v.root, "task", options, &f.output); err != nil {
		t.Fatalf("kit verify: %v\n%s", err, f.output.String())
	}
	recorded := starts()
	if len(recorded) != 1+len(safe) {
		t.Fatalf("runner made %d starts, want %d", len(recorded), 1+len(safe))
	}
	runs := map[string]string{}
	for i, start := range recorded {
		if start.err != nil || !start.receipt.LaunchAuthorized {
			t.Fatalf("start %d of %s: receipt %+v, error %v", i, start.request.Subject.ObligationID, start.receipt, start.err)
		}
		if start.request.ReplayAuthorizationID != s.authorization {
			t.Fatalf("start of %s sent authorization %q, want %q", start.request.Subject.ObligationID, start.request.ReplayAuthorizationID, s.authorization)
		}
		runs[start.request.Subject.ObligationID] = start.receipt.ID
	}
	if last := recorded[len(recorded)-1].request.Subject.ObligationID; last != gated.ID {
		t.Fatalf("the runner started %s last, want the gated subject after every ungated one", last)
	}
	for _, attempt := range s.snapshot(t).Attempts {
		want := ""
		if attempt.Subject.ObligationID == gated.ID {
			want = s.authorization
		}
		if attempt.ID != runs[attempt.Subject.ObligationID] || attempt.ReplayAuthorizationID != want {
			t.Fatalf("attempt %s of %s records authorization %q, want %q", attempt.ID, attempt.Subject.ObligationID, attempt.ReplayAuthorizationID, want)
		}
	}
	if a := s.recovered(t); a.Successor == nil || a.Successor.RunID != runs[gated.ID] {
		t.Fatalf("authorization successor = %+v, want the gated run %s", a.Successor, runs[gated.ID])
	}
	if strings.Contains(f.output.String(), "operator_action_required") {
		t.Fatalf("the run printed an operator checkpoint:\n%s", f.output.String())
	}
}
