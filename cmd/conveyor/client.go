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

// client is a thin wrapper over the control-plane API (component-runtime).
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
	return c.createTaskWithDependencies(body, repo, base, nil, nil, nil)
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

// createTaskWithDependencies sends pipeline-policy intake only: gates and
// dependencies. A task carries no hold (DEC-74(3)), and setup selection is
// client-local (DEC-56(2)).
func (c *client) createTaskWithDependencies(body, repo, base string, specApproval, mergeApproval *bool, dependsOn []string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("a credential is required for task creation; run `conveyor auth login`")
	}
	payload := map[string]any{"body": body, "repo": repo, "base_branch": base, "source": "cli"}
	if len(dependsOn) > 0 {
		payload["depends_on"] = dependsOn
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

// openTaskStates are the task states other than merged and closed.
var openTaskStates = []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}

// taskListPageLimit is the largest page GET /v1/tasks serves.
const taskListPageLimit = 200

// taskListPage is one GET /v1/tasks page. HasTotal is false when the server
// sent no X-Conveyor-Total, as a server that predates paging does; its body is
// then the complete list.
type taskListPage struct {
	Tasks    []core.Task
	Total    int
	HasTotal bool
}

// listTaskPage reads one page of GET /v1/tasks under the shared filter family
// (component-http-api, Task list read).
func (c *client) listTaskPage(query url.Values) (taskListPage, error) {
	path := "/v1/tasks"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	body, header, err := c.getRawWithHeader(path)
	if err != nil {
		return taskListPage{}, err
	}
	var page taskListPage
	if err = json.Unmarshal(body, &page.Tasks); err != nil {
		return taskListPage{}, err
	}
	if raw := header.Get("X-Conveyor-Total"); raw != "" {
		if page.Total, err = strconv.Atoi(raw); err != nil {
			return taskListPage{}, fmt.Errorf("invalid X-Conveyor-Total %q", raw)
		}
		page.HasTotal = true
	}
	return page, nil
}

// listOpenTasksHoldingBranch reads every open task of repo whose identifying
// text contains branch, a page of 200 at a time. The occupied-branch rule
// still compares repository, branch, and state exactly, so an older server
// that ignores the filter answers correctly with its whole list
// (component-http-api, Task list read).
func (c *client) listOpenTasksHoldingBranch(repo, branch string) ([]core.Task, error) {
	query := url.Values{}
	for _, state := range openTaskStates {
		query.Add("state", string(state))
	}
	if repo != "" {
		query.Set("repository", repo)
	}
	if branch != "" {
		query.Set("q", branch)
	}
	query.Set("limit", strconv.Itoa(taskListPageLimit))
	var tasks []core.Task
	for offset := 0; ; {
		query.Set("offset", strconv.Itoa(offset))
		page, err := c.listTaskPage(query)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, page.Tasks...)
		offset += len(page.Tasks)
		if !page.HasTotal || len(page.Tasks) == 0 || offset >= page.Total {
			return tasks, nil
		}
	}
}

func (c *client) getTask(id string) (core.Task, error) {
	var t core.Task
	err := c.do(http.MethodGet, "/v1/tasks/"+id, nil, &t)
	return t, err
}

type attachBranchError struct {
	Code        string
	Message     string
	OtherTaskID string
	status      int
}

func (e *attachBranchError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Code == "branch_in_use" && strings.TrimSpace(e.OtherTaskID) != "":
		return "branch_in_use: branch already belongs to task " + e.OtherTaskID
	case e.Code != "" && e.Message != "":
		if strings.HasPrefix(e.Message, e.Code) {
			return e.Message
		}
		return e.Code + ": " + e.Message
	case e.Code != "":
		return e.Code
	case strings.TrimSpace(e.Message) != "":
		return e.Message
	default:
		return fmt.Sprintf("attach task branch failed: %s", http.StatusText(e.status))
	}
}

