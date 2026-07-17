package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type workerListResponse struct {
	Workers               []core.Worker `json:"workers"`
	AutoAvailable         bool          `json:"auto_available"`
	AutoUnavailableReason string        `json:"auto_unavailable_reason"`
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
	var result workerservice.WorkerConfig
	err := c.workerDo(http.MethodGet, "/v1/worker/config", nil, &result, credential)
	return result, err
}
func (c *client) heartbeatWorker(credential string, probes []core.HarnessProbe) (core.Worker, error) {
	var result core.Worker
	payload, _ := json.Marshal(map[string]any{"probes": probes})
	err := c.workerDo(http.MethodPost, "/v1/worker/heartbeat", payload, &result, credential)
	return result, err
}
func (c *client) listWorkerOrders(credential string) ([]workerservice.DispatchOrder, error) {
	var result []workerservice.DispatchOrder
	err := c.workerDo(http.MethodGet, "/v1/worker/work-orders", nil, &result, credential)
	return result, err
}
func (c *client) claimWorkerOrder(credential, id, session, clientToken string) (core.WorkOrder, error) {
	var result core.WorkOrder
	payload, _ := json.Marshal(map[string]any{"session_id": session, "client_token": clientToken, "lease_seconds": int64(workerservice.DefaultClaimLease.Seconds())})
	err := c.workerDo(http.MethodPost, "/v1/worker/work-orders/"+id+"/claim", payload, &result, credential)
	return result, err
}
func (c *client) renewWorkerOrder(credential, id string) (core.WorkOrder, error) {
	var result core.WorkOrder
	err := c.workerDo(http.MethodPost, "/v1/worker/work-orders/"+id+"/renew", []byte(`{}`), &result, credential)
	return result, err
}
func (c *client) releaseWorkerOrder(credential, id, reason string) error {
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	var ignored core.WorkOrder
	return c.workerDo(http.MethodPost, "/v1/worker/work-orders/"+id+"/release", payload, &ignored, credential)
}

func (c *client) workerDo(method, path string, body []byte, out any, credential string) error {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
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
		return fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		message, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(message))
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
