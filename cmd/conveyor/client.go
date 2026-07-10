package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// client is a thin wrapper over the control-plane API (spec §17.3).
type client struct {
	base  string
	token string
}

func newClient() *client {
	base := os.Getenv("CONVEYOR_ADDR")
	if base == "" {
		base = "http://localhost:8080"
	}
	return &client{base: base, token: os.Getenv("CONVEYOR_API_TOKEN")}
}

func (c *client) createTask(title, body, repo, base string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for task creation")
	}
	payload, _ := json.Marshal(map[string]string{
		"title":       title,
		"body":        body,
		"repo":        repo,
		"base_branch": base,
		"source":      "cli",
	})
	var t core.Task
	err := c.do(http.MethodPost, "/v1/tasks", payload, &t)
	return t, err
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

func (c *client) redispatchTask(id string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("CONVEYOR_API_TOKEN is required for redispatch")
	}
	var t core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/redispatch", []byte(`{}`), &t)
	return t, err
}

func (c *client) do(method, path string, body []byte, out any) error {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
