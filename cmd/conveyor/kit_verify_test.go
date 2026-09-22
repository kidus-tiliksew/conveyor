package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"gopkg.in/yaml.v3"
)

type kitExecutionFixture struct {
	order           core.WorkOrder
	starts          int
	operations      []string
	mutations       int
	refuseDispatch  bool
	v               *kitVerifier
	mu              sync.Mutex
	snapshot        store.VerificationSnapshot
	outcome         string
	uploads         int
	loseClaim       bool
	offline         bool
	renewDelayAfter int
	renewCalls      int
	output          bytes.Buffer
}

func newKitExecutionFixture(t *testing.T, e verification.Exercise) *kitExecutionFixture {
	t.Helper()
	root, _ := kitCLIRepo(t)
	if _, err := localKitGit(t.Context(), root, "remote", "add", "origin", "https://example.test/repo"); err != nil {
		t.Fatal(err)
	}
	head, err := localKitGit(t.Context(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: "order", TaskID: "task", SessionID: "session", AttemptID: "claim", Stage: core.StageVerify, State: core.WorkOrderClaimed, HeadSHA: strings.TrimSpace(string(head)), LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionDeadline: time.Now().Add(time.Minute)}
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: e.ID, ContractDigest: strings.Repeat("a", 64)}
	vc := store.VerificationContext{ID: "context", WorkspaceID: "demo", TaskID: "task", WorkOrderID: "order", WorkOrderAttemptID: "claim", Revisions: []core.VerificationRevision{{Repository: "repo", RemoteIdentity: "https://example.test/repo", SHA: order.HeadSHA}}, GoverningPins: []core.VerificationPin{}}
	f := &kitExecutionFixture{order: order, snapshot: store.VerificationSnapshot{Contexts: []store.VerificationContext{vc}, Obligations: []store.VerificationObligation{{ID: e.ID, Digest: subject.ContractDigest, Contract: e}}, Attempts: []store.VerificationAttempt{{ID: "run", ContextID: "context", Subject: subject, State: "running", GrantID: "grant"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/provider" {
			if len(f.operations) < 2 || f.operations[len(f.operations)-1] != "dispatching" {
				t.Error("provider mutation preceded durable dispatch acknowledgement")
			}
			f.mutations++
			w.WriteHeader(200)
			return
		}
		var request struct {
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if f.offline {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var result any
		failed := false
		switch request.Params.Name {
		case "renew_work_order":
			f.renewCalls++
			if f.renewDelayAfter > 0 && f.renewCalls > f.renewDelayAfter {
				time.Sleep(1500 * time.Millisecond)
			}
			result = f.order
			failed = f.loseClaim
		case "get_work_order":
			result = map[string]any{"work_order": f.order, "task": core.Task{ID: "task", Repo: "repo"}}
		case "prepare_verification":
			result = f.snapshot
		case "start_verification_attempt":
			var args workorder.VerificationStartRequest
			_ = json.Unmarshal(request.Params.Arguments, &args)
			f.starts++
			f.snapshot.Attempts = append(f.snapshot.Attempts, store.VerificationAttempt{ID: "run", ContextID: "context", StartKey: args.StartKey, Subject: args.Subject, GrantID: args.GrantID, State: "running"})
			result = store.VerificationReceipt{ID: "run", State: "running", LaunchAuthorized: true}
		case "report_progress":
			result = f.order
		case "get_verification_context":
			result = f.snapshot
		case "submit_verification_evidence":
			var args struct {
				Evidence []core.VerificationEvidence `json:"evidence"`
			}
			_ = json.Unmarshal(request.Params.Arguments, &args)
			for _, item := range args.Evidence {
				item.SubmittedBy = "worker:test"
				if err := item.Validate(core.VerificationEvidenceAuthority{SubmittedBy: item.SubmittedBy}); err != nil {
					t.Errorf("invalid runner evidence: %v", err)
					failed = true
				}
				f.snapshot.Evidence = append(f.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: item})
			}
			f.uploads++
			result = store.VerificationReceipt{ID: "evidence"}
		case "prepare_verification_operation":
			var args workorder.VerificationOperationRequest
			_ = json.Unmarshal(request.Params.Arguments, &args)
			f.operations = append(f.operations, args.Action)
			failed = args.Action == "dispatching" && f.refuseDispatch
			result = store.VerificationReceipt{ID: "operation", State: args.Action, DispatchAuthorized: args.Action == "dispatching" && !failed}
			if args.Action == "completed" {
				f.snapshot.Operations = append(f.snapshot.Operations, store.VerificationOperation{ID: "operation", ContextID: "context", RunID: "run", StepID: "create", Target: "fixture", History: []store.VerificationOperationObservation{{State: "completed"}}})
			}
		case "report_verification_outcome":
			var args workorder.VerificationOutcomeRequest
			_ = json.Unmarshal(request.Params.Arguments, &args)
			f.outcome = args.State
			result = store.VerificationReceipt{ID: "run", State: args.State}
		default:
			t.Errorf("unexpected runner RPC %s", request.Params.Name)
			failed = true
		}
		text, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": failed, "content": []map[string]string{{"type": "text", "text": string(text)}}}})
	}))
	t.Cleanup(server.Close)
	f.v = &kitVerifier{rpc: kitRPC{client: &client{base: server.URL, token: "factory-secret-fixture", workspace: "demo"}, order: "order", session: "session", claimToken: "claim-secret-fixture"}, root: root, order: order, task: core.Task{ID: "task", Repo: "repo"}, snapshot: f.snapshot, output: &f.output, redactor: redact.New([]string{"factory-secret-fixture", "claim-secret-fixture", "credential-secret-fixture"})}
	return f
}

func TestKitRunnerExecutionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, script, kind, want string
		timeout                  int
	}{
		{"script", "exit 0", "script", "succeeded", 5},
		{"failure", "exit 7", "script", "failed", 5},
		{"timeout", "sleep 30 & wait", "script", "timed_out", 1},
		{"interactive waits", "exit 0", "interactive", "waiting", 5},
		{"changed source", "touch changed-source", "script", "blocked", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := verification.Exercise{ID: "check", Kind: tc.kind, Argv: []string{"sh", "-c", tc.script}, TimeoutSeconds: tc.timeout, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
			f := newKitExecutionFixture(t, e)
			subject := f.snapshot.Attempts[0].Subject
			environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
			err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", subject, environment, []string{"PATH=/usr/bin:/bin"})
			if f.outcome != tc.want {
				t.Fatalf("outcome %q want %q: %v", f.outcome, tc.want, err)
			}
			if tc.want == "succeeded" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "succeeded" && err == nil {
				t.Fatal("non-success returned success")
			}
			if f.uploads != 1 || len(f.snapshot.Evidence) != 1 {
				t.Fatalf("execution reports: %d", f.uploads)
			}
		})
	}
}

