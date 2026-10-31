package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ClaimServiceFunc returns workorder.Service.Claim over st with cfg.
// storetest cannot import workorder: workorder tests import storetest.
type ClaimServiceFunc func(st store.Store, cfg *config.Config) func(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)

// claimFailingStore fails the claim transaction with the backend sentinel
// while failures remain, as SingleStore's refused long-data write did
// (task 261030-fb4d3c). Every other method reaches the real backend.
type claimFailingStore struct {
	store.Store
	failures *int
}

func (s claimFailingStore) ClaimWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	if *s.failures > 0 {
		*s.failures--
		return core.WorkOrder{}, fmt.Errorf("SingleStore database operation failed: %w", store.ErrBackendOperation)
	}
	return s.Store.ClaimWorkOrderCommand(ctx, lease, id, claim)
}

// runClaimFailureStall fails an order's claim in the backend through the real
// work-order service until the stall threshold, reads the next action and the
// inbox through the real handler, and proves a successful claim clears both
// (component-work-orders, Claim admission and Next action;
// feature-operator-next-action NA-3, NA-5; req-operator-next-action AC-1.8,
// AC-2.5).
func runClaimFailureStall(t *testing.T, x Fixture, claimService ClaimServiceFunc) {
	if claimService == nil {
		t.Fatal("conformance factory must provide ClaimService for the claim-failure stall")
	}
	if x.LineageHTTP == nil {
		t.Fatal("conformance fixture must provide LineageHTTP for the claim-failure stall")
	}
	st, ctx := x.Backend, x.Context
	handler := x.LineageHTTP(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Claim failure " + id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, task))
	job := core.Job{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, ExecutionTimeoutText: "1h", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	_, err := CreateStageWorkOrder(ctx, st, job, order)
	requireOK(t, err)

	failures := nextaction.ClaimFailureStallThreshold
	failing := claimFailingStore{Store: st, failures: &failures}
	claim := claimService(failing, x.Config)
	attempt := func(n int) (core.WorkOrder, error) {
		return claim(ctx, order.ID, core.WorkOrderClaim{SessionID: fmt.Sprintf("claim-failure-%s-%d", id, n), ClientToken: fmt.Sprintf("claim-failure-token-%s-%d", id, n), ClaimantID: "run:storetest", Agent: "codex", Model: "model", Lease: time.Minute})
	}

	for n := 1; n <= nextaction.ClaimFailureStallThreshold; n++ {
		if _, err := attempt(n); err == nil || !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("claim %d error = %v, want the backend sentinel", n, err)
		}
		records, err := st.ListWorkOrderClaimFailures(ctx, []string{order.ID})
		requireOK(t, err)
		if len(records) != 1 || records[0].WorkOrderID != order.ID || records[0].ConsecutiveFailures != n || records[0].ErrorClass != core.WorkOrderClaimFailureClassBackendOperation {
			t.Fatalf("after claim %d records = %+v, want one backend_operation record counting %d", n, records, n)
		}
		if records[0].LastFailedAt.Before(records[0].FirstFailedAt) {
			t.Fatalf("record times = %+v", records[0])
		}
		action := claimFailureNextAction(t, handler, x.Workspace, id)
		if n < nextaction.ClaimFailureStallThreshold && action.Kind != nextaction.KindQueued {
			t.Fatalf("below the threshold next action = %s, want queued", action.Kind)
		}
	}
	stored, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if stored.State != core.WorkOrderQueued {
		t.Fatalf("order state after failed claims = %s, want queued", stored.State)
	}

	action := claimFailureNextAction(t, handler, x.Workspace, id)
	if action.Kind != nextaction.KindStalled || action.Actor != nextaction.ActorOperator || action.Judgment || action.Details.StallReason != nextaction.StallClaimFailing {
		t.Fatalf("next action = %s/%s judgment=%v stall=%q, want the claim_failing stall", action.Actor, action.Kind, action.Judgment, action.Details.StallReason)
	}
	if action.Label != "Claims failing" || action.Details.ClaimFailure == nil || action.Details.ClaimFailure.ConsecutiveFailures != nextaction.ClaimFailureStallThreshold {
		t.Fatalf("label %q claim_failure %+v", action.Label, action.Details.ClaimFailure)
	}
	for _, choice := range action.Choices {
		if choice.ID == "redispatch" {
			t.Fatalf("claim_failing offered redispatch: %+v", action.Choices)
		}
	}
	item, found := claimFailureInboxItem(t, handler, x.Workspace, id)
	if !found || !item.NoJudgmentNeeded || item.NextAction.Label != action.Label || item.NextAction.Summary != action.Summary {
		t.Fatalf("inbox item found=%v %+v, want an automatic no-judgment item labelled %q", found, item, action.Label)
	}

	// A refusal for another reason neither records nor clears.
	if _, err := claim(ctx, order.ID, core.WorkOrderClaim{ClientToken: "no-session", ClaimantID: "run:storetest", Lease: time.Minute}); err == nil {
		t.Fatal("claim without a session succeeded")
	}
	records, err := st.ListWorkOrderClaimFailures(ctx, []string{order.ID})
	requireOK(t, err)
	if len(records) != 1 || records[0].ConsecutiveFailures != nextaction.ClaimFailureStallThreshold {
		t.Fatalf("after a refusal records = %+v, want the unchanged record", records)
	}

	claimed, err := attempt(nextaction.ClaimFailureStallThreshold + 1)
	if err != nil {
		t.Fatalf("claim after the failures: %v", err)
	}
	if claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claimed state = %s", claimed.State)
	}
	records, err = st.ListWorkOrderClaimFailures(ctx, []string{order.ID})
	requireOK(t, err)
	if len(records) != 0 {
		t.Fatalf("after a successful claim records = %+v, want none", records)
	}
	action = claimFailureNextAction(t, handler, x.Workspace, id)
	if action.Kind == nextaction.KindStalled || action.Details.ClaimFailure != nil {
		t.Fatalf("after a successful claim next action = %s stall=%q", action.Kind, action.Details.StallReason)
	}
	if _, found := claimFailureInboxItem(t, handler, x.Workspace, id); found {
		t.Fatal("after a successful claim the inbox still lists the task as automatic")
	}
}

