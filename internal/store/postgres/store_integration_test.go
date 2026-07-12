package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	basestore "github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPostgresCreateSpecVersionAlwaysStartsUnapproved(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := time.Now().UTC().Format("150405.000000000")
	cfg := &config.Config{
		Workspace: "spec-parity-" + suffix,
		Repos:     []config.Repo{{Name: "api", URL: "file:///tmp/api", Base: "main"}},
	}
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{
		ID: "spec-parity-" + suffix, Workspace: cfg.Workspace, Repo: "api",
		Branch: "conveyor/spec-parity-" + suffix, State: core.TaskAwaiting,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: task.ID, Content: "# Spec", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}),
		Approved: true, ApprovedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Approved || !created.ApprovedAt.IsZero() {
		t.Fatalf("created spec bypassed approval gate: %+v", created)
	}
}

func TestPostgresStorePersistsEventsAndRejectsMutation(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	ctx := basestore.WithActor(context.Background(), basestore.Actor{ID: "operator-1", Role: core.ActorHuman})
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UTC().Format("150405.000000000")
	cfg := &config.Config{
		Workspace: "integration", Repos: []config.Repo{{Name: "api", URL: "file:///tmp/api", Base: "main"}},
		Credentials: []config.Credential{{
			ID: "cred-" + suffix, OwnerID: "operator-" + suffix, OwnerKind: "user", Kind: "personal_sub",
			Vendor: "openai", Harness: "codex", Ref: "/tmp/codex-auth",
		}},
		VendorPolicies: []config.VendorPolicy{{
			Vendor: "openai", Harness: "codex", AuthMode: "personal_sub",
			SubscriptionHeadless: "allowed", ReviewedAt: "2026-07-10", SourceURL: "https://example.com/terms",
		}},
	}
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		st.Close()
		t.Fatal(err)
	}
	claim := routing.ClaimRequest{TaskID: "route-task-" + suffix, JobID: "route-job-" + suffix, OwnerID: "operator-" + suffix, Harnesses: []string{"codex"}, LeaseSeconds: 60}
	credential, err := st.ClaimCredential(ctx, claim)
	if err != nil || credential.ID != cfg.Credentials[0].ID {
		st.Close()
		t.Fatalf("credential claim = %+v, err=%v", credential, err)
	}
	if _, err := st.ClaimCredential(ctx, routing.ClaimRequest{TaskID: claim.TaskID + "-other", JobID: claim.JobID + "-other", OwnerID: "operator-" + suffix, Harnesses: []string{"codex"}, LeaseSeconds: 60}); !errors.Is(err, routing.ErrNoCapacity) {
		st.Close()
		t.Fatalf("concurrent claim error = %v", err)
	}
	if err := st.ReleaseCredential(ctx, credential.ID, claim.JobID, ""); err != nil {
		st.Close()
		t.Fatal(err)
	}
	orphan := claim
	orphan.TaskID = "orphan-task-" + suffix
	orphan.JobID = "orphan-job-" + suffix
	orphan.LeaseSeconds = 4 * 60 * 60
	if _, err := st.ClaimCredential(ctx, orphan); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if sameAttempt, err := st.ClaimCredential(ctx, orphan); err != nil || sameAttempt.ID != credential.ID {
		st.Close()
		t.Fatalf("same attempt could not recover its lease: credential=%+v err=%v", sameAttempt, err)
	}
	rescuerJobID := "rescuer-job-" + suffix
	if err := st.RescueTaskCredentialLeases(ctx, orphan.TaskID, rescuerJobID); err != nil {
		st.Close()
		t.Fatal(err)
	}
	rescued := orphan
	rescued.JobID = rescuerJobID
	rescuedCredential, err := st.ClaimCredential(ctx, rescued)
	if err != nil || rescuedCredential.ID != credential.ID {
		st.Close()
		t.Fatalf("rescued credential = %+v err=%v", rescuedCredential, err)
	}
	if err := st.ReleaseCredential(ctx, rescuedCredential.ID, rescued.JobID, ""); err != nil {
		st.Close()
		t.Fatal(err)
	}
	task := core.Task{
		ID: "pg-" + suffix, Workspace: cfg.Workspace, Source: "test", Title: "persist",
		Level: core.L2, Repo: "api", BaseBranch: "main", Branch: "conveyor/pg-" + suffix,
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		st.Close()
		t.Fatal(err)
	}
	claiming := task
	claiming.ID += "-claiming"
	claiming.Branch += "-claiming"
	claiming.State = core.TaskClaiming
	if err := st.CreateTask(ctx, claiming); err != nil {
		st.Close()
		t.Fatal(err)
	}
	var claimingQueued int
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		claiming.ID,
	).Scan(&claimingQueued); err != nil || claimingQueued != 0 {
		st.Close()
		t.Fatalf("claiming task River count = %d, err=%v", claimingQueued, err)
	}
	if err := st.UpdateTaskState(ctx, claiming.ID, core.TaskQueued); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		claiming.ID,
	).Scan(&claimingQueued); err != nil || claimingQueued != 1 {
		st.Close()
		t.Fatalf("finalized claim River count = %d, err=%v", claimingQueued, err)
	}
	var queued int
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		task.ID,
	).Scan(&queued); err != nil || queued != 1 {
		st.Close()
		t.Fatalf("transactional River job count = %d, err=%v", queued, err)
	}
	stranded := task
	stranded.ID = "stranded-" + suffix
	stranded.Branch = "conveyor/stranded-" + suffix
	if err := st.CreateTask(ctx, stranded); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, "DELETE FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1", stranded.ID); err != nil {
		st.Close()
		t.Fatal(err)
	}
	repaired, err := st.ReconcileQueuedTasks(ctx)
	if err != nil || repaired != 1 {
		st.Close()
		t.Fatalf("reconciled queued tasks = %d, err=%v", repaired, err)
	}
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		stranded.ID,
	).Scan(&queued); err != nil || queued != 1 {
		st.Close()
		t.Fatalf("repaired River job count = %d, err=%v", queued, err)
	}
	strandedEvents, err := st.ListEvents(ctx, stranded.ID)
	if err != nil || strandedEvents[len(strandedEvents)-1].Kind != "dispatch.reconciled" {
		st.Close()
		t.Fatalf("stranded task events = %+v, err=%v", strandedEvents, err)
	}
	if _, err := st.pool.Exec(ctx,
		"UPDATE river_job SET state = 'completed', finalized_at = now() WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		task.ID,
	); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, core.TaskAwaiting); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, core.TaskQueued); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		task.ID,
	).Scan(&queued); err != nil || queued != 2 {
		st.Close()
		t.Fatalf("redispatch River job count = %d, want 2, err=%v", queued, err)
	}
	rejected := task
	rejected.ID += "-duplicate-branch"
	if err := st.CreateTask(ctx, rejected); err == nil {
		st.Close()
		t.Fatal("duplicate branch task unexpectedly committed")
	}
	if err := st.pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1",
		rejected.ID,
	).Scan(&queued); err != nil || queued != 0 {
		st.Close()
		t.Fatalf("rolled-back River job count = %d, err=%v", queued, err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, core.TaskRunning); err != nil {
		st.Close()
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-job", TaskID: task.ID, Stage: core.StageImplement, Harness: "codex", Runner: "test", Confinement: "tierA", State: core.JobDone, StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC()}
	if err := st.CreateJob(ctx, job); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "s3://transcripts/" + job.ID, RedactionStats: core.RedactionStats{Exact: 1}}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionRedirect, ReasonCode: "spec-wrong", Comment: "retry"}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.SetTaskTransition(ctx, task.ID, core.TaskQueued, core.StageImplement, ""); err != nil {
		st.Close()
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if len(events) != 9 {
		st.Close()
		t.Fatalf("events = %d, want 9", len(events))
	}
	latest, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || latest.ID != job.ID {
		st.Close()
		t.Fatalf("latest job = %+v ok=%t err=%v", latest, ok, err)
	}
	after, err := st.ListEventsAfter(ctx, task.ID, events[len(events)-2].ID)
	if err != nil || len(after) != 1 || after[0].ID != events[len(events)-1].ID {
		st.Close()
		t.Fatalf("incremental events = %+v err=%v", after, err)
	}
	markers, err := st.ListActivityMarkers(ctx)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	foundMarker := false
	for _, marker := range markers {
		if marker.TaskID == task.ID {
			foundMarker = marker.LatestStage == core.StageImplement && !marker.LastEventAt.IsZero()
		}
	}
	if !foundMarker {
		st.Close()
		t.Fatalf("activity marker missing for %s: %+v", task.ID, markers)
	}
	if _, err := st.pool.Exec(ctx, "UPDATE events SET kind = 'tampered' WHERE id = $1", events[0].ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		st.Close()
		t.Fatalf("event mutation error = %v", err)
	}
	st.Close()

	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskQueued {
		t.Fatalf("persisted state = %s", persisted.State)
	}
	var riverTable string
	if err := reopened.pool.QueryRow(ctx, "SELECT to_regclass('river_job')::text").Scan(&riverTable); err != nil || riverTable != "river_job" {
		t.Fatalf("River schema missing: table=%q err=%v", riverTable, err)
	}

	other, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherCfg := &config.Config{Workspace: "other-" + suffix, Repos: []config.Repo{{Name: "api", URL: "file:///tmp/api", Base: "main"}}}
	if err := other.BootstrapConfig(ctx, otherCfg); err != nil {
		t.Fatal(err)
	}
	otherTasks, err := other.ListTasks(ctx)
	if err != nil || len(otherTasks) != 0 {
		t.Fatalf("cross-workspace list leaked %d task(s), err=%v", len(otherTasks), err)
	}
	if _, err := other.GetTask(ctx, task.ID); err == nil {
		t.Fatal("cross-workspace task read succeeded")
	}
	if err := other.UpdateTaskState(ctx, task.ID, core.TaskClosed); err == nil {
		t.Fatal("cross-workspace task mutation succeeded")
	}
}

