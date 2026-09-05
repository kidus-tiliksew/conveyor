package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

const localGitTokenEnv = "CONVEYOR_GIT_TOKEN"
const localGitPreflightTimeout = 20 * time.Second
const localGitCredentialRemedy = "set CONVEYOR_GIT_TOKEN, or configure the host's git credential for that host"
const localGitCredentialHelp = "Git credentials resolve locally: set CONVEYOR_GIT_TOKEN in the startup environment for child-only askpass, or leave it unset to use the host's Git credentials. The token is never accepted as an argument or saved. Before claiming, Git checks repository access with terminal prompting disabled and a bounded timeout. Repository checks are cached for this invocation; restart after changing credentials. Stored account forge-token presence is still required for control-plane operations."

// localGitCredential is invocation-local, shared by run and worker. No stored
// forge credential crosses the control plane (req-260821-830dbf REQ-6,
// AC-6.1 through AC-6.3, DEC-33; component-work-orders).
type localGitCredential struct {
	token  string
	values map[string]string
}

func (c *client) withLocalGitCredential() (*client, error) {
	if c.gitCredentials != nil {
		return c, nil
	}
	token := os.Getenv(localGitTokenEnv)
	values, err := childGitCredentialEnvironment(token)
	if err != nil {
		return nil, err
	}
	copy := *c
	copy.gitCredentials = &localGitCredential{token: token, values: values}
	return &copy, nil
}

func (g *localGitCredential) environment() []string {
	// The startup variable never propagates to a child. Only self-askpass holds
	// the explicit value; ambient Git configuration remains intact when unset.
	return isolatedChildEnvironment(os.Environ(), g.values)
}

func (c *client) preflightLocalGitCredential(ctx context.Context, item workerservice.DispatchOrder) error {
	g := c.gitCredentials
	environment := isolatedChildEnvironment(g.environment(), map[string]string{"GIT_TERMINAL_PROMPT": "0"})
	checkCtx, cancel := context.WithTimeout(ctx, localGitPreflightTimeout)
	defer cancel()
	var err error
	switch {
	case item.Repository.URL == "":
		err = fmt.Errorf("repository URL is missing")
	case g.token != "":
		err = requireHTTPSRemote(item.Repository.URL)
	}
	if err == nil {
		if c.gitPreflight != nil {
			err = c.gitPreflight(checkCtx, item, environment)
		} else {
			base := item.Task.BaseBranch
			if base == "" {
				base = item.Repository.Base
			}
			if base == "" {
				base = "main"
			}
			command := exec.CommandContext(checkCtx, "git", "ls-remote", "--heads", item.Repository.URL, base)
			command.Env = environment
			command.Dir = os.TempDir()
			if item.Order.Stage != "spec" {
				directory, resolveErr := resolveHarnessWorkingDirectory(checkCtx, localExecutionConfigFromContext(ctx), item)
				if resolveErr != nil {
					return fmt.Errorf("local Git credential preflight failed: %s; %s; work order left queued", g.scrub(resolveErr.Error()), localGitCredentialRemedy)
				}
				command.Dir = directory
			}
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			command.Cancel = func() error {
				err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				if errors.Is(err, syscall.ESRCH) {
					return os.ErrProcessDone
				}
				return err
			}
			command.WaitDelay = time.Second
			// Git output may include credentials. Only the exit category is reported.
			command.Stdout, command.Stderr = io.Discard, io.Discard
			err = command.Run()
			if checkCtx.Err() != nil {
				err = checkCtx.Err()
			}
		}
	}
	if err != nil {
		return fmt.Errorf("local Git credential preflight failed for repository %s: %s; %s; work order left queued", g.scrub(item.Task.Repo), g.scrub(err.Error()), localGitCredentialRemedy)
	}
	return nil
}

// scrub is the exact-match client boundary, also used before JSON encoding and
// output fanout. Redaction precedes truncation so tails cannot retain a token.
func (g *localGitCredential) scrub(value string) string {
	if g == nil || g.token == "" {
		return value
	}
	return strings.ReplaceAll(value, g.token, "[REDACTED:exact]")
}

func (g *localGitCredential) redactor(additional ...string) *redact.Redactor {
	// Harness event streams JSON-escape string values before the launcher sees
	// them. Match that representation before a renderer decodes it for the TUI.
	encoded, _ := json.Marshal(g.token)
	return redact.New(append(additional, g.token, string(encoded[1:len(encoded)-1])))
}

func (g *localGitCredential) scrubJSON(body []byte) ([]byte, error) {
	if g.token == "" {
		return body, nil
	}
	clean, _, err := g.redactor().RedactJSON(body)
	return clean, err
}

// localGitOutputWriter holds a possible token prefix across writes, including
// newlines. The existing redactor then removes the other session credentials.
// Nothing unsanitized reaches the transcript spool, activity tail, or TUI.
type localGitOutputWriter struct {
	credential  *localGitCredential
	destination *redact.Writer
	mu          sync.Mutex
	pending     string
}

func (g *localGitCredential) outputWriter(destination io.Writer, redactor *redact.Redactor) *localGitOutputWriter {
	return &localGitOutputWriter{credential: g, destination: &redact.Writer{Destination: destination, Redactor: redactor}}
}

func (w *localGitOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += string(p)
	token := w.credential.token
	// Replace complete matches before retaining an incomplete suffix.
	w.pending = w.credential.scrub(w.pending)
	keep := 0
	for n := 1; n < len(token) && n <= len(w.pending); n++ {
		if strings.HasSuffix(w.pending, token[:n]) {
			keep = n
		}
	}
	ready := w.pending[:len(w.pending)-keep]
	w.pending = w.pending[len(w.pending)-keep:]
	if _, err := io.WriteString(w.destination, ready); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *localGitOutputWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := io.WriteString(w.destination, w.credential.scrub(w.pending)); err != nil {
		return err
	}
	w.pending = ""
	return w.destination.Flush()
}

// Preserve error identity for lease/cancellation handling while removing local
// secrets from diagnostics that a caller may print or report.
type localGitError struct {
	cause   error
	message string
}

func (e *localGitError) Error() string { return e.message }
func (e *localGitError) Unwrap() error { return e.cause }
func (g *localGitCredential) scrubError(err error) error {
	if err == nil {
		return nil
	}
	clean := g.scrub(err.Error())
	if clean == err.Error() {
		return err
	}
	return &localGitError{cause: err, message: clean}
}