func (c *client) attachTaskBranch(taskID, branch string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("a credential is required to attach a task branch; run `conveyor auth login`")
	}
	payload, err := json.Marshal(map[string]string{"branch": branch})
	if err != nil {
		return core.Task{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/tasks/"+url.PathEscape(taskID)+"/branch", bytes.NewReader(payload))
	if err != nil {
		return core.Task{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.workspace != "" {
		req.Header.Set("X-Workspace-ID", c.workspace)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Task{}, err
	}
	if resp.StatusCode >= 300 {
		var encoded struct {
			Error       string `json:"error"`
			Message     string `json:"message"`
			OtherTaskID string `json:"other_task_id"`
		}
		if json.Unmarshal(body, &encoded) == nil && strings.TrimSpace(encoded.Error) != "" {
			return core.Task{}, &attachBranchError{
				Code: encoded.Error, Message: encoded.Message, OtherTaskID: encoded.OtherTaskID, status: resp.StatusCode,
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return core.Task{}, fmt.Errorf("%s: %s (%s)", resp.Status, bytes.TrimSpace(body), c.credentialDiagnostic())
		}
		return core.Task{}, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var task core.Task
	if err := json.Unmarshal(body, &task); err != nil {
		return core.Task{}, err
	}
	return task, nil
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

// mergeTask posts the explicit merge act. The route answers the bare task and
// refuses with 409 unless the forge confirms the merge.
func (c *client) mergeTask(id string) (core.Task, error) {
	if c.token == "" {
		return core.Task{}, fmt.Errorf("a credential is required to merge a task; run `conveyor auth login`")
	}
	var task core.Task
	err := c.do(http.MethodPost, "/v1/tasks/"+url.PathEscape(id)+"/merge", []byte("{}"), &task)
	return task, err
}

// taskMergeGate is the task activity slice `task merge` needs to confirm
// the one-act gate merge: the task and the readiness peek the activity
// envelope carries at the merge_approval gate (DEC-64(3)).
type taskMergeGate struct {
	Task           core.Task                            `json:"task"`
	MergeReadiness *workerservice.TaskRunMergeReadiness `json:"merge_readiness"`
}

func (c *client) taskMergeGate(id string) (taskMergeGate, error) {
	var gate taskMergeGate
	err := c.do(http.MethodGet, "/v1/tasks/"+url.PathEscape(id)+"/activity", nil, &gate)
	return gate, err
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
	resp, err := c.send(method, path, body, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// getRawWithHeader is a GET that returns the body bytes and response headers.
func (c *client) getRawWithHeader(path string) ([]byte, http.Header, error) {
	resp, err := c.send(http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Header, nil
}

// send issues one authenticated request. A status of 300 or above becomes an
// error carrying the server's text; otherwise the caller owns the open body.
func (c *client) send(method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	if c.configErr != nil {
		return nil, c.configErr
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, &apiStatusError{Status: resp.StatusCode, message: fmt.Sprintf("%s: %s (%s)", resp.Status, bytes.TrimSpace(msg), c.credentialDiagnostic())}
		}
		return nil, &apiStatusError{Status: resp.StatusCode, message: fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(msg))}
	}
	return resp, nil
}

// apiStatusError is a refused request. Its text is the status and the
// server's message; Status lets a caller branch on one status, such as a 404
// from a server that predates a route.
type apiStatusError struct {
	Status  int
	message string
}

func (e *apiStatusError) Error() string { return e.message }

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
	requestID := strings.TrimSpace(request.RequestID)
	err = c.doHeaders(http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/restart", body, &result, map[string]string{
		"X-Request-Id":      requestID,
		"X-Idempotency-Key": requestID,
	})
	if err != nil {
		return result, wrapRestartClientError(err, requestID)
	}
	return result, nil
}

func wrapRestartClientError(err error, requestID string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	msg := text
	_, body, ok := strings.Cut(text, ": ")
	if ok {
		var parsed struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed) == nil && (parsed.Error != "" || parsed.Message != "") {
			switch {
			case parsed.Error != "" && parsed.Message != "":
				msg = parsed.Error + ": " + parsed.Message
			case parsed.Error != "":
				msg = parsed.Error
			default:
				msg = parsed.Message
			}
			prefix, _, _ := strings.Cut(text, ": ")
			msg = prefix + ": " + msg
		}
	}
	if requestID = strings.TrimSpace(requestID); requestID == "" {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("restart request %s failed: %s", requestID, msg)
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