func TestBootstrapConfigSanitizesAndRevokesRemovedCapacity(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := time.Now().UTC().Format("150405.000000000")
	credential := config.Credential{
		ID: "reconcile-cred-" + suffix, OwnerID: "reconcile-owner-" + suffix,
		OwnerKind: "user", Kind: "personal_sub", Vendor: "vendor-" + suffix,
		Harness: "codex", Ref: "/tmp/reconcile-auth",
	}
	policy := config.VendorPolicy{
		Vendor: credential.Vendor, Harness: credential.Harness, AuthMode: credential.Kind,
		SubscriptionHeadless: "allowed", ReviewedAt: "2026-07-11", SourceURL: "https://example.com/terms",
	}
	cfg := &config.Config{
		Workspace:   "reconcile-a-" + suffix,
		Database:    config.Database{Backend: "postgres", URL: "postgres://user:plaintext-secret@db.example/conveyor"},
		Credentials: []config.Credential{credential}, VendorPolicies: []config.VendorPolicy{policy},
	}
	otherCfg := &config.Config{
		Workspace:   "reconcile-b-" + suffix,
		Credentials: []config.Credential{credential}, VendorPolicies: []config.VendorPolicy{policy},
	}
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.BootstrapConfig(ctx, otherCfg); err != nil {
		t.Fatal(err)
	}
	claim := routing.ClaimRequest{
		TaskID: "reconcile-task-" + suffix, JobID: "reconcile-job-1-" + suffix, OwnerID: credential.OwnerID,
		Harnesses: []string{credential.Harness}, LeaseSeconds: 60,
	}
	claimed, err := st.ClaimCredential(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseCredential(ctx, claimed.ID, claim.JobID, ""); err != nil {
		t.Fatal(err)
	}
	cfg.Credentials = nil
	cfg.VendorPolicies = nil
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	claim.JobID = "reconcile-job-2-" + suffix
	claimed, err = st.ClaimCredential(ctx, claim)
	if err != nil {
		t.Fatalf("shared capacity was revoked while another workspace referenced it: %v", err)
	}
	if err := st.ReleaseCredential(ctx, claimed.ID, claim.JobID, ""); err != nil {
		t.Fatal(err)
	}
	otherCfg.Credentials = nil
	otherCfg.VendorPolicies = nil
	if err := st.BootstrapConfig(ctx, otherCfg); err != nil {
		t.Fatal(err)
	}
	claim.JobID = "reconcile-job-3-" + suffix
	if _, err := st.ClaimCredential(ctx, claim); !errors.Is(err, routing.ErrNoCapacity) {
		t.Fatalf("removed credential remained routable: %v", err)
	}
	var credentialState, policyState, storedConfig string
	if err := st.pool.QueryRow(ctx, "SELECT rate_limit_state FROM credentials WHERE id = $1", credential.ID).Scan(&credentialState); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		"SELECT subscription_headless FROM vendor_policies WHERE vendor = $1 AND harness = $2 AND auth_mode = $3",
		policy.Vendor, policy.Harness, policy.AuthMode,
	).Scan(&policyState); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, "SELECT config_yaml FROM workspaces WHERE id = $1", cfg.Workspace).Scan(&storedConfig); err != nil {
		t.Fatal(err)
	}
	if credentialState != "disabled" || policyState != "unknown" {
		t.Fatalf("revoked capacity state = credential:%s policy:%s", credentialState, policyState)
	}
	if strings.Contains(storedConfig, "plaintext-secret") {
		t.Fatalf("stored config leaked database URL: %s", storedConfig)
	}
	cfg.Credentials = []config.Credential{credential}
	cfg.VendorPolicies = []config.VendorPolicy{policy}
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	claim.JobID = "reconcile-job-4-" + suffix
	claimed, err = st.ClaimCredential(ctx, claim)
	if err != nil || claimed.ID != credential.ID {
		t.Fatalf("re-added capacity was not restored: credential=%+v err=%v", claimed, err)
	}
}
