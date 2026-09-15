package main

import (
	"context"
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"net/http"
	"net/url"
)

func (c *client) worktreeHandoffContext(ctx context.Context, credential string, item workerservice.DispatchOrder, request core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	var result core.WorktreeHandoff
	payload, _ := json.Marshal(request)
	workerPath := "/v1/worker/work-orders/" + url.PathEscape(item.Order.ID) + "/worktree-handoff"
	runPath := "/v1/tasks/" + url.PathEscape(item.Task.ID) + "/run-orders/" + url.PathEscape(item.Order.ID) + "/worktree-handoff"
	path := workerPath
	if item.Dispatch == "run" {
		path = runPath
	}
	err := c.workerDoContext(ctx, http.MethodPost, path, payload, &result, credential)
	if item.Dispatch == "" && workerPlaneUnauthorized(err) {
		err = c.workerDoContext(ctx, http.MethodPost, runPath, payload, &result, credential)
	}
	return result, err
}
