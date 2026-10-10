package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type verificationCLIFixture struct {
	b                       store.Backend
	ctx                     context.Context
	orderID, contextID      string
	operatorPAT, viewerPAT  string
	subject, networkSubject core.VerificationSubject
}

// newVerificationCLIFixture serves the real HTTP handler over local HTTP with
// a claimed verify order, a prepared context and two registered obligations.
func newVerificationCLIFixture(t *testing.T) verificationCLIFixture {
	t.Helper()
	b := store.NewVolatileBackend()
	t.Cleanup(b.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://github.com/org/repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := b.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "verification-cli", Workspace: "demo", Repo: "repo", Title: "verification", Branch: "conveyor/cli", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	if err := b.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, CreatedAt: task.CreatedAt}
	if _, err := storetest.CreateStageWorkOrder(ctx, b, job, order); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimWorkOrder(ctx, b, order.ID, core.WorkOrderClaim{Requirements: []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe"}}}}, WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session", ClientToken: "token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: b, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	worker := store.WithActor(ctx, store.Actor{ID: "worker:fixture", Role: core.ActorWorker})
	call := func(name string, input any) any {
		t.Helper()
		out, err := service.Verification(worker, order.ID, "session", "token", name, core.JSONPayload(input))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	f := verificationCLIFixture{b: b, ctx: ctx, orderID: order.ID}
	f.contextID = call("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "cli"}).(store.VerificationSnapshot).Contexts[0].ID
	register := func(id string, permissions []verification.Permission) core.VerificationSubject {
		receipt := call("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: f.contextID, ObligationID: id, Description: "fixture " + id, Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}}, Contract: verification.Exercise{ID: id, Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 1, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read only", Permissions: permissions}}).(store.VerificationReceipt)
		return core.VerificationSubject{Kind: "ordinary", ObligationID: id, ContractDigest: receipt.Digest}
	}
	f.subject = register("ordinary", nil)
	f.networkSubject = register("network", []verification.Permission{{Kind: "network", TargetBinding: "api"}})
	if _, err := b.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}, "cli-operator-token"); err != nil {
		t.Fatal(err)
	}
	f.operatorPAT = "cli-operator-token"
	owner, err := b.VerifyPersonalAccessToken(ctx, f.operatorPAT)
	if err != nil {
		t.Fatal(err)
	}
	operator := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser, OwnerUserID: owner.ID}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	viewer, err := b.ProvisionIdentityUser(operator, "viewer@example.test", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.GrantWorkspaceRole(operator, viewer.Email, "demo", core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	issued, err := b.IssueOwnPersonalAccessToken(ctx, viewer.ID, "viewer")
	if err != nil {
		t.Fatal(err)
	}
	f.viewerPAT = issued.Value
	s := httpapi.NewServer(b)
	s.Workspace = "demo"
	s.Workspaces = b
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Setenv("CONVEYOR_ADDR", srv.URL)
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldWorkspace, oldServer := workspaceFlag, serverFlag
	workspaceFlag, serverFlag = "", ""
	t.Cleanup(func() { workspaceFlag, serverFlag = oldWorkspace, oldServer })
	return f
}

func runVerificationCLI(t *testing.T, token, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CONVEYOR_API_TOKEN", token)
	cmd := verificationCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"permissions"}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

// component-verification-runner: one grant issued and read back through the real handler.
func TestVerificationPermissionsCLIGrantReadBackAndRevoke(t *testing.T) {
	f := newVerificationCLIFixture(t)
	out, err := runVerificationCLI(t, f.operatorPAT, "", "inspect", f.orderID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Eligibility  eligible", "ordinary:network", f.networkSubject.ContractDigest, "action       network binding api (required; target unresolved", "Context      " + f.contextID} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, f.operatorPAT) || strings.Contains(out, "token") {
		t.Fatalf("inspect exposed a credential:\n%s", out)
	}
	if out, err = runVerificationCLI(t, f.operatorPAT, "no\n", "grant", f.orderID, "--subject", "ordinary:network", "--action", "network:api=https://api.example.test", "--request-key", "cli-grant"); err == nil || !strings.Contains(out, "\"request_key\": \"cli-grant\"") {
		t.Fatalf("declined confirmation sent a grant: %v\n%s", err, out)
	}
	if view, _ := newClient().verificationPermissionView(f.orderID, ""); len(view.Grants) != 0 {
		t.Fatal("declined confirmation created a grant")
	}
	grantArgs := []string{"grant", f.orderID, "--subject", "ordinary:network", "--action", "network:api=https://api.example.test", "--request-key", "cli-grant", "--json"}
	out, err = runVerificationCLI(t, f.operatorPAT, "yes\n", grantArgs...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var receipt store.VerificationPermissionGrantView
	// --json keeps stdout to the receipt; the preview and prompt use stderr.
	if err = json.Unmarshal([]byte(out[:strings.Index(out, "\n")]), &receipt); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if receipt.ID == "" || receipt.Subject != f.networkSubject || receipt.ContextID != f.contextID || receipt.WorkOrderID != f.orderID || receipt.WorkOrderAttemptID == "" || len(receipt.Actions) != 1 || receipt.Actions[0].Target != "https://api.example.test:443" || !strings.HasPrefix(receipt.Actor, "user:") || len(receipt.Revisions) != 1 {
		t.Fatalf("receipt: %+v", receipt)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", append(grantArgs, "--yes")...)
	if err != nil || !strings.Contains(out, receipt.ID) {
		t.Fatalf("unchanged retry: %v\n%s", err, out)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:network", "--action", "network:api=https://other.example.test", "--request-key", "cli-grant", "--yes")
	var refusal *verificationPermissionRefusal
	if !errors.As(err, &refusal) || refusal.Reason != store.VerificationRefusalRequestConflict || !strings.Contains(err.Error(), "recovery:") {
		t.Fatalf("changed request: %v\n%s", err, out)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:ordinary", "--no-actions", "--request-key", "empty", "--yes")
	if err != nil || !strings.Contains(out, "actions      none") {
		t.Fatalf("explicit empty grant: %v\n%s", err, out)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", "revoke", f.orderID, "--grant-id", receipt.ID, "--reason", "fixture revocation", "--request-key", "cli-revoke", "--yes")
	if err != nil || !strings.Contains(out, "revoked      by user:") || !strings.Contains(out, "fixture revocation") {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", append(grantArgs, "--yes")...)
	if !errors.As(err, &refusal) || refusal.Reason != store.VerificationRefusalGrantRevoked {
		t.Fatalf("revoked key replay: %v\n%s", err, out)
	}
}

func TestVerificationPermissionsCLIRefusals(t *testing.T) {
	f := newVerificationCLIFixture(t)
	if _, err := runVerificationCLI(t, f.viewerPAT, "", "inspect", f.orderID); err == nil {
		t.Fatal("viewer inspected grant projection")
	}
	out, err := runVerificationCLI(t, f.viewerPAT, "", "grant", f.orderID, "--subject", "ordinary:ordinary", "--no-actions", "--request-key", "viewer", "--yes")
	var refusal *verificationPermissionRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "" || strings.Contains(out, f.contextID) {
		t.Fatalf("viewer grant: %v\n%s", err, out)
	}
	if _, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:absent", "--no-actions", "--request-key", "absent", "--yes"); err == nil || !strings.Contains(err.Error(), "not in context") {
		t.Fatalf("absent subject: %v", err)
	}
	if _, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:ordinary", "--request-key", "none", "--yes"); err == nil {
		t.Fatal("grant without an explicit action list")
	}
	t.Setenv("CONVEYOR_WORKSPACE", "foreign")
	if _, err = runVerificationCLI(t, f.operatorPAT, "", "inspect", f.orderID); err == nil {
		t.Fatal("foreign workspace inspected")
	}
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	order, err := f.b.GetWorkOrder(f.ctx, f.orderID)
	if err != nil {
		t.Fatal(err)
	}
	order.HeadSHA = strings.Repeat("b", 40)
	if err = storetest.UpdateWorkOrder(f.ctx, f.b, order, core.WorkOrderCmdRenew); err != nil {
		t.Fatal(err)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:ordinary", "--no-actions", "--request-key", "stale", "--yes")
	if !errors.As(err, &refusal) || refusal.Reason != store.VerificationRefusalHeadChanged || refusal.Recovery == "" {
		t.Fatalf("changed head: %v\n%s", err, out)
	}
	order.HeadSHA = strings.Repeat("a", 40)
	if err = storetest.UpdateWorkOrder(f.ctx, f.b, order, core.WorkOrderCmdRenew); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ReleaseWorkerClaim(f.ctx, f.b, f.orderID, "fixture", core.WorkOrderRelease{SessionID: "session", Reason: "fixture handoff"}); err != nil {
		t.Fatal(err)
	}
	out, err = runVerificationCLI(t, f.operatorPAT, "", "inspect", f.orderID)
	if err != nil || !strings.Contains(out, "Eligibility  not_claimed") || !strings.Contains(out, "Recovery:") {
		t.Fatalf("released inspect: %v\n%s", err, out)
	}
	if _, err = runVerificationCLI(t, f.operatorPAT, "", "grant", f.orderID, "--subject", "ordinary:ordinary", "--no-actions", "--request-key", "late", "--yes"); !errors.As(err, &refusal) || refusal.Reason != store.VerificationRefusalNotClaimed {
		t.Fatalf("released grant: %v", err)
	}
}

func TestParseVerificationAction(t *testing.T) {
	for raw, want := range map[string]core.VerificationPermission{
		"network:api=https://api.example.test": {Kind: "network", Binding: "api", Target: "https://api.example.test"},
		"filesystem_write:repo=/srv/out":       {Kind: "filesystem_write", Binding: "repo", Target: "/srv/out"},
		"credential:token=CONVEYOR_KIT_SECRET": {Kind: "credential", Binding: "token", Target: "CONVEYOR_KIT_SECRET"},
		"operator_interaction:repo":            {Kind: "operator_interaction", Binding: "repo"},
	} {
		if got, err := parseVerificationAction(raw); err != nil || got != want {
			t.Fatalf("%s: %+v %v", raw, got, err)
		}
	}
	for _, raw := range []string{"network", "network:api", "operator_interaction:repo=x", ":a=b"} {
		if _, err := parseVerificationAction(raw); err == nil {
			t.Fatalf("%s accepted", raw)
		}
	}
}