func TestKitRunnerCancellationAndClaimLoss(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", "sleep 30 & wait"}, TimeoutSeconds: 10, RequiredAssertions: []string{}}
			f := newKitExecutionFixture(t, e)
			if lost {
				time.AfterFunc(200*time.Millisecond, func() { f.mu.Lock(); f.loseClaim = true; f.mu.Unlock() })
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if !lost {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			env := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
			started := time.Now()
			err := f.v.launch(ctx, e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, []string{"PATH=/usr/bin:/bin"})
			if err == nil || time.Since(started) > 8*time.Second {
				t.Fatalf("teardown: %v duration %s", err, time.Since(started))
			}
			if lost {
				if f.uploads != 0 || f.outcome != "" || !strings.Contains(f.output.String(), "execution_report") {
					t.Fatalf("writes after loss or report absent: uploads=%d state=%s", f.uploads, f.outcome)
				}
			} else if f.outcome != "cancelled" {
				t.Fatalf("cancellation outcome %q: %v", f.outcome, err)
			}
		})
	}
}

func TestKitRunnerChildEnvironmentAndMissingCredentials(t *testing.T) {
	t.Setenv("CONVEYOR_API_TOKEN", "factory-secret-fixture")
	t.Setenv("CONVEYOR_CLIENT_TOKEN", "claim-secret-fixture")
	t.Setenv("GH_TOKEN", "forge-secret-fixture")
	t.Setenv("OPENAI_API_KEY", "parent-secret-fixture")
	e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", `test -z "$CONVEYOR_API_TOKEN$CONVEYOR_CLIENT_TOKEN$GH_TOKEN$OPENAI_API_KEY" && test -n "$CONVEYOR_KIT_OPERATIONS"`}, TimeoutSeconds: 5, RequiredAssertions: []string{}}
	f := newKitExecutionFixture(t, e)
	env := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
	if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	e.Prerequisites = []verification.Prerequisite{{ID: "api", Kind: "credential", EnvironmentBinding: "api"}}
	if _, _, _, err := kitExerciseActions(e, f.v.root, "repo", nil); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("missing credential: %v", err)
	}
	grants := []verification.VerificationPermission{{Kind: "credential", Binding: "api", Target: "CONVEYOR_API_TOKEN"}}
	if _, _, _, err := kitExerciseActions(e, f.v.root, "repo", grants); err == nil {
		t.Fatal("factory credential accepted")
	}
}