type claimFailureInbox struct {
	Items []struct {
		Band             string              `json:"band"`
		Task             *core.Task          `json:"task"`
		NextAction       *nextaction.Compact `json:"next_action"`
		NoJudgmentNeeded bool                `json:"no_judgment_needed"`
	} `json:"items"`
}

type claimFailureItem struct {
	NoJudgmentNeeded bool
	NextAction       nextaction.Compact
}

func claimFailureGet(t *testing.T, handler http.Handler, workspace, path string, out any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("X-Workspace-ID", workspace)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func claimFailureNextAction(t *testing.T, handler http.Handler, workspace, taskID string) nextaction.NextAction {
	t.Helper()
	var action nextaction.NextAction
	claimFailureGet(t, handler, workspace, "/v1/tasks/"+taskID+"/next-action", &action)
	return action
}

func claimFailureInboxItem(t *testing.T, handler http.Handler, workspace, taskID string) (claimFailureItem, bool) {
	t.Helper()
	var inbox claimFailureInbox
	claimFailureGet(t, handler, workspace, "/v1/next-actions?band=automatic&limit=500", &inbox)
	for _, item := range inbox.Items {
		if item.Task != nil && item.Task.ID == taskID && item.NextAction != nil {
			if item.Band != "automatic" {
				t.Fatalf("inbox band = %q, want automatic", item.Band)
			}
			return claimFailureItem{NoJudgmentNeeded: item.NoJudgmentNeeded, NextAction: *item.NextAction}, true
		}
	}
	return claimFailureItem{}, false
}
