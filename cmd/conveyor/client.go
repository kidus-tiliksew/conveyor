package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// client is a thin wrapper over the control-plane API (spec §17.3).
type client struct {
	base      string
	token     string
	workspace string
}

func newClient() *client {
	base := os.Getenv("CONVEYOR_ADDR")
	if base == "" {
		base = "http://localhost:8080"
	}
	return &client{base: base, token: os.Getenv("CONVEYOR_API_TOKEN"), workspace: workspaceFlag}
}

func (c *client) createTask(body, repo, base string) (core.Task, error) {
	return c.createTaskWithMode(body, repo, base, "", nil, nil)
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

func (c *client) createTaskWithMode(body, repo, base string, mode core.TaskMode, specApproval, mergeApproval *bool) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for task creation")
	}
	payload := map[string]any{"body": body, "repo": repo, "base_branch": base, "source": "cli"}
	if mode != "" {
		payload["mode"] = mode
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

func (c *client) do(method, path string, body []byte, out any) error {
	return c.doHeaders(method, path, body, out, nil)
}

func (c *client) doHeaders(method, path string, body []byte, out any, headers map[string]string) error {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-Conveyor-Actor", "cli-operator")
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