func TestKitRunnerRedactsBeforeSpool(t *testing.T) {
	e := verification.Exercise{ID: "check"}
	f := newKitExecutionFixture(t, e)
	payload := core.JSONPayload(map[string]any{"method": "GET", "request_at": time.Now().UTC().Format(time.RFC3339Nano), "response_at": time.Now().UTC().Format(time.RFC3339Nano), "request_summary": "fixture", "response_status": 200, "response_headers": map[string]string{}, "url": "https://user:password@example.test/path?token=credential-secret-fixture", "request_headers": map[string]string{"Authorization": "credential-secret-fixture"}, "response_summary": "credential-secret-fixture"})
	data, err := f.v.evidenceBatch("run", f.snapshot.Attempts[0].Subject, core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown"}, "exchange", "api_exchange", payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-secret-fixture", "user:password", "?token=", "Authorization"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("sensitive data retained: %s", secret)
		}
	}
}

func TestKitRunnerExactCheckout(t *testing.T) {
	f := newKitExecutionFixture(t, verification.Exercise{ID: "check"})
	if err := f.v.checkCheckout(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.v.order.HeadSHA = strings.Repeat("b", 40)
	if err := f.v.checkCheckout(t.Context()); err == nil {
		t.Fatal("new revision accepted")
	}
	head, _ := localKitGit(t.Context(), f.v.root, "rev-parse", "HEAD")
	f.v.order.HeadSHA = strings.TrimSpace(string(head))
	if err := os.WriteFile(filepath.Join(f.v.root, "dirty"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.v.checkCheckout(t.Context()); err == nil {
		t.Fatal("dirty checkout accepted")
	}
}

func TestKitRunnerDurableOperationRelay(t *testing.T) {
	fixture, err := filepath.Abs("testdata/verification-kit-runner/instrumented.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprint(refused), func(t *testing.T) {
			e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"python3", fixture}, TimeoutSeconds: 10, RequiredAssertions: []string{}, Operations: []verification.Operation{{ID: "create", TargetBinding: "fixture"}}}
			f := newKitExecutionFixture(t, e)
			f.refuseDispatch = refused
			env := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
			err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, []string{"PATH=/usr/bin:/bin", "FIXTURE_PROVIDER_URL=" + f.v.rpc.client.base + "/provider"})
			if refused {
				if err == nil || f.mutations != 0 || f.outcome != "blocked" {
					t.Fatalf("refused dispatch: mutations=%d state=%s err=%v", f.mutations, f.outcome, err)
				}
			} else if err != nil || f.mutations != 1 || f.outcome != "succeeded" {
				t.Fatalf("durable relay: mutations=%d state=%s err=%v", f.mutations, f.outcome, err)
			}
		})
	}
}

func TestKitRunnerUninstrumentedMutationIsBlocked(t *testing.T) {
	e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", "exit 0"}, TimeoutSeconds: 5, RequiredAssertions: []string{}, Operations: []verification.Operation{{ID: "create", TargetBinding: "fixture"}}}
	f := newKitExecutionFixture(t, e)
	env := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
	if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, env, []string{"PATH=/usr/bin:/bin"}); err == nil || f.outcome != "blocked" {
		t.Fatalf("uninstrumented mutation: %s %v", f.outcome, err)
	}
}

