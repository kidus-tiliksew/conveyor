package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// EvaluateTaskApprovalsBatch evaluates tasks in one workspace using live membership.
func EvaluateTaskApprovalsBatch(ctx context.Context, st Store, tasks []core.Task, trusted map[string][]string, prospective string) (map[string]ApprovalEvaluation, error) {
	return evaluateTaskApprovalsBatch(ctx, st, tasks, nil, false, trusted, prospective)
}

func evaluateTaskApprovalsBatch(ctx context.Context, st Store, tasks []core.Task, orders []core.WorkOrder, ordersLoaded bool, trusted map[string][]string, prospective string) (map[string]ApprovalEvaluation, error) {
	out := make(map[string]ApprovalEvaluation, len(tasks))
	var ids []string
	var workspace string
	for _, task := range tasks {
		if !task.SetupContract.Risk.Enforcing() {
			out[task.ID] = ApprovalEvaluation{}
			continue
		}
		if len(ids) == 0 {
			workspace = task.Workspace
		} else if task.Workspace != workspace {
			return nil, fmt.Errorf("approval batch spans workspaces")
		}
		ids = append(ids, task.ID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	if workspace != "" {
		ctx = WithWorkspace(ctx, workspace)
	}
	ratings, err := st.ListRiskRatingsForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	events, err := st.ListClaimEventsForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	if !ordersLoaded {
		orders, err = st.ListWorkOrdersForTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	records, err := st.ListTaskApprovalsForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	ordersByTask := make(map[string][]core.WorkOrder, len(ids))
	for _, order := range orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	people := map[string]bool{}
	for _, task := range tasks {
		if !task.SetupContract.Risk.Enforcing() {
			continue
		}
		taskOrders := ordersByTask[task.ID]
		requirements, tier, _ := EnforcedRiskRequirements(task, ratings[task.ID])
		authors, _ := TaskAuthors(events[task.ID], taskOrders)
		e := ApprovalEvaluation{Enforced: true, Head: strings.TrimSpace(task.ReviewedHeadSHA), ReviewRound: LatestReviewRound(taskOrders), Tier: tier, Requirements: requirements, Authors: authors}
		for _, record := range records[task.ID] {
			if e.Head == "" || record.HeadSHA != e.Head || record.ReviewRound != e.ReviewRound {
				continue
			}
			switch record.Kind {
			case core.TaskApprovalPerson:
				e.Approvals = append(e.Approvals, record)
			case core.TaskApprovalAuthorWaiver:
				e.Waivers = append(e.Waivers, record)
			}
		}
		for _, user := range authors {
			if user != "" {
				people[user] = true
			}
		}
		for _, record := range e.Approvals {
			if record.UserID != "" {
				people[record.UserID] = true
			}
		}
		for _, record := range e.Waivers {
			if record.UserID != "" {
				people[record.UserID] = true
			}
		}
		out[task.ID] = e
	}
	if prospective != "" {
		people[prospective] = true
	}
	var userIDs []string
	for userID := range people {
		userIDs = append(userIDs, userID)
	}
	slices.Sort(userIDs)
	gatekeepers := map[string]bool{}
	if membership, ok := st.(CapabilityAuthorizer); ok && len(userIDs) > 0 {
		gatekeepers, err = membership.AuthorizeWorkspaceUsers(ctx, userIDs, workspace, core.CapabilityOperateGates)
		if err != nil {
			return nil, err
		}
	}
	for _, task := range tasks {
		if !task.SetupContract.Risk.Enforcing() {
			continue
		}
		e := out[task.ID]
		_, unknown := TaskAuthors(events[task.ID], ordersByTask[task.ID])
		var approvers []string
		for _, record := range e.Approvals {
			approvers = append(approvers, record.UserID)
		}
		waived := false
		for _, waiver := range e.Waivers {
			if !slices.Contains(e.Authors, waiver.UserID) && gatekeepers[waiver.UserID] {
				waived = true
				break
			}
		}
		input := riskpolicy.ApprovalInput{Required: e.Requirements.HumanApprovals, Authors: e.Authors, UnknownAuthor: unknown, TrustedAuthors: trusted[task.ID], Gatekeepers: gatekeepers, Approvals: approvers, Waived: waived, Prospective: prospective}
		e.Status = riskpolicy.EvaluateApprovals(input)
		e.Base = e.Status
		if prospective != "" {
			input.Prospective = ""
			e.Base = riskpolicy.EvaluateApprovals(input)
		}
		out[task.ID] = e
	}
	return out, nil
}
