package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// client is a thin wrapper over the control-plane API (design-http-api).
type client struct {
	gitCredentials *localGitCredential
	gitPreflight   func(context.Context, workerservice.DispatchOrder, []string) error
	base           string
	token          string
	workspace      string
	configErr      error
	resolved       resolvedClientConfig
}

func newClient() *client {
	resolved, err := resolveClientConfig()
	c := &client{base: resolved.Server.Value, token: resolved.Token.Value, workspace: resolved.Workspace.Value, configErr: err, resolved: resolved}
	return c
}

func (c *client) createTask(body, repo, base string) (core.Task, error) {
	return c.createTaskWithSetup(body, repo, base, false, nil, nil, "")
}

func (c *client) createTaskWithLevel(body, repo, base string, level core.EscalationLevel) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("a credential is required for task creation; run `conveyor auth login`")
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
		return core.Task{}, fmt.Errorf("a credential is required for task creation; run `conveyor auth login`")
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
		return core.Task{}, fmt.Errorf("a credential is required for redispatch; run `conveyor auth login`")
	}
	var t core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/redispatch", []byte(`{}`), &t)
	return t, err
}

func (c *client) changeTaskSetup(id, setup, reason, requestID string, applyLatest bool) (store.SetupChangeResult, error) {
	if c.token == "" {
		return store.SetupChangeResult{}, fmt.Errorf("a credential is required for setup changes; run `conveyor auth login`")
	}
	payload, _ := json.Marshal(map[string]any{"setup": setup, "reason": reason, "request_id": requestID, "apply_latest": applyLatest})
	var result store.SetupChangeResult
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/setup", payload, &result)
	return result, err
}

func (c *client) reviewTask(id string, action core.InterventionAction, reasonCode, comment string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("a credential is required for review actions; run `conveyor auth login`")
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
		return core.Task{}, fmt.Errorf("a credential is required to request changes; run `conveyor auth login`")
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
		return core.Task{}, fmt.Errorf("a credential is required for task close; run `conveyor auth login`")
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	var task core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+id+"/close", payload, &task)
	return task, err
}

func (c *client) removeTaskDependency(taskID, dependencyID, reason, requestID string) (store.DependencyRemovalResult, error) {
	if c.token == "" {
		return store.DependencyRemovalResult{}, fmt.Errorf("a credential is required for dependency removal; run `conveyor auth login`")
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "request_id": requestID})
	var result store.DependencyRemovalResult
	err := c.do(http.MethodDelete, "/v1/tasks/"+taskID+"/dependencies/"+dependencyID, payload, &result)
	return result, err
}

func (c *client) addTaskDependency(taskID, dependencyID, reason, requestID string) (store.DependencyAdditionResult, error) {
	if c.token == "" {
		return store.DependencyAdditionResult{}, fmt.Errorf("a credential is required for dependency linking; run `conveyor auth login`")
	}
	payload, _ := json.Marshal(map[string]string{"depends_on_task_id": dependencyID, "reason": reason, "request_id": requestID})
	var result store.DependencyAdditionResult
	err := c.do(http.MethodPost, "/v1/tasks/"+taskID+"/dependencies", payload, &result)
	return result, err
}

func (c *client) getWorkspaceConfig() (config.VersionedDocument, error) {
	if c.token == "" {
		return config.VersionedDocument{}, fmt.Errorf("a credential is required for workspace config; run `conveyor auth login`")
	}
	var record config.VersionedDocument
	err := c.do(http.MethodGet, "/v1/workspace/config", nil, &record)
	return record, err
}

func (c *client) updateWorkspaceConfig(document config.WorkspaceDocument, version int64) (config.UpdateReceipt, error) {
	if c.token == "" {
		return config.UpdateReceipt{}, fmt.Errorf("a credential is required for workspace config; run `conveyor auth login`")
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
		return core.LineageRebuildResult{}, fmt.Errorf("a credential is required for lineage rebuild; run `conveyor auth login`")
	}
	payload, _ := json.Marshal(core.LineageRebuildRequest{Reason: reason, RequestID: requestID})
	var result core.LineageRebuildResult
	err := c.do(http.MethodPost, "/v1/lineage/rebuild", payload, &result)
	return result, err
}

