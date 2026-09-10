package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"github.com/spf13/cobra"
)

const submitCredentialRemedy = "set CONVEYOR_GIT_TOKEN, or configure git credential fill for https://github.com on this machine"

func submitCmd() *cobra.Command {
	return &cobra.Command{Use: "submit <task-id>", Short: "Push the task branch, open its pull request locally, and submit for review", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newClient().withLocalGitCredential()
		if err != nil {
			return err
		}
		directory, err := os.Getwd()
		if err != nil {
			return err
		}
		result, err := c.submitTask(cmd.Context(), args[0], os.Getenv("CONVEYOR_WORK_ORDER_ID"), os.Getenv("CONVEYOR_SESSION_ID"), directory)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
}

// submitTask keeps all forge credentials local. Every potentially credential-
// bearing diagnostic is scrubbed before returning it to the caller.
func (c *client) submitTask(ctx context.Context, taskID, orderID, session, directory string) (result map[string]any, retErr error) {
	if orderID == "" || session == "" {
		return nil, fmt.Errorf("CONVEYOR_WORK_ORDER_ID and CONVEYOR_SESSION_ID are required; run from the claimed task session")
	}
	g := c.gitCredentials
	if g == nil {
		return nil, fmt.Errorf("local Git credentials are not initialized")
	}
	// The launcher passes the explicit startup token through child-only askpass.
	if g.token == "" && os.Getenv(gitAskPassModeEnv) == "1" {
		copy := *g
		copy.token = os.Getenv(gitAskPassTokenEnv)
		g = &copy
	}
	token := g.token
	defer func() {
		redactor := g.redactor(token, c.token)
		if retErr != nil {
			clean, _ := redactor.Redact(retErr.Error())
			retErr = fmt.Errorf("%s", clean)
		}
		if result != nil {
			raw, _ := json.Marshal(result)
			clean, _, err := redactor.RedactJSON(raw)
			if err != nil {
				result = nil
				retErr = err
			} else {
				_ = json.Unmarshal(clean, &result)
			}
		}
	}()
	endpoint := "/v1/work-orders/" + url.PathEscape(orderID)
	var template workorder.PullRequestTemplate
	query := "?session_id=" + url.QueryEscape(session) + "&workspace_id=" + url.QueryEscape(c.workspace)
	if err := c.workerDoContext(ctx, http.MethodGet, endpoint+"/pull-request-template"+query, nil, &template, c.token); err != nil {
		return nil, err
	}
	if template.TaskID != taskID || template.Branch == "" || template.Repository == "" || template.RepositoryURL == "" {
		return nil, fmt.Errorf("pull-request template does not match task %s", taskID)
	}
	git := func(args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = directory
		command.Env = isolatedChildEnvironment(g.environment(), map[string]string{"GIT_TERMINAL_PROMPT": "0"})
		out, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	if root == primary {
		return nil, fmt.Errorf("run conveyor submit from the dedicated task worktree resolved by conveyor checkout")
	}
	if _, err = requireSafeWorktree(ctx, root); err != nil {
		return nil, err
	}
	branch, err := git("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	if branch != template.Branch {
		return nil, fmt.Errorf("task branch mismatch: expected %s, observed %s", template.Branch, branch)
	}
	remote, err := git("remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return nil, err
	}
	expectedIdentity, expectedErr := gitx.NormalizeRepositoryIdentity(template.RepositoryURL)
	observedIdentity, observedErr := gitx.NormalizeRepositoryIdentity(remote)
	if strings.Contains(remote, "\n") || expectedErr != nil || observedErr != nil || expectedIdentity != observedIdentity {
		return nil, fmt.Errorf("origin push URL does not match the assigned repository")
	}
	if token != "" && !strings.HasPrefix(remote, "file://") {
		if err = requireHTTPSRemote(remote); err != nil {
			return nil, fmt.Errorf("explicit Git credential requires an HTTPS remote; %s", submitCredentialRemedy)
		}
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if token == "" {
		credentialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		fill := exec.CommandContext(credentialCtx, "git", "credential", "fill")
		fill.Dir = directory
		fill.Env = isolatedChildEnvironment(g.environment(), map[string]string{"GIT_TERMINAL_PROMPT": "0"})
		fill.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
		fill.Stderr = io.Discard
		raw, fillErr := fill.Output()
		if fillErr != nil {
			return nil, fmt.Errorf("resolve GitHub API credential: %s", submitCredentialRemedy)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if value, ok := strings.CutPrefix(line, "password="); ok {
				token = value
			}
		}
		if token == "" {
			return nil, fmt.Errorf("resolve GitHub API credential: %s", submitCredentialRemedy)
		}
	}
	// Push the exact observed commit, even if HEAD moves concurrently.
	if _, err = git("push", "--set-upstream", "origin", head+":refs/heads/"+template.Branch); err != nil {
		return nil, fmt.Errorf("push task branch: %w; %s", err, submitCredentialRemedy)
	}
	// A SHA refspec does not configure local branch tracking by itself.
	if _, err = git("config", "branch."+template.Branch+".remote", "origin"); err != nil {
		return nil, err
	}
	if _, err = git("config", "branch."+template.Branch+".merge", "refs/heads/"+template.Branch); err != nil {
		return nil, err
	}
	if _, err = github.EnsureSubmissionPRWithCredential(ctx, template.Repository, template.Branch, template.Base, template.Title, template.Body, token); err != nil {
		return nil, fmt.Errorf("open task pull request: %w; %s; pushed branch is preserved", err, submitCredentialRemedy)
	}
	body, _ := json.Marshal(map[string]string{"session_id": session, "head_sha": head, "workspace_id": c.workspace})
	err = c.workerDoContext(ctx, http.MethodPost, endpoint+"/submit-for-review", body, &result, c.token)
	return result, err
}
