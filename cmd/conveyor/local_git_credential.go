package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

const localGitTokenEnv = "CONVEYOR_GIT_TOKEN"
const localGitPreflightTimeout = 20 * time.Second

// localGitCredential is invocation-local, used by conveyor submit and the Git
// commands of conveyor checkout. It resolves on the executing machine for
// committing, pushing, and opening the pull request; no stored forge
// credential or control-plane installation token is forwarded to it
// (req-delivery-and-forge REQ-7, AC-7.1 through AC-7.3, DEC-59(3)-(4);
// component-git-delivery).
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
	values := append(append([]string(nil), additional...), g.token)
	for _, value := range append([]string(nil), values...) {
		encoded, _ := json.Marshal(value)
		values = append(values, string(encoded[1:len(encoded)-1]))
	}
	return redact.New(values)
}

func (g *localGitCredential) scrubJSON(body []byte) ([]byte, error) {
	if g.token == "" {
		return body, nil
	}
	clean, _, err := g.redactor().RedactJSON(body)
	return clean, err
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

// isolatedChildEnvironment builds a Git child's environment: the startup
// CONVEYOR_GIT_TOKEN never propagates, and each named value replaces any
// inherited entry (component-git-delivery).
func isolatedChildEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if name == localGitTokenEnv {
			continue
		}
		if found {
			if _, replaced := values[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

const (
	gitAskPassModeEnv     = "CONVEYOR_GIT_ASKPASS_MODE"
	gitAskPassTokenEnv    = "CONVEYOR_GIT_ASKPASS_TOKEN"
	gitAskPassUsernameEnv = "CONVEYOR_GIT_ASKPASS_USERNAME"
)

// childGitCredentialEnvironment keeps the forge token in process memory and
// the child-only environment. Git invokes this same executable as askpass;
// neither the token nor a token-bearing URL enters argv or Git configuration
// (req-delivery-and-forge REQ-7/AC-7.2; req-security-boundaries REQ-2).
func childGitCredentialEnvironment(token string) (map[string]string, error) {
	if token == "" {
		return nil, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Conveyor executable for Git askpass: %w", err)
	}
	return map[string]string{
		"GIT_ASKPASS":         executable,
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "never",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "credential.helper",
		"GIT_CONFIG_VALUE_0":  "",
		gitAskPassModeEnv:     "1",
		gitAskPassTokenEnv:    token,
		gitAskPassUsernameEnv: "x-access-token",
	}, nil
}

func runGitAskPass(output io.Writer, prompt string) error {
	lowerPrompt := strings.ToLower(strings.TrimSpace(prompt))
	switch {
	case strings.HasPrefix(lowerPrompt, "username for "):
		username := os.Getenv(gitAskPassUsernameEnv)
		if username == "" {
			return fmt.Errorf("Git askpass username is unavailable")
		}
		_, err := fmt.Fprintln(output, username)
		return err
	case strings.HasPrefix(lowerPrompt, "password for "):
		token := os.Getenv(gitAskPassTokenEnv)
		if token == "" {
			return fmt.Errorf("Git askpass token is unavailable")
		}
		_, err := fmt.Fprintln(output, token)
		return err
	default:
		return fmt.Errorf("Git askpass received an unsupported prompt")
	}
}

func isGitAskPassPrompt(prompt string) bool {
	lowerPrompt := strings.ToLower(strings.TrimSpace(prompt))
	return strings.HasPrefix(lowerPrompt, "username for ") || strings.HasPrefix(lowerPrompt, "password for ")
}

func requireHTTPSRemote(remote string) error {
	parsed, err := url.Parse(strings.TrimSpace(remote))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("repository origin must use an HTTPS URL without embedded credentials; configure origin with an https:// remote")
	}
	return nil
}
