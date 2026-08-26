package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if result.Order.ID == "" && result.Task.ID == "" {
		return nil, nil
	}
	return &result, nil
}

func (c *client) confirmTaskRunProposalContext(ctx context.Context, credential, taskID string, proposal workerservice.TaskRunProposal) error {
	switch proposal.Kind {
	case "design":
		var result map[string]any
		path := "/v1/system-designs/" + url.PathEscape(proposal.DocumentID) + "/versions/" + fmt.Sprint(proposal.Version) + "/confirm"
		return c.workerDoContext(ctx, http.MethodPost, path, nil, &result, credential)
	case "decision":
		var result core.Decision
		return c.workerDoContext(ctx, http.MethodPost, "/v1/decisions/"+url.PathEscape(proposal.DocumentID)+"/confirm", nil, &result, credential)
	case "plan_revision":
		return c.reviewTaskRunGateContext(ctx, credential, taskID, core.InterventionRedirect, "plan-revision-approved", "")
	default:
		return fmt.Errorf("unsupported task run proposal kind %q", proposal.Kind)
	}
}

func (c *client) approveTaskRunGateContext(ctx context.Context, credential string, item workerservice.DispatchOrder) error {
	action, reason := core.InterventionApprove, "approved"
	if item.Gate != nil && item.Gate.Kind == "plan_revision" {
		action, reason = core.InterventionRedirect, "plan-revision-approved"
	}
	return c.reviewTaskRunGateContext(ctx, credential, item.Task.ID, action, reason, "")
}

func (c *client) requestTaskRunGateChangesContext(ctx context.Context, credential string, item workerservice.DispatchOrder, feedback string) error {
	if item.Gate != nil && item.Gate.Kind == "merge" {
		payload, _ := json.Marshal(map[string]string{"feedback": strings.TrimSpace(feedback)})
		var response struct {
			Task core.Task `json:"task"`
		}
		return c.workerDoContext(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(item.Task.ID)+"/request-changes", payload, &response, credential)
	}
	reason := "changes-requested"
	if item.Gate != nil && item.Gate.Kind == "plan_revision" {
		reason = "plan-revision-declined"
	}
	return c.reviewTaskRunGateContext(ctx, credential, item.Task.ID, core.InterventionRedirect, reason, strings.TrimSpace(feedback))
}

func (c *client) reviewTaskRunGateContext(ctx context.Context, credential, taskID string, action core.InterventionAction, reason, comment string) error {
	payload, _ := json.Marshal(map[string]string{"action": string(action), "reason_code": reason, "comment": comment})
	var response struct {
		Task core.Task `json:"task"`
	}
	return c.workerDoContext(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/review", payload, &response, credential)
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

type issuedTaskRunAgentCredential struct {
	ID    string `json:"credential_id"`
	Value string `json:"credential"`
}

func (c *client) issueTaskRunAgentCredentialContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string) (issuedTaskRunAgentCredential, error) {
	var result issuedTaskRunAgentCredential
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	err := c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/agent-credential"), payload, &result, credential)
	return result, err
}

func (c *client) revokeTaskRunAgentCredentialContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID, credentialID string) error {
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID, "credential_id": credentialID})
	var result map[string]any
	return c.workerDoContext(ctx, http.MethodDelete, taskRunOrderPath(item, "/agent-credential"), payload, &result, credential)
}

func (c *client) renewTaskRunOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string, snapshot *core.WorkOrderActivitySnapshotInput) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(workerRenewRequest{SessionID: sessionID, ActivitySnapshot: snapshot})
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
	payloadValue := map[string]any{
		"session_id": release.SessionID, "reason": release.Reason, "release_cause": release.Cause,
		"outcome": release.Outcome, "exit_status": release.ExitStatus, "failure_detail": release.FailureDetail,
	}
	if release.Checkpoint != nil {
		payloadValue["checkpoint"] = release.Checkpoint
	}
	payload, _ := json.Marshal(payloadValue)
	var result core.WorkOrder
	return c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/release"), payload, &result, credential)
}

