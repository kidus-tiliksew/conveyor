package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// client is a thin wrapper over the control-plane API (design-http-api).
type client struct {
	base                string
	token               string
	workspace           string
	configErr           error
	resolved            resolvedClientConfig
	forgeTokenPreflight func(context.Context, string) error
}

func newClient() *client {
	resolved, err := resolveClientConfig()
	c := &client{base: resolved.Server.Value, token: resolved.Token.Value, workspace: resolved.Workspace.Value, configErr: err, resolved: resolved}
	c.forgeTokenPreflight = c.fetchForgeTokenPreflight
	return c
}

func (c *client) createTask(body, repo, base string) (core.Task, error) {
	return c.createTaskWithSetup(body, repo, base, false, nil, nil, "")
}

func (c *client) createTaskWithLevel(body, repo, base string, level core.EscalationLevel) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for task creation")
	}
	payload, _ := json.Marshal(map[string]string{
		"body":        body,
		"repo":        repo,
		"base_branch": base,
		"source":      "cli",
		"level":       string(level),
	})
	var t core.Task
	err := c.do(http.MethodPost, "/v1/tasks", payload, &t)
	return t, err
}

func (c *client) createTaskWithSetup(body, repo, base string, hold bool, specApproval, mergeApproval *bool, setup string) (core.Task, error) {
	return c.createTaskWithDependencies(body, repo, base, hold, specApproval, mergeApproval, setup, nil)
}

func (c *client) createTaskWithDependencies(body, repo, base string, hold bool, specApproval, mergeApproval *bool, setup string, dependsOn []string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for task creation")
	}
	payload := map[string]any{"body": body, "repo": repo, "base_branch": base, "source": "cli"}
	if setup != "" {
		payload["setup"] = setup
	}
	if len(dependsOn) > 0 {
		payload["depends_on"] = dependsOn
	}
	if hold {
		payload["hold"] = true
	}
	if specApproval != nil {
		payload["spec_approval"] = *specApproval
	}
	if mergeApproval != nil {
		payload["merge_approval"] = *mergeApproval
	}
	data, _ := json.Marshal(payload)
	var task core.Task
	err := c.do(http.MethodPost, "/v1/tasks", data, &task)
	return task, err
}

func (c *client) listTasks() ([]core.Task, error) {
	var ts []core.Task
	err := c.do(http.MethodGet, "/v1/tasks", nil, &ts)
	return ts, err
}

func (c *client) getTask(id string) (core.Task, error) {
	var t core.Task
	err := c.do(http.MethodGet, "/v1/tasks/"+id, nil, &t)
	return t, err
}

func (c *client) listJobs(taskID string) ([]core.Job, error) {
	var js []core.Job
	err := c.do(http.MethodGet, "/v1/tasks/"+taskID+"/jobs", nil, &js)
	return js, err
}

func (c *client) getLatestSpec(taskID string) (core.SpecVersion, error) {
	var spec core.SpecVersion
	err := c.do(http.MethodGet, "/v1/tasks/"+taskID+"/spec", nil, &spec)
	return spec, err
}

func (c *client) redispatchTask(id string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for redispatch")
	}
	var t core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/redispatch", []byte(`{}`), &t)
	return t, err
}

func (c *client) changeTaskSetup(id, setup, reason, requestID string, applyLatest bool) (store.SetupChangeResult, error) {
	if c.token == "" {
		return store.SetupChangeResult{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for setup changes")
	}
	payload, _ := json.Marshal(map[string]any{"setup": setup, "reason": reason, "request_id": requestID, "apply_latest": applyLatest})
	var result store.SetupChangeResult
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/setup", payload, &result)
	return result, err
}

func (c *client) reviewTask(id string, action core.InterventionAction, reasonCode, comment string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for review actions")
	}
	payload, _ := json.Marshal(map[string]string{
		"action": string(action), "reason_code": reasonCode, "comment": comment,
	})
	var response struct {
		Task core.Task `json:"task"`
	}
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/review", payload, &response)
	return response.Task, err
}

