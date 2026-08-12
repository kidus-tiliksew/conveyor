package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func (c *client) getTaskRunOrderContext(ctx context.Context, credential, taskID string) (*workerservice.DispatchOrder, error) {
	var result workerservice.DispatchOrder
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/run-order", nil, &result, credential)
	if err != nil {
		return nil, err
	}
	if result.Order.ID == "" {
		return nil, nil
	}
	return &result, nil
}

func taskRunOrderPath(item workerservice.DispatchOrder, suffix string) string {
	return "/v1/tasks/" + url.PathEscape(item.Task.ID) + "/run-orders/" + url.PathEscape(item.Order.ID) + suffix
}

func (c *client) claimTaskRunOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, session, clientToken string) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(map[string]any{
		"session_id": session, "client_token": clientToken, "agent": item.Harness.Name,
		"model": item.Model, "lease_seconds": int64(workerservice.DefaultClaimLease.Seconds()),
	})
	err := c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/claim"), payload, &result, credential)
	var response *workerHTTPError
	if errors.As(err, &response) && response.StatusCode == http.StatusConflict && strings.TrimSpace(response.Message) != "" {
		return core.WorkOrder{}, errors.New(strings.TrimSpace(response.Message))
	}
	return result, err
}

func (c *client) renewTaskRunOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	err := c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/renew"), payload, &result, credential)
	return result, err
}

func (c *client) reconcileTaskRunOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string) (workerservice.ClaimReconciliation, error) {
	var result workerservice.ClaimReconciliation
	path := taskRunOrderPath(item, "/reconcile") + "?session_id=" + url.QueryEscape(sessionID)
	err := c.workerDoContext(ctx, http.MethodGet, path, nil, &result, credential)
	return result, err
}

func (c *client) releaseTaskRunOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, release core.WorkOrderRelease) error {
	payload, _ := json.Marshal(map[string]any{
		"session_id": release.SessionID, "reason": release.Reason, "release_cause": release.Cause,
		"outcome": release.Outcome, "exit_status": release.ExitStatus, "failure_detail": release.FailureDetail,
	})
	var result core.WorkOrder
	return c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/release"), payload, &result, credential)
}

func (c *client) checkpointTaskRunOrderAttemptContext(ctx context.Context, credential string, item workerservice.DispatchOrder, checkpoint core.WorkOrderAttemptCheckpoint) error {
	payload, _ := json.Marshal(checkpoint)
	var result map[string]bool
	return c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/attempt-checkpoint"), payload, &result, credential)
}

func (c *client) claimDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, session, clientToken string) (core.WorkOrder, error) {
	if item.Dispatch == "run" {
		return c.claimTaskRunOrderContext(ctx, credential, item, session, clientToken)
	}
	return c.claimWorkerOrderContext(ctx, credential, item.Order.ID, session, clientToken)
}

func (c *client) renewDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string) (core.WorkOrder, error) {
	if item.Dispatch == "run" {
		return c.renewTaskRunOrderContext(ctx, credential, item, sessionID)
	}
	return c.renewWorkerOrderContext(ctx, credential, item.Order.ID, sessionID)
}

func (c *client) reconcileDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string) (workerservice.ClaimReconciliation, error) {
	if item.Dispatch == "run" {
		return c.reconcileTaskRunOrderContext(ctx, credential, item, sessionID)
	}
	return c.reconcileWorkerOrderContext(ctx, credential, item.Order.ID, sessionID)
}

func (c *client) releaseDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, release core.WorkOrderRelease) error {
	if item.Dispatch == "run" {
		return c.releaseTaskRunOrderContext(ctx, credential, item, release)
	}
	return c.releaseWorkerOrderContext(ctx, credential, item.Order.ID, release)
}

func (c *client) checkpointDispatchOrderAttemptContext(ctx context.Context, credential string, item workerservice.DispatchOrder, checkpoint core.WorkOrderAttemptCheckpoint) error {
	if item.Dispatch == "run" {
		return c.checkpointTaskRunOrderAttemptContext(ctx, credential, item, checkpoint)
	}
	return c.checkpointWorkerOrderAttemptContext(ctx, credential, item.Order.ID, checkpoint)
}