func TestKitVerifyOrdinaryObligationsAndReplay(t *testing.T) {
	for _, discovery := range []string{"no_manifest", "no_eligible_kits"} {
		t.Run(discovery, func(t *testing.T) {
			e := verification.Exercise{ID: "ordinary", Kind: "script", Argv: []string{"true"}, Cwd: ".", TimeoutSeconds: 5, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
			f := newKitExecutionFixture(t, e)
			subject := f.snapshot.Attempts[0].Subject
			f.snapshot.Attempts = nil
			if discovery == "no_manifest" {
				if _, err := localKitGit(t.Context(), f.v.root, "rm", ".conveyor/kits/manifest.yaml"); err != nil {
					t.Fatal(err)
				}
				if _, err := localKitGit(t.Context(), f.v.root, "commit", "-qm", "no manifest"); err != nil {
					t.Fatal(err)
				}
				head, err := localKitGit(t.Context(), f.v.root, "rev-parse", "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				f.order.HeadSHA = strings.TrimSpace(string(head))
				f.snapshot.Contexts[0].Revisions[0].SHA = f.order.HeadSHA
			} else {
				f.snapshot.Selections = []store.VerificationSelection{{Receipt: verification.SelectionReceipt{Kits: []verification.KitReceipt{{KitID: "sample", Eligibility: "ineligible", Reasons: []verification.SelectionReason{{Code: "pin_mismatch"}}}}}}}
			}
			f.snapshot.PermissionGrants = []store.VerificationPermissionGrant{{ID: "grant", Subject: subject, Actions: []core.VerificationPermission{}}}
			cfg := config.Config{KitPermissions: []config.KitPermissionGrant{{Server: f.v.rpc.client.base, Workspace: "demo", Repository: "repo", Binding: "repo", Actions: []verification.VerificationPermission{}}}}
			harness := config.HarnessTemplates()[0].Harness
			document := localExecutionDocument("demo", newExecutionWizardState(healthyDetections(harness), nil).choices, []config.Harness{harness})
			cfg.ExecutionSettings, cfg.Harnesses, cfg.Review = document.ExecutionSettings, document.Harnesses, document.Review
			cfgBytes, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfgPath, cfgBytes, 0600); err != nil {
				t.Fatal(err)
			}
			coverage := store.VerificationCoverage{ObligationIDs: []string{"ordinary"}, Justification: "ordinary checks remain required", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "ordinary command", Subjects: []core.VerificationSubject{subject}}}}
			coveragePath := filepath.Join(dir, "coverage.json")
			if err := os.WriteFile(coveragePath, core.JSONPayload(coverage), 0600); err != nil {
				t.Fatal(err)
			}
			options := kitVerifyOptions{configPath: cfgPath, coveragePath: coveragePath, attemptRoot: filepath.Join(dir, "attempts")}
			if err := verifyKits(t.Context(), f.v.rpc, f.v.root, "task", options, &f.output); err != nil {
				t.Fatal(err)
			}
			if f.starts != 1 || f.uploads != 1 || f.outcome != "succeeded" {
				t.Fatalf("ordinary execution missing: starts %d uploads %d state %s", f.starts, f.uploads, f.outcome)
			}
			if err := verifyKits(t.Context(), f.v.rpc, f.v.root, "task", options, &f.output); err == nil {
				t.Fatal("replayed start launched again")
			}
			if f.starts != 1 || f.uploads != 1 {
				t.Fatal("replay mutated execution")
			}
		})
	}
}

func TestKitRunnerUIExecutionReport(t *testing.T) {
	e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", "sleep 0.2"}, TimeoutSeconds: 5, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
	f := newKitExecutionFixture(t, e)
	f.v.ui = &verification.UI{Argv: []string{"sh", "-c", "sleep 30 & wait"}, Port: 8765}
	f.v.uiRoot = f.v.root
	environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
	if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, environment, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	if len(f.snapshot.Evidence) != 2 {
		t.Fatal("missing child or UI execution report")
	}
	for _, item := range f.snapshot.Evidence {
		if item.Envelope.Type != "execution_report" {
			t.Fatal("wrong report type")
		}
	}
}