func (c *client) requestTaskChanges(id, feedback string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required to request changes")
	}
	payload, _ := json.Marshal(map[string]string{"feedback": feedback})
	var response struct {
		Task core.Task `json:"task"`
	}
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/request-changes", payload, &response)
	return response.Task, err
}

func (c *client) closeTask(id, reason string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for task close")
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	var task core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/close", payload, &task)
	return task, err
}

func (c *client) removeTaskDependency(taskID, dependencyID, reason, requestID string) (store.DependencyRemovalResult, error) {
	if c.token == "" {
		return store.DependencyRemovalResult{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for dependency removal")
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "request_id": requestID})
	var result store.DependencyRemovalResult
	err := c.do(http.MethodDelete, "/v1/tasks/"+taskID+"/dependencies/"+dependencyID, payload, &result)
	return result, err
}

func (c *client) getWorkspaceConfig() (config.VersionedDocument, error) {
	if c.token == "" {
		return config.VersionedDocument{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for workspace config")
	}
	var record config.VersionedDocument
	err := c.do(http.MethodGet, "/v1/workspace/config", nil, &record)
	return record, err
}

func (c *client) updateWorkspaceConfig(document config.WorkspaceDocument, version int64) (config.UpdateReceipt, error) {
	if c.token == "" {
		return config.UpdateReceipt{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for workspace config")
	}
	payload, err := json.Marshal(map[string]any{"document": document})
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	var receipt config.UpdateReceipt
	err = c.doHeaders(http.MethodPut, "/v1/workspace/config", payload, &receipt, map[string]string{
		"If-Match": strconv.FormatInt(version, 10),
	})
	return receipt, err
}

func (c *client) monitorStatus() (monitor.Status, error) {
	var status monitor.Status
	err := c.do(http.MethodGet, "/v1/monitor", nil, &status)
	return status, err
}

func (c *client) rebuildLineage(reason, requestID string) (core.LineageRebuildResult, error) {
	if c.token == "" {
		return core.LineageRebuildResult{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for lineage rebuild")
	}
	payload, _ := json.Marshal(core.LineageRebuildRequest{Reason: reason, RequestID: requestID})
	var result core.LineageRebuildResult
	err := c.do(http.MethodPost, "/v1/lineage/rebuild", payload, &result)
	return result, err
}

func (c *client) resolveMonitorDrift(id, outcome string) (monitor.Drift, error) {
	if c.token == "" {
		return monitor.Drift{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for drift reconciliation")
	}
	payload, _ := json.Marshal(map[string]string{"outcome": outcome})
	var drift monitor.Drift
	err := c.do(http.MethodPost, "/v1/monitor/drift/"+id+"/resolve", payload, &drift)
	return drift, err
}

func (c *client) do(method, path string, body []byte, out any) error {
	return c.doHeaders(method, path, body, out, nil)
}

func (c *client) doHeaders(method, path string, body []byte, out any, headers map[string]string) error {
	if c.configErr != nil {
		return c.configErr
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.workspace != "" {
		req.Header.Set("X-Workspace-ID", c.workspace)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) callerIdentity() (core.CallerIdentity, error) {
	var identity core.CallerIdentity
	err := c.do(http.MethodGet, "/v1/me", nil, &identity)
	return identity, err
}

func (c *client) personalAccessTokens() ([]core.PersonalAccessToken, error) {
	var tokens []core.PersonalAccessToken
	err := c.do(http.MethodGet, "/v1/tokens", nil, &tokens)
	return tokens, err
}

func (c *client) revokePersonalAccessToken(id string) error {
	return c.do(http.MethodDelete, "/v1/tokens/"+id, nil, nil)
}

func matchingPersonalAccessToken(value string, tokens []core.PersonalAccessToken) (core.PersonalAccessToken, bool) {
	for _, token := range tokens {
		if strings.HasPrefix(value, "cv_pat_"+token.ID+"_") {
			return token, true
		}
	}
	return core.PersonalAccessToken{}, false
}
