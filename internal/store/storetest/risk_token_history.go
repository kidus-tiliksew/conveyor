package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRiskTokenHistoryUpgrade proves the operator's decision of review round 1
// on task 261114-9d3b4c for claims that ended before the claim ledger
// migration, on a durable backend (req-risk-based-review AC-12.1;
// feature-risk-based-review RR-11):
//
//   - a claim released before the upgrade, and the first attempt of a claim
//     retried before the upgrade, keep their session ID refusal;
//   - a fresh session that reuses only such a claim's token is admitted, and
//     the risk order's activity records that the task's token history is
//     incomplete, with no hash;
//   - the retried claim's current attempt, whose fingerprint the migration
//     backfill copies, keeps its token refusal;
//   - a claim released after the upgrade keeps its token refusal, and a task
//     whose history is complete records no indication.
//
// upgrade returns the backend to its schema before the claim ledger migration,
// with the ledger absent and that migration unapplied, and then runs the
// backend's real migrator. ctx carries the workspace and an actor.
func RunRiskTokenHistoryUpgrade(t *testing.T, st store.Store, ctx context.Context, workspace string, upgrade func(*testing.T)) {
	t.Helper()
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	fileTask := func(t *testing.T) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &record,
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "1h"}}
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	implementOrder := func(t *testing.T, taskID string) core.WorkOrder {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
		now := time.Now().UTC()
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
		return core.WorkOrder{ID: id, TaskID: taskID}
	}
	claim := func(id, session, token string) (core.WorkOrder, error) {
		return ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "run:upgrade-owner", SessionID: session, ClientToken: token,
			Agent: "claude-code", Model: "model-" + session, Lease: time.Hour, ExecutionTimeout: time.Hour})
	}
	release := func(t *testing.T, order core.WorkOrder, session string) {
		t.Helper()
		_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			return st.ReleaseWorkerClaimCommand(ctx, lease, order.ID, core.WorkOrderClaimIdentity{ClaimantID: "run:upgrade-owner", SessionID: session},
				core.WorkOrderRelease{SessionID: session, Reason: "harness crashed " + session, Outcome: core.WorkOrderOutcomeChildFailure,
					FailureDetail: "harness crashed " + session, InitialRetryDelay: time.Millisecond, MaximumRetryDelay: time.Millisecond})
		})
		requireOK(t, err)
		time.Sleep(20 * time.Millisecond)
	}
	riskOrder := func(t *testing.T, taskID string) core.WorkOrder {
		t.Helper()
		type created struct {
			order core.WorkOrder
			made  bool
		}
		out, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (created, error) {
			order, made, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: taskID,
				Subject: core.RiskOrderSubject{HeadSHA: "head-" + taskID, BaseSHA: "base-" + taskID}, ChangedPaths: []string{"a.go"}})
			return created{order, made}, err
		})
		requireOK(t, err)
		if !out.made {
			t.Fatalf("risk order for %s was not created", taskID)
		}
		return out.order
	}
	refused := func(t *testing.T, id, session, token string) {
		t.Helper()
		if _, err := claim(id, session, token); !errors.Is(err, store.ErrRiskOrderNotIndependent) {
			t.Fatalf("risk claim by session %q token %q err=%v, want refusal", session, token, err)
		}
	}
	indications := func(t *testing.T, taskID string) []core.Event {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		var found []core.Event
		for _, event := range events {
			if event.Kind == store.RiskTokenHistoryIncompleteEvent {
				found = append(found, event)
			}
		}
		return found
	}
	noHash := func(t *testing.T, taskID string, tokens ...string) {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		for _, event := range events {
			payload := string(event.Payload)
			if strings.Contains(payload, "client_token") {
				t.Fatalf("event %s names a client token field: %s", event.Kind, payload)
			}
			for _, token := range tokens {
				if strings.Contains(payload, token) || strings.Contains(payload, fmt.Sprintf("%x", sha256.Sum256([]byte(token)))) {
					t.Fatalf("event %s carries a token or its hash: %s", event.Kind, payload)
				}
			}
		}
	}

	// Before the upgrade: one claim released, and one claim retried, so its
	// first attempt's token survives nowhere and its second attempt still
	// holds the order row.
	released := fileTask(t)
	releasedOrder := implementOrder(t, released.ID)
	_, err := claim(releasedOrder.ID, "before-released", "before-released-token")
	requireOK(t, err)
	release(t, releasedOrder, "before-released")
	retried := fileTask(t)
	retriedOrder := implementOrder(t, retried.ID)
	_, err = claim(retriedOrder.ID, "before-first", "before-first-token")
	requireOK(t, err)
	release(t, retriedOrder, "before-first")
	retriedAttempt, err := claim(retriedOrder.ID, "before-second", "before-second-token")
	requireOK(t, err)

	upgrade(t)

	// After the upgrade: one claim released under the ledger.
	after := fileTask(t)
	afterOrder := implementOrder(t, after.ID)
	_, err = claim(afterOrder.ID, "after-released", "after-released-token")
	requireOK(t, err)
	release(t, afterOrder, "after-released")

	t.Run("ReleasedBeforeUpgrade", func(t *testing.T) {
		risk := riskOrder(t, released.ID)
		refused(t, risk.ID, "before-released", "fresh-released-1")
		claimed, err := claim(risk.ID, "fresh-released-2", "before-released-token")
		if err != nil {
			t.Fatalf("a fresh session reusing only a pre-upgrade token was refused: %v", err)
		}
		found := indications(t, released.ID)
		if len(found) != 1 || found[0].JobID != risk.JobID {
			t.Fatalf("incomplete-history indications=%+v, want one on the risk order", found)
		}
		var payload struct {
			WorkOrderID        string                      `json:"work_order_id"`
			AttemptID          string                      `json:"attempt_id"`
			ClaimsWithoutToken int                         `json:"claims_without_token"`
			Claims             []store.RiskTokenHistoryGap `json:"claims"`
			ClaimRefused       bool                        `json:"claim_refused"`
		}
		requireOK(t, json.Unmarshal(found[0].Payload, &payload))
		if payload.WorkOrderID != risk.ID || payload.AttemptID != claimed.AttemptID || payload.ClaimsWithoutToken != 1 || payload.ClaimRefused ||
			len(payload.Claims) != 1 || payload.Claims[0].WorkOrderID != releasedOrder.ID || payload.Claims[0].Stage != core.StageImplement {
			t.Fatalf("indication payload=%+v", payload)
		}
		noHash(t, released.ID, "before-released-token", "fresh-released-1")
	})

	t.Run("RetriedBeforeUpgrade", func(t *testing.T) {
		risk := riskOrder(t, retried.ID)
		refused(t, risk.ID, "before-first", "fresh-retried-1")
		refused(t, risk.ID, "before-second", "fresh-retried-2")
		// The backfill copied the current attempt's fingerprint.
		refused(t, risk.ID, "fresh-retried-3", "before-second-token")
		if _, err := claim(risk.ID, "fresh-retried-4", "before-first-token"); err != nil {
			t.Fatalf("a fresh session reusing only the first attempt's token was refused: %v", err)
		}
		found := indications(t, retried.ID)
		if len(found) != 1 {
			t.Fatalf("incomplete-history indications=%d, want 1", len(found))
		}
		var payload struct {
			Claims []store.RiskTokenHistoryGap `json:"claims"`
		}
		requireOK(t, json.Unmarshal(found[0].Payload, &payload))
		if len(payload.Claims) != 1 || payload.Claims[0].WorkOrderID != retriedOrder.ID || payload.Claims[0].AttemptID == retriedAttempt.AttemptID {
			t.Fatalf("indication claims=%+v, want only the first attempt of %s", payload.Claims, retriedOrder.ID)
		}
		noHash(t, retried.ID, "before-first-token", "before-second-token")
	})

	t.Run("ReleasedAfterUpgrade", func(t *testing.T) {
		risk := riskOrder(t, after.ID)
		refused(t, risk.ID, "fresh-after-1", "after-released-token")
		refused(t, risk.ID, "after-released", "fresh-after-2")
		if _, err := claim(risk.ID, "fresh-after-3", "fresh-after-3-token"); err != nil {
			t.Fatalf("an independent post-upgrade claim was refused: %v", err)
		}
		if found := indications(t, after.ID); len(found) != 0 {
			t.Fatalf("a task with complete token history recorded %d indications", len(found))
		}
		noHash(t, after.ID, "after-released-token")
	})
}