func (c *client) resolveMonitorDrift(id, outcome string) (monitor.Drift, error) {
	if c.token == "" {
		return monitor.Drift{}, fmt.Errorf("a credential is required for drift reconciliation; run `conveyor auth login`")
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
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s: %s (%s)", resp.Status, bytes.TrimSpace(msg), c.credentialDiagnostic())
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// credentialDiagnostic names the credential source and whether a stored
// credential exists for the resolved server. It never includes a credential
// value (req-cli-authentication AC-2.1).
func (c *client) credentialDiagnostic() string {
	switch {
	case c.resolved.Token.Source != "":
		detail := "token from " + c.resolved.Token.Source
		if c.resolved.StoredCredential {
			detail += fmt.Sprintf("; a stored credential exists for %s", c.base)
		} else {
			detail += fmt.Sprintf("; no stored credential exists for %s", c.base)
		}
		return detail
	case c.token != "":
		return "token from an untracked credential source"
	default:
		return fmt.Sprintf("no credential is configured for %s; run `conveyor auth login`", c.base)
	}
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

// Restart preview reads stay on the authenticated operator surfaces
// (req-task-lifecycle-and-queue AC-7.1 through AC-7.5; component-runtime).
type taskRestartActivity struct {
	WorkOrders []core.WorkOrder `json:"work_orders"`
	Events     []core.Event     `json:"events"`
}

func (c *client) taskRestartActivity(id string) (taskRestartActivity, error) {
	var activity taskRestartActivity
	err := c.do(http.MethodGet, "/v1/tasks/"+url.PathEscape(id)+"/activity", nil, &activity)
	return activity, err
}

func (c *client) taskRestartProposals(id string) ([]core.PendingProposal, error) {
	var response struct {
		Items []core.PendingProposal `json:"items"`
	}
	if err := c.do(http.MethodGet, "/v1/pending-proposals", nil, &response); err != nil {
		return nil, err
	}
	var result []core.PendingProposal
	for _, p := range response.Items {
		if p.OriginType == "task" && p.OriginID == id && p.Tier != "task_context" {
			result = append(result, p)
		}
	}
	return result, nil
}

func (c *client) restartTask(request core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	var result core.TaskStartOverResult
	if c.token == "" {
		return result, fmt.Errorf("a credential is required for task restart; run `conveyor auth login`")
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	err = c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/restart", body, &result)
	return result, err
}

func (c *client) restartPullRequest(ctx context.Context, task core.Task) (github.SubmissionPullRequest, error) {
	var empty github.SubmissionPullRequest
	cfg, err := c.getWorkspaceConfig()
	if err != nil {
		return empty, fmt.Errorf("read repository configuration: %w", err)
	}
	repo := ""
	for _, entry := range cfg.Document.Repos {
		if entry.Name == task.Repo {
			repo = entry.GitHub
			break
		}
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " ?#%\\") || task.Branch == "" {
		return empty, fmt.Errorf("task repository has no usable GitHub repository or branch in workspace configuration")
	}
	local, err := c.withLocalGitCredential()
	if err != nil {
		return empty, err
	}
	g := local.gitCredentials
	token := g.token
	if token == "" && os.Getenv(gitAskPassModeEnv) == "1" {
		token = os.Getenv(gitAskPassTokenEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, localGitPreflightTimeout)
	defer cancel()
	if token == "" {
		fill := exec.CommandContext(ctx, "git", "credential", "fill")
		fill.Env = isolatedChildEnvironment(g.environment(), map[string]string{"GIT_TERMINAL_PROMPT": "0"})
		fill.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
		fill.Stderr = io.Discard
		raw, err := fill.Output()
		if err != nil {
			return empty, fmt.Errorf("resolve GitHub preview credential: %s", submitCredentialRemedy)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if value, ok := strings.CutPrefix(line, "password="); ok {
				token = value
			}
		}
	}
	if token == "" {
		return empty, fmt.Errorf("resolve GitHub preview credential: %s", submitCredentialRemedy)
	}
	pr, err := github.SubmissionPRForBranch(github.WithCredential(ctx, token, "executing machine Git credential"), repo, task.Branch)
	if err != nil {
		clean, _ := g.redactor(token, c.token).Redact(err.Error())
		return empty, fmt.Errorf("read GitHub pull request: %s", clean)
	}
	return pr, nil
}
