package workorder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ChangeTaskSetup resolves a current named workspace setup and builds the
// future-only transition submitted atomically to persistence (spec §21.35).
func (s *Service) ChangeTaskSetup(ctx context.Context, taskID, setupName, reason, requestID string) (store.SetupChangeResult, error) {
	setupName, reason, requestID = strings.TrimSpace(setupName), strings.TrimSpace(reason), strings.TrimSpace(requestID)
	if setupName == "" || reason == "" || requestID == "" {
		return store.SetupChangeResult{}, fmt.Errorf("setup, non-empty reason, and request_id are required")
	}
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	selected, ok := cfg.Setup(setupName)
	if !ok {
		return store.SetupChangeResult{}, fmt.Errorf("setup %q is not defined in this workspace", setupName)
	}
	request := store.SetupChangeRequest{TaskID: taskID, RequestID: requestID, Reason: reason, Setup: selected}
	updatedTask := task
	updatedTask.SetupName, updatedTask.SetupContract = selected.Name, selected
	orders, err := s.Store.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	for _, order := range orders {
		if order.State != core.WorkOrderQueued || (order.Stage != core.StageSpec && order.Stage != core.StageImplement) {
			continue
		}
		desired, buildErr := dispatch.BuildFutureWorkOrderRouting(cfg, updatedTask, order.Stage)
		if buildErr != nil {
			return store.SetupChangeResult{}, buildErr
		}
		desired.ID, desired.TaskID, desired.JobID = order.ID, order.TaskID, order.JobID
		request.WorkOrderUpdates = append(request.WorkOrderUpdates, desired)
	}
	if err = planReviewSetupChange(cfg, updatedTask, orders, events, &request); err != nil {
		return store.SetupChangeResult{}, err
	}
	return s.Store.ChangeTaskSetup(ctx, request)
}

func planReviewSetupChange(cfg *config.Config, task core.Task, orders []core.WorkOrder, events []core.Event, request *store.SetupChangeRequest) error {
	latest := latestReviewRound(orders)
	if latest == 0 {
		request.ReviewTransition = "none"
		return nil
	}
	superseded := store.SupersededReviewWorkOrders(events)
	var current []core.WorkOrder
	hasQueued := false
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == latest && !superseded[order.ID] {
			current = append(current, order)
			hasQueued = hasQueued || order.State == core.WorkOrderQueued
		}
	}
	if !hasQueued {
		request.ReviewTransition = "none"
		return nil
	}
	sort.Slice(current, func(i, j int) bool { return current[i].ReviewSeat < current[j].ReviewSeat })
	route := cfg.WithSetup(task.SetupContract).Routing.Stages[string(core.StageReview)]
	_, desired, err := dispatch.BuildReviewRound(cfg, task, route, latest)
	if err != nil {
		return err
	}
	request.PriorReviewRound, request.ResultingReviewRound = latest, latest
	forceNewRound := len(current) != len(desired)
	if !forceNewRound {
		for i := range current {
			if current[i].ReviewSeat != i+1 || (current[i].State != core.WorkOrderQueued && current[i].State != core.WorkOrderCompleted) {
				forceNewRound = true
				break
			}
		}
	}
	if forceNewRound {
		request.ReviewTransition = "new_full_round"
		request.ResultingReviewRound = latest + 1
		for _, order := range current {
			request.SupersedeWorkOrderIDs = append(request.SupersedeWorkOrderIDs, order.ID)
		}
		jobs, next, buildErr := dispatch.BuildReviewRound(cfg, task, route, latest+1)
		if buildErr != nil {
			return buildErr
		}
		request.NewJobs, request.NewWorkOrders = jobs, next
		return nil
	}
	request.ReviewTransition = "same_round_reconciled"
	suffix := fmt.Sprintf("setup-%x", sha256.Sum256([]byte(request.RequestID)))[:14]
	for i, old := range current {
		fresh := desired[i]
		if old.State == core.WorkOrderCompleted {
			if sameReviewAssignment(old, fresh) {
				request.RetainedWorkOrderIDs = append(request.RetainedWorkOrderIDs, old.ID)
				continue
			}
			request.SupersedeWorkOrderIDs = append(request.SupersedeWorkOrderIDs, old.ID)
			job := core.Job{ID: fresh.JobID + "-" + suffix, TaskID: task.ID, Stage: core.StageReview, Harness: "external-mcp", ModelTier: fresh.RequiredModel, AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending}
			fresh.ID, fresh.JobID = job.ID, job.ID
			request.NewJobs, request.NewWorkOrders = append(request.NewJobs, job), append(request.NewWorkOrders, fresh)
			continue
		}
		fresh.ID, fresh.TaskID, fresh.JobID = old.ID, old.TaskID, old.JobID
		request.WorkOrderUpdates = append(request.WorkOrderUpdates, fresh)
	}
	return nil
}

func sameReviewAssignment(left, right core.WorkOrder) bool {
	return left.RequiredHarness == right.RequiredHarness && left.RequiredModel == right.RequiredModel &&
		left.RequiredEffort == right.RequiredEffort
}
