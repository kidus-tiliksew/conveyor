package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMain answers Git askpass prompts when Git invokes the test binary as
// its askpass helper, as main does for the CLI (component-git-delivery).
func TestMain(m *testing.M) {
	if os.Getenv(gitAskPassModeEnv) == "1" && len(os.Args) == 2 && isGitAskPassPrompt(os.Args[1]) {
		if err := runGitAskPass(os.Stdout, os.Args[1]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestLocalGitCredentialResolutionExplicitAndAmbient(t *testing.T) {
	t.Setenv(localGitTokenEnv, "local-only-secret")
	original := &client{}
	explicit, err := original.withLocalGitCredential()
	if err != nil {
		t.Fatal(err)
	}
	if original.gitCredentials != nil {
		t.Fatal("mutated caller's client")
	}
	environment := explicit.gitCredentials.environment()
	found := 0
	for _, entry := range environment {
		if strings.HasPrefix(entry, localGitTokenEnv+"=") {
			t.Fatal("startup variable reached child")
		}
		if strings.Contains(entry, "local-only-secret") {
			found++
			if entry != gitAskPassTokenEnv+"=local-only-secret" {
				t.Fatal("credential outside askpass")
			}
		}
	}
	if found != 1 {
		t.Fatalf("credential environment occurrences=%d", found)
	}
	t.Setenv(localGitTokenEnv, "")
	same, err := explicit.withLocalGitCredential()
	if err != nil || same.gitCredentials != explicit.gitCredentials {
		t.Fatal("credential changed mid-invocation")
	}
	ambient, err := original.withLocalGitCredential()
	if err != nil {
		t.Fatal(err)
	}
	if len(ambient.gitCredentials.values) != 0 {
		t.Fatal("ambient credential overrides installed")
	}
	expected := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, localGitTokenEnv+"=") {
			expected = append(expected, entry)
		}
	}
	if !reflect.DeepEqual(ambient.gitCredentials.environment(), expected) {
		t.Fatal("ambient environment changed")
	}
}

// The exact token never reaches an upload body or a returned error
// (req-delivery-and-forge AC-7.1; component-git-delivery).
func TestLocalGitCredentialScrubsUploadBytesAndErrors(t *testing.T) {
	const secret = "locally-held-\"secret\nvalue"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Error(err)
		}
		decoded, _ := json.Marshal(value)
		encodedSecret, _ := json.Marshal(secret)
		if bytes.Contains(decoded, encodedSecret[1:len(encodedSecret)-1]) || bytes.Contains(body, []byte(secret)) {
			t.Error("local credential reached upload")
		}
		if !bytes.Contains(body, []byte("[REDACTED:exact]")) {
			t.Error("missing exact scrub")
		}
		if requests.Add(1) == 2 {
			http.Error(w, "refused "+secret, http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := &client{base: server.URL, gitCredentials: &localGitCredential{token: secret}}
	body, _ := json.Marshal(map[string]any{"evidence": map[string]string{"content": secret}})
	if err := c.workerDoContext(t.Context(), http.MethodPost, "/verification-evidence", body, nil, "credential"); err != nil {
		t.Fatal(err)
	}
	err := c.workerDoContext(t.Context(), http.MethodPost, "/v1/tasks/task/worktree-cleanup", body, nil, "credential")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED:exact]") {
		t.Fatalf("refusal error = %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests=%d", requests.Load())
	}
}