func TestKitRunnerOfflineEvidenceRetentionAndRetry(t *testing.T) {
	e := verification.Exercise{ID: "offline", Kind: "script", Argv: []string{"sh", "-c", "sleep 5"}, TimeoutSeconds: 10, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
	f := newKitExecutionFixture(t, e)
	root := t.TempDir()
	time.AfterFunc(200*time.Millisecond, func() { f.mu.Lock(); f.offline = true; f.mu.Unlock() })
	environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
	err := f.v.launch(t.Context(), e, f.v.root, root, "run", "grant", f.snapshot.Attempts[0].Subject, environment, []string{"PATH=/usr/bin:/bin"})
	if err == nil || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("offline output not retained: %v", err)
	}
	path := filepath.Join(root, "run", "spool", "execution.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.offline = false
	f.mu.Unlock()
	if err := f.v.resumeSpool(t.Context(), root, f.snapshot.Attempts[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("acknowledged spool not removed")
	}
	if f.uploads != 1 || f.starts != 0 {
		t.Fatal("spool retry relaunched execution")
	}
}

func TestKitRunnerRevocationAndExpiryDuringRequest(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			e := verification.Exercise{ID: "stop", Kind: "script", Argv: []string{"sh", "-c", "sleep 20 & wait"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
			f := newKitExecutionFixture(t, e)
			if revoke {
				time.AfterFunc(200*time.Millisecond, func() {
					f.mu.Lock()
					f.snapshot.PermissionRevocations = append(f.snapshot.PermissionRevocations, store.VerificationPermissionRevocation{GrantID: "grant"})
					f.mu.Unlock()
				})
			} else {
				f.order.LeaseExpiresAt = time.Now().Add(1200 * time.Millisecond)
				f.v.order = f.order
				f.renewDelayAfter = 1
			}
			environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
			started := time.Now()
			err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", f.snapshot.Attempts[0].Subject, environment, []string{"PATH=/usr/bin:/bin"})
			if err == nil || time.Since(started) > 4*time.Second {
				t.Fatalf("teardown %v after %s", err, time.Since(started))
			}
			if f.uploads != 0 || f.outcome != "" {
				t.Fatal("worker wrote after revoked or expired authority")
			}
		})
	}
}

func TestKitRunnerRejectsParentCredentialAlias(t *testing.T) {
	t.Setenv("GH_TOKEN", "forge-alias-fixture")
	t.Setenv("CONVEYOR_KIT_SECRET_API", "forge-alias-fixture")
	e := verification.Exercise{Argv: []string{"true"}, Prerequisites: []verification.Prerequisite{{ID: "api", Kind: "credential", EnvironmentBinding: "api"}}}
	local := []verification.VerificationPermission{{Kind: "credential", Binding: "api", Target: "CONVEYOR_KIT_SECRET_API"}}
	if _, _, _, err := kitExerciseActions(e, t.TempDir(), "repo", local); err == nil {
		t.Fatal("forge credential alias reached child")
	}
}

func TestKitRunnerRetainsSanitizedOutput(t *testing.T) {
	e := verification.Exercise{ID: "output", Kind: "script", Argv: []string{"sh", "-c", `printf '%s\n' "$KIT_VALUE" 'https://example.test/path?password=unknown-secret' 'Authorization: unknown-header'`}, TimeoutSeconds: 5, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
	f := newKitExecutionFixture(t, e)
	root := t.TempDir()
	env := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
	if err := f.v.launch(t.Context(), e, f.v.root, root, "run", "grant", f.snapshot.Attempts[0].Subject, env, []string{"PATH=/usr/bin:/bin", "KIT_VALUE=credential-secret-fixture"}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(filepath.Join(root, "run", "output.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-secret-fixture", "unknown-secret", "unknown-header"} {
		if bytes.Contains(output, []byte(secret)) {
			t.Fatal("sensitive output retained")
		}
	}
}

func TestKitRunnerDigestAndMissingGrantAdmission(t *testing.T) {
	f := newKitExecutionFixture(t, verification.Exercise{ID: "observe"})
	f.v.snapshot.Contexts[0].GoverningPins = []core.VerificationPin{{Kind: "requirement", DocumentID: "req-sample", Version: 1}}
	subject := store.VerificationSubjectContract{Subject: core.VerificationSubject{Kind: "kit", KitID: "sample", KitVersion: "1", ExerciseID: "observe", ContentDigest: strings.Repeat("b", 64)}, Contract: verification.Exercise{ID: "observe"}}
	if err := f.v.run(t.Context(), subject, t.TempDir()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("changed kit admitted: %v", err)
	}
	subject = store.VerificationSubjectContract{Subject: f.snapshot.Attempts[0].Subject, Contract: verification.Exercise{ID: "observe", Permissions: []verification.Permission{{Kind: "network", TargetBinding: "fixture"}}}}
	if err := f.v.run(t.Context(), subject, t.TempDir()); err == nil || !strings.Contains(err.Error(), "network fixture") {
		t.Fatalf("missing action not named: %v", err)
	}
	if f.starts != 0 || f.uploads != 0 {
		t.Fatal("refused admission launched exercise")
	}
	if _, err := localKitGit(t.Context(), f.v.root, "checkout", "--detach"); err != nil {
		t.Fatal(err)
	}
	if err := f.v.checkCheckout(t.Context()); err == nil {
		t.Fatal("detached checkout admitted")
	}
}

func TestKitRunnerInteractiveCompletionUsesOperatorEvidence(t *testing.T) {
	e := verification.Exercise{ID: "interactive", Kind: "interactive", Argv: []string{"true"}, TimeoutSeconds: 5, RequiredAssertions: []string{}, Operations: []verification.Operation{}}
	f := newKitExecutionFixture(t, e)
	subject := f.snapshot.Attempts[0].Subject
	at := time.Now().UTC().Format(time.RFC3339Nano)
	observation := core.VerificationEvidence{SchemaVersion: 1, ID: "operator-observation", SubmissionKey: "operator", Type: "operator_observation", CapturedAt: at, ReceivedAt: at, SubmittedBy: "user:operator", CapturedBy: core.VerificationCaptureActor{Identity: "user:operator", Kind: "operator", Version: "1", Attribution: "authenticated_operator"}, WorkspaceID: "demo", TaskID: "task", WorkOrderID: "order", WorkOrderAttemptID: "claim", ContextID: "context", RunID: "run", Subject: subject, Revisions: f.snapshot.Contexts[0].Revisions, GoverningPins: []core.VerificationPin{}, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown"}, Payload: core.JSONPayload(core.OperatorObservationPayload{OperatorID: "user:operator", Fact: "Completed the fixture interaction", CapturedAt: at, Supporting: []core.VerificationReference{{EvidenceID: "run-execution"}}})}
	if err := observation.Validate(core.VerificationEvidenceAuthority{SubmittedBy: observation.SubmittedBy, OperateGates: true}); err != nil {
		t.Fatal(err)
	}
	f.snapshot.Evidence = append(f.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: observation})
	env := observation.Environment
	env.Attributes = map[string]string{}
	if err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", subject, env, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	if f.outcome != "succeeded" {
		t.Fatal("operator completion was not honored")
	}
}

func TestKitRunnerRejectsParentCredentialInOrdinaryInput(t *testing.T) {
	t.Setenv("GH_TOKEN", "forge-input-fixture")
	t.Setenv("CONVEYOR_KIT_SECRET_API", "kit-input-fixture")
	e := verification.Exercise{Inputs: []verification.Input{{Name: "message", Type: "string"}}}
	for _, value := range []string{"forge-input-fixture", "Bearer forge-input-fixture", "kit-input-fixture", "Bearer kit-input-fixture"} {
		raw, _ := json.Marshal(value)
		if _, _, _, _, err := kitInputValues(e, map[string]json.RawMessage{"message": raw}, nil); err == nil {
			t.Fatal("parent credential reached ordinary inputs")
		}
	}
	if _, _, _, _, err := kitInputValues(e, map[string]json.RawMessage{"message": json.RawMessage(`"ordinary message"`)}, nil); err != nil {
		t.Fatal(err)
	}
}
