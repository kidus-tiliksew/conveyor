package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type workerHTTPError struct {
	StatusCode int
	Status     string
	Message    string
	Code       string
}

func (e *workerHTTPError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return e.Status + ": " + e.Message
}

// transientWorkerError is deliberately narrow: authentication, conflicts,
// malformed responses, and invalid configuration must fail closed instead of
// disappearing into the reconnect loop (spec §21.26).
func transientWorkerError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var response *workerHTTPError
	if errors.As(err, &response) {
		return response.StatusCode >= 500 && response.StatusCode <= 599
	}
	var network net.Error
	return errors.As(err, &network)
}

type workerListResponse struct {
	Workers               []core.Worker          `json:"workers"`
	AutoAvailable         bool                   `json:"auto_available"`
	AutoUnavailableReason string                 `json:"auto_unavailable_reason"`
	RateLimits            []core.RateLimitHealth `json:"rate_limits"`
}

func (c *client) issueWorkerPairing(ttl time.Duration) (string, time.Time, error) {
	var response struct {
		PairingToken string    `json:"pairing_token"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	payload, _ := json.Marshal(map[string]int64{"ttl_seconds": int64(ttl.Seconds())})
	err := c.do(http.MethodPost, "/v1/workers/pairings", payload, &response)
	return response.PairingToken, response.ExpiresAt, err
}

func (c *client) listWorkers() (workerListResponse, error) {
	var result workerListResponse
	err := c.do(http.MethodGet, "/v1/workers", nil, &result)
	return result, err
}
func (c *client) revokeWorker(id string) error {
	var ignored any
	return c.do(http.MethodDelete, "/v1/workers/"+id, nil, &ignored)
}

func (c *client) enrollWorker(pairing, name string) (workerservice.Enrollment, error) {
	var result workerservice.Enrollment
	payload, _ := json.Marshal(map[string]string{"pairing_token": pairing, "name": name})
	err := c.workerDo(http.MethodPost, "/v1/worker/enroll", payload, &result, "")
	return result, err
}

func (c *client) workerConfig(credential string) (workerservice.WorkerConfig, error) {
	return c.workerConfigContext(context.Background(), credential)
}
func (c *client) workerConfigContext(ctx context.Context, credential string) (workerservice.WorkerConfig, error) {
	var result workerservice.WorkerConfig
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/worker/config", nil, &result, credential)
	return result, err
}
func (c *client) heartbeatWorker(credential string, probes []core.HarnessProbe) (core.Worker, error) {
	return c.heartbeatWorkerContext(context.Background(), credential, probes)
}
func (c *client) heartbeatWorkerContext(ctx context.Context, credential string, probes []core.HarnessProbe) (core.Worker, error) {
	var result core.Worker
	payload, _ := json.Marshal(map[string]any{"probes": probes})
	err := c.workerDoContext(ctx, http.MethodPost, "/v1/worker/heartbeat", payload, &result, credential)
	return result, err
}
func (c *client) listWorkerOrders(credential string) ([]workerservice.DispatchOrder, error) {
	return c.listWorkerOrdersContext(context.Background(), credential)
}
func (c *client) listWorkerOrdersContext(ctx context.Context, credential string) ([]workerservice.DispatchOrder, error) {
	var result []workerservice.DispatchOrder
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/worker/work-orders", nil, &result, credential)
	return result, err
}
func (c *client) claimWorkerOrder(credential, id, session, clientToken string) (core.WorkOrder, error) {
	return c.claimWorkerOrderContext(context.Background(), credential, id, session, clientToken)
}
func (c *client) claimWorkerOrderContext(ctx context.Context, credential, id, session, clientToken string) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(map[string]any{"session_id": session, "client_token": clientToken, "lease_seconds": int64(workerservice.DefaultClaimLease.Seconds())})
	err := c.workerDoContext(ctx, http.MethodPost, "/v1/worker/work-orders/"+id+"/claim", payload, &result, credential)
	return result, err
}
func (c *client) renewWorkerOrder(credential, id, sessionID string) (core.WorkOrder, error) {
	return c.renewWorkerOrderContext(context.Background(), credential, id, sessionID)
}
func (c *client) renewWorkerOrderContext(ctx context.Context, credential, id, sessionID string) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	err := c.workerDoContext(ctx, http.MethodPost, "/v1/worker/work-orders/"+id+"/renew", payload, &result, credential)
	return result, err
}
func (c *client) reconcileWorkerOrderContext(ctx context.Context, credential, id, sessionID string) (workerservice.ClaimReconciliation, error) {
	result, err := c.reconcileWorkerOrderReadOnlyContext(ctx, credential, id, sessionID)
	var response *workerHTTPError
	if (errors.As(err, &response) && response.StatusCode == http.StatusNotFound) || (err == nil && result.WorkOrder.ID == "") {
		// Compatibility for older control planes during rolling upgrades. The
		// renew response is still server-authoritative and retains stale-session
		// rejection; new servers use the read-only reconciliation endpoint.
		order, renewErr := c.renewWorkerOrderContext(ctx, credential, id, sessionID)
		if renewErr != nil {
			return result, renewErr
		}
		result.WorkOrder = order
		result.Authorized = order.State == core.WorkOrderClaimed
		result.Reason = "legacy control plane confirmed the active claim by renewal"
		return result, nil
	}
	return result, err
}

func (c *client) reconcileWorkerOrderReadOnlyContext(ctx context.Context, credential, id, sessionID string) (workerservice.ClaimReconciliation, error) {
	var result workerservice.ClaimReconciliation
	path := "/v1/worker/work-orders/" + id + "/reconcile?session_id=" + url.QueryEscape(sessionID)
	err := c.workerDoContext(ctx, http.MethodGet, path, nil, &result, credential)
	return result, err
}

func (c *client) reportWorkerFallbackUsageContext(ctx context.Context, credential, id, sessionID string, tokensIn, tokensOut int64) error {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "report_usage",
			"arguments": map[string]any{
				"workspace_id": c.workspace, "work_order_id": id, "session_id": sessionID,
				"tokens_in": tokensIn, "tokens_out": tokensOut, "cost_usd": 0, "source": "worker_fallback",
			},
		},
	})
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.workerDoContext(ctx, http.MethodPost, "/mcp", payload, &envelope, credential); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("report worker fallback usage: MCP %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return nil
}
func (c *client) releaseWorkerOrder(credential, id string, release core.WorkOrderRelease) error {
	return c.releaseWorkerOrderContext(context.Background(), credential, id, release)
}
func (c *client) releaseWorkerOrderContext(ctx context.Context, credential, id string, release core.WorkOrderRelease) error {
	payload, _ := json.Marshal(map[string]any{"session_id": release.SessionID, "reason": release.Reason, "outcome": release.Outcome, "exit_status": release.ExitStatus, "failure_detail": release.FailureDetail})
	var ignored core.WorkOrder
	return c.workerDoContext(ctx, http.MethodPost, "/v1/worker/work-orders/"+id+"/release", payload, &ignored, credential)
}

func (c *client) checkpointWorkerOrderAttemptContext(ctx context.Context, credential, id string, checkpoint core.WorkOrderAttemptCheckpoint) error {
	payload, _ := json.Marshal(checkpoint)
	var result map[string]bool
	return c.workerDoContext(ctx, http.MethodPost, "/v1/worker/work-orders/"+id+"/attempt-checkpoint", payload, &result, credential)
}

func (c *client) workerDo(method, path string, body []byte, out any, credential string) error {
	return c.workerDoContext(context.Background(), method, path, body, out, credential)
}

func (c *client) workerDoContext(ctx context.Context, method, path string, body []byte, out any, credential string) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if c.workspace != "" {
		req.Header.Set("X-Workspace-ID", c.workspace)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("worker API transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		message, _ := io.ReadAll(resp.Body)
		return &workerHTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Message: string(bytes.TrimSpace(message)), Code: resp.Header.Get("X-Conveyor-Error-Code")}
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