func TestIsolatedChildEnvironmentReplacesLaunchIdentity(t *testing.T) {
	childValues := map[string]string{
		"CONVEYOR_ADDR":                    "endpoint",
		"CONVEYOR_API_TOKEN":               "fresh-token",
		"CONVEYOR_CLIENT_TOKEN":            "fresh-client",
		"CONVEYOR_CURRENT_ATTEMPT_ID":      "attempt-current",
		"CONVEYOR_PREVIOUS_ATTEMPT_ID":     "attempt-previous",
		"CONVEYOR_PREVIOUS_ATTEMPT_REASON": "previous harness exit",
		"CONVEYOR_PREDECESSOR":             `{"task_id":"task-1","attempt_id":"attempt-previous"}`,
		"CONVEYOR_PREVIOUS_WORK_ORDER_ID":  "task-1-implement-0",
		"CONVEYOR_SESSION_ID":              "session-current",
		"CONVEYOR_TASK_BASE_BRANCH":        "main",
		"CONVEYOR_TASK_BRANCH":             "conveyor/task-1",
		"CONVEYOR_TASK_ID":                 "task-1",
		"CONVEYOR_TASK_REPO":               "conveyor",
		"CONVEYOR_TASK_REPO_URL":           "https://github.com/kidus-tiliksew/conveyor.git",
		"CONVEYOR_WORKSPACE":               "demo",
		"CONVEYOR_WORKTREE_ROOT":           "/var/lib/conveyor/worktrees",
		"CONVEYOR_WORK_ORDER_ID":           "task-1-implement-1",
		"CONVEYOR_WRITER_GENERATION":       "session-current",
		"CONVEYOR_WRITER_PATH":             "/var/lib/conveyor/writers/task-1.json",
		"GIT_AUTHOR_NAME":                  "Executing User",
		"GIT_AUTHOR_EMAIL":                 "executor@example.test",
		"GIT_COMMITTER_NAME":               "Executing User",
		"GIT_COMMITTER_EMAIL":              "executor@example.test",
	}
	base := []string{
		"PATH=/bin", "GOCACHE=/cache/go", "CONVEYOR_TEST_DATABASE_URL=postgres://fixture",
		localGitTokenEnv + "=parent-forge-token",
		"GIT_AUTHOR_NAME=Host", "GIT_AUTHOR_EMAIL=host@example.test",
		"GIT_COMMITTER_NAME=Host", "GIT_COMMITTER_EMAIL=host@example.test",
	}
	for name := range childValues {
		base = append(base, name+"=stale-"+strings.ToLower(name))
	}
	env := isolatedChildEnvironment(base, childValues)
	for name, want := range childValues {
		if got := environmentValue(env, name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}
	for name, want := range map[string]string{
		"PATH":                       "/bin",
		"GOCACHE":                    "/cache/go",
		"CONVEYOR_TEST_DATABASE_URL": "postgres://fixture",
	} {
		if got := environmentValue(env, name); got != want {
			t.Fatalf("preserved %s=%q want=%q", name, got, want)
		}
	}
	if got := environmentValue(env, localGitTokenEnv); got != "" {
		t.Fatalf("parent forge token escaped into child environment: %q", got)
	}
	other := isolatedChildEnvironment(env, map[string]string{
		"CONVEYOR_API_TOKEN": "other-token", "CONVEYOR_ADDR": "other-endpoint", "CONVEYOR_WORKSPACE": "demo",
		"CONVEYOR_WORK_ORDER_ID": "other-order", "CONVEYOR_SESSION_ID": "other-session", "CONVEYOR_CLIENT_TOKEN": "other-client",
	})
	if environmentValue(env, "CONVEYOR_SESSION_ID") != "session-current" || environmentValue(other, "CONVEYOR_SESSION_ID") != "other-session" || environmentValue(other, "CONVEYOR_CLIENT_TOKEN") != "other-client" {
		t.Fatalf("concurrent child environments shared launch identity")
	}
}

func TestChildGitCredentialEnvironmentUsesAskPassWithoutPersistentSecrets(t *testing.T) {
	const token = "forge-token-only-in-environment"
	environment, err := childGitCredentialEnvironment(token)
	if err != nil {
		t.Fatal(err)
	}
	if environment[gitAskPassTokenEnv] != token || environment[gitAskPassModeEnv] != "1" || environment["GIT_TERMINAL_PROMPT"] != "0" || environment["GIT_CONFIG_VALUE_0"] != "" {
		t.Fatalf("Git environment=%v", environment)
	}
	for name, value := range environment {
		if name != gitAskPassTokenEnv && strings.Contains(value, token) {
			t.Fatalf("token escaped its dedicated environment value through %s", name)
		}
	}
	t.Setenv(gitAskPassUsernameEnv, environment[gitAskPassUsernameEnv])
	t.Setenv(gitAskPassTokenEnv, environment[gitAskPassTokenEnv])
	var output bytes.Buffer
	if err = runGitAskPass(&output, "Username for 'https://example.test':"); err != nil || strings.TrimSpace(output.String()) != "x-access-token" {
		t.Fatalf("username response=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err = runGitAskPass(&output, "Password for 'https://x-access-token@example.test':"); err != nil || strings.TrimSpace(output.String()) != token {
		t.Fatalf("password response=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err = runGitAskPass(&output, "OTP:"); err == nil || output.Len() != 0 {
		t.Fatalf("unsupported prompt response=%q err=%v", output.String(), err)
	}
	if err = requireHTTPSRemote("https://github.com/example/repository.git"); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"git@github.com:example/repository.git", "ssh://git@github.com/example/repository.git", "https://token@github.com/example/repository.git"} {
		if err = requireHTTPSRemote(remote); err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Fatalf("remote %q error=%v", remote, err)
		}
	}
}

func TestLocalGitCredentialPushesWithExplicitAndAmbientCredentials(t *testing.T) {
	for _, mode := range []string{"explicit", "ambient"} {
		t.Run(mode, func(t *testing.T) {
			const token = "forge-token-used-only-by-askpass"
			serverRoot := t.TempDir()
			bare := filepath.Join(serverRoot, "repository.git")
			if output, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
				t.Fatalf("init bare repository: %v\n%s", err, output)
			}
			if output, err := exec.Command("git", "--git-dir", bare, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
				t.Fatalf("enable HTTP receive-pack: %v\n%s", err, output)
			}
			authenticatedRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "x-access-token" || password != token {
					w.Header().Set("WWW-Authenticate", `Basic realm="conveyor-test"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				authenticatedRequests++
				command := exec.Command("git", "http-backend")
				command.Env = append(os.Environ(),
					"GIT_PROJECT_ROOT="+serverRoot,
					"GIT_HTTP_EXPORT_ALL=1",
					"REQUEST_METHOD="+r.Method,
					"PATH_INFO="+r.URL.Path,
					"QUERY_STRING="+r.URL.RawQuery,
					"CONTENT_TYPE="+r.Header.Get("Content-Type"),
					"CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10),
					"REMOTE_USER=x-access-token",
				)
				command.Stdin = r.Body
				var response, stderr bytes.Buffer
				command.Stdout, command.Stderr = &response, &stderr
				if err := command.Run(); err != nil {
					http.Error(w, "Git backend failed", http.StatusInternalServerError)
					return
				}
				headerBlock, body, ok := bytes.Cut(response.Bytes(), []byte("\r\n\r\n"))
				if !ok {
					http.Error(w, "Git backend returned malformed CGI output", http.StatusInternalServerError)
					return
				}
				status := http.StatusOK
				for _, line := range strings.Split(string(headerBlock), "\r\n") {
					name, value, found := strings.Cut(line, ":")
					if !found {
						continue
					}
					value = strings.TrimSpace(value)
					if strings.EqualFold(name, "Status") {
						if code, parseErr := strconv.Atoi(strings.Fields(value)[0]); parseErr == nil {
							status = code
						}
						continue
					}
					w.Header().Add(name, value)
				}
				w.WriteHeader(status)
				_, _ = w.Write(body)
			}))
			defer server.Close()

			checkout := t.TempDir()
			if output, err := exec.Command("git", "init", checkout).CombinedOutput(); err != nil {
				t.Fatalf("init checkout: %v\n%s", err, output)
			}
			if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("credential fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"-C", checkout, "add", "README.md"}, {"-C", checkout, "-c", "user.name=Worker", "-c", "user.email=worker@example.test", "commit", "-m", "fixture"}} {
				if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
			}
			hostHome := t.TempDir()
			t.Setenv("HOME", hostHome)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(hostHome, ".gitconfig"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_TERMINAL_PROMPT", "0")
			t.Setenv(localGitTokenEnv, "")
			if mode == "explicit" {
				t.Setenv(localGitTokenEnv, token)
			} else {
				t.Setenv("CONVEYOR_TEST_AMBIENT_TOKEN", token)
				helper := `!f() { printf '%s\n' 'username=x-access-token' "password=$CONVEYOR_TEST_AMBIENT_TOKEN"; }; f`
				mustGit(t, "", "config", "--global", "credential.helper", helper)
			}
			c, err := (&client{}).withLocalGitCredential()
			if err != nil {
				t.Fatal(err)
			}
			push := exec.Command("git", "-C", checkout, "push", server.URL+"/repository.git", "HEAD:refs/heads/task")
			push.Env = c.gitCredentials.environment()
			push.Env = isolatedChildEnvironment(push.Env, map[string]string{"HOME": hostHome})
			output, err := push.CombinedOutput()
			if err != nil {
				t.Fatalf("credentialed push: %v\n%s", err, output)
			}
			if bytes.Contains(output, []byte(token)) || authenticatedRequests == 0 {
				t.Fatalf("unsafe push output=%q authenticated_requests=%d", output, authenticatedRequests)
			}
			if output, err = exec.Command("git", "--git-dir", bare, "rev-parse", "refs/heads/task").CombinedOutput(); err != nil {
				t.Fatalf("pushed branch is missing: %v\n%s", err, output)
			}
			for _, root := range []string{checkout, serverRoot, hostHome} {
				if err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil || entry.IsDir() {
						return walkErr
					}
					data, readErr := os.ReadFile(path)
					if readErr != nil {
						return readErr
					}
					if bytes.Contains(data, []byte(token)) {
						return fmt.Errorf("forge token persisted in %s", path)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// environmentValue returns the last value of name in env.
func environmentValue(env []string, name string) string {
	prefix := name + "="
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}