func (c *client) checkpointTaskRunOrderAttemptContext(ctx context.Context, credential string, item workerservice.DispatchOrder, checkpoint core.WorkOrderAttemptCheckpoint, transcript *core.WorkOrderAttemptTranscript) error {
	checkpoint.Transcript = transcript
	payload, _ := json.Marshal(checkpoint)
	var result map[string]bool
	return c.workerDoContext(ctx, http.MethodPost, taskRunOrderPath(item, "/attempt-checkpoint"), payload, &result, credential)
}

// checkpointTaskRunOrderAttemptByIDContext posts one predecessor attempt
// checkpoint through the task run plane for callers that hold only the
// assigned task and work-order identity (conveyor checkout's recording
// fallback). The payload is the same checkpoint encoding the dispatch planes
// send, with no transcript attached.
func (c *client) checkpointTaskRunOrderAttemptByIDContext(ctx context.Context, credential, taskID, orderID string, checkpoint core.WorkOrderAttemptCheckpoint) error {
	payload, _ := json.Marshal(checkpoint)
	var result map[string]bool
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/run-orders/" + url.PathEscape(orderID) + "/attempt-checkpoint"
	return c.workerDoContext(ctx, http.MethodPost, path, payload, &result, credential)
}

func (c *client) claimDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, session, clientToken string) (workerservice.ClaimDelivery, error) {
	if item.Dispatch == "run" {
		order, err := c.claimTaskRunOrderContext(ctx, credential, item, session, clientToken)
		return workerservice.ClaimDelivery{WorkOrder: order}, err
	}
	return c.claimWorkerOrderContext(ctx, credential, item.Order.ID, session, clientToken)
}

func (c *client) renewDispatchOrderContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string, snapshot *core.WorkOrderActivitySnapshotInput) (core.WorkOrder, error) {
	if item.Dispatch == "run" {
		return c.renewTaskRunOrderContext(ctx, credential, item, sessionID, snapshot)
	}
	return c.renewWorkerOrderContext(ctx, credential, item.Order.ID, sessionID, snapshot)
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

func (c *client) checkpointDispatchOrderAttemptContext(ctx context.Context, credential string, item workerservice.DispatchOrder, checkpoint core.WorkOrderAttemptCheckpoint, transcript *core.WorkOrderAttemptTranscript) error {
	if item.Dispatch == "run" {
		return c.checkpointTaskRunOrderAttemptContext(ctx, credential, item, checkpoint, transcript)
	}
	return c.checkpointWorkerOrderAttemptWithTranscriptContext(ctx, credential, item.Order.ID, checkpoint, transcript)
}

func (c *client) reportDispatchProgressContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID, message string) error {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "report_progress",
			"arguments": map[string]any{
				"workspace_id": c.workspace, "work_order_id": item.Order.ID,
				"session_id": sessionID, "message": message,
			},
		},
	})
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.workerDoContext(ctx, http.MethodPost, "/mcp", payload, &envelope, credential); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("MCP %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result.IsError {
		message := "report_progress failed"
		if len(envelope.Result.Content) > 0 && strings.TrimSpace(envelope.Result.Content[0].Text) != "" {
			message = strings.TrimSpace(envelope.Result.Content[0].Text)
		}
		return errors.New(message)
	}
	return nil
}

func (c *client) reportDispatchContinuationContext(ctx context.Context, credential string, item workerservice.DispatchOrder, sessionID string, continuation core.WorkOrderContinuation) error {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "report_continuation",
			"arguments": map[string]any{
				"workspace_id": c.workspace, "work_order_id": item.Order.ID, "session_id": sessionID,
				"continuation_session_id": continuation.SessionID, "attempt_id": continuation.AttemptID,
				"harness": continuation.Harness, "launch_environment": continuation.LaunchEnvironment,
			},
		},
	})
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.workerDoContext(ctx, http.MethodPost, "/mcp", payload, &envelope, credential); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("MCP %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result.IsError {
		message := "report_continuation failed"
		if len(envelope.Result.Content) > 0 && strings.TrimSpace(envelope.Result.Content[0].Text) != "" {
			message = strings.TrimSpace(envelope.Result.Content[0].Text)
		}
		return errors.New(message)
	}
	return nil
}
