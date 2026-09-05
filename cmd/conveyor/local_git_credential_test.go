package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

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

func TestLocalGitScrubberHandlesEveryChunkAndNewlineBoundary(t *testing.T) {
	for _, token := range []string{"locally-held-secret", "local\nsecret", "quote\"\\secret"} {
		input := "before " + token + " after\n" + token
		for split := 0; split <= len(input); split++ {
			g := &localGitCredential{token: token}
			var out bytes.Buffer
			writer := g.outputWriter(&out, g.redactor())
			if _, err := writer.Write([]byte(input[:split])); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(input[split:])); err != nil {
				t.Fatal(err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), token) || !strings.Contains(out.String(), "[REDACTED:exact]") {
				t.Fatalf("unsafe split %d", split)
			}
		}
	}
}

func TestLocalGitCredentialScrubsWorkerAndRunUploadBytes(t *testing.T) {
	const secret = "locally-held-\"secret\nvalue"
	for _, dispatch := range []string{"worker", "run"} {
		t.Run(dispatch, func(t *testing.T) {
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
				requests.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			c := &client{base: server.URL, gitCredentials: &localGitCredential{token: secret}}
			item := workerservice.DispatchOrder{Dispatch: dispatch, Task: core.Task{ID: "task"}, Order: core.WorkOrder{ID: "order"}}
			ctx := t.Context()
			_, err := c.renewDispatchOrderContext(ctx, "credential", item, "session", &core.WorkOrderActivitySnapshotInput{Content: secret})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: "session", TerminationReason: secret}
			if err := c.checkpointDispatchOrderAttemptContext(ctx, "credential", item, checkpoint, &core.WorkOrderAttemptTranscript{Content: secret}); err != nil {
				t.Fatal(err)
			}
			if err := c.releaseDispatchOrderContext(ctx, "credential", item, core.WorkOrderRelease{SessionID: "session", Reason: secret, FailureDetail: secret}); err != nil {
				t.Fatal(err)
			}
			// Evidence bodies use the same serialized upload boundary.
			body, _ := json.Marshal(map[string]any{"evidence": map[string]string{"content": secret}})
			if err := c.workerDoContext(ctx, http.MethodPost, "/verification-evidence", body, nil, "credential"); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 4 {
				t.Fatalf("requests=%d", requests.Load())
			}
		})
	}
}

func TestLocalGitPreflightUsesEnvironmentAndBoundedContext(t *testing.T) {
	t.Setenv(localGitTokenEnv, "local-secret")
	c, err := (&client{}).withLocalGitCredential()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	c.gitPreflight = func(ctx context.Context, item workerservice.DispatchOrder, env []string) error {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > localGitPreflightTimeout {
			t.Error("unbounded preflight")
		}
		if !reflect.DeepEqual(env, isolatedChildEnvironment(c.gitCredentials.environment(), map[string]string{"GIT_TERMINAL_PROMPT": "0"})) {
			t.Error("preflight environment differs from child")
		}
		return errors.New("unusable local-secret")
	}
	item := workerservice.DispatchOrder{Task: core.Task{Repo: "repo", BaseBranch: "main"}, Repository: config.Repo{URL: "https://example.test/repo.git"}}
	err = c.preflightLocalGitCredential(t.Context(), item)
	if !called || err == nil || strings.Contains(err.Error(), "local-secret") || !strings.Contains(err.Error(), localGitCredentialRemedy) {
		t.Fatalf("preflight error=%v", err)
	}
}

func TestLocalGitPreflightRunsRealGitAndRefusesBeforeWorkerOrRunClaim(t *testing.T) {
	t.Setenv(localGitTokenEnv, "")
	t.Setenv("CONVEYOR_WORKER_TOKEN", "enrolled-worker")
	configPath := writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{"true"})
	missing := filepath.Join(t.TempDir(), "unreachable.git")
	item := workerservice.DispatchOrder{Task: core.Task{ID: "task", Repo: "repo", BaseBranch: "main"}, Repository: config.Repo{Name: "repo", URL: missing}, Order: core.WorkOrder{ID: "order", Stage: core.StageSpec, Claimable: true, State: core.WorkOrderQueued}}
	var claims atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/worker/heartbeat":
			_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
		case "/v1/worker/work-orders":
			_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{item})
		case "/v1/tasks/task/run-order":
			_ = json.NewEncoder(w).Encode(item)
		default:
			claims.Add(1)
			http.Error(w, "claim should not be attempted", 500)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo", token: "user", forgeTokenPreflight: func(context.Context, string) error { return nil }}
	var output bytes.Buffer
	err := runTask(t.Context(), c, "task", configPath, strings.NewReader(""), &output, true, false)
	if err == nil || !strings.Contains(err.Error(), localGitCredentialRemedy) {
		t.Fatalf("run refusal=%v", err)
	}
	if err := runWorkerWithPolicyAndConfig(t.Context(), c, "", "test", true, defaultWorkerReconnectPolicy, configPath); err != nil {
		t.Fatal(err)
	}
	if claims.Load() != 0 || item.Order.State != core.WorkOrderQueued {
		t.Fatalf("preflight attempted %d claims", claims.Load())
	}
}

func TestWorkerLocalGitPreflightCachesFailedRepositories(t *testing.T) {
	t.Setenv(localGitTokenEnv, "")
	t.Setenv("CONVEYOR_WORKER_TOKEN", "enrolled-worker")
	configPath := writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{"true"})
	orders := []workerservice.DispatchOrder{}
	for _, id := range []string{"one", "two", "three"} {
		remote := "https://example.test/first.git"
		if id == "three" {
			remote = "https://example.test/second.git"
		}
		orders = append(orders, workerservice.DispatchOrder{Task: core.Task{ID: id, Repo: "repo"}, Repository: config.Repo{URL: remote}, Order: core.WorkOrder{ID: id, Stage: core.StageSpec, Claimable: true}})
	}
	var claims atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/worker/heartbeat":
			_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
		case "/v1/worker/work-orders":
			_ = json.NewEncoder(w).Encode(orders)
		default:
			claims.Add(1)
			http.Error(w, "unexpected claim", 500)
		}
	}))
	defer server.Close()
	checked := map[string]int{}
	c := &client{base: server.URL, workspace: "demo", gitPreflight: func(_ context.Context, item workerservice.DispatchOrder, _ []string) error {
		checked[item.Repository.URL]++
		return errors.New("no access")
	}}
	if err := runWorkerWithPolicyAndConfig(t.Context(), c, "", "test", true, defaultWorkerReconnectPolicy, configPath); err != nil {
		t.Fatal(err)
	}
	if claims.Load() != 0 || len(checked) != 2 {
		t.Fatalf("claims=%d checks=%v", claims.Load(), checked)
	}
	for _, n := range checked {
		if n != 1 {
			t.Fatalf("cache missed: %v", checked)
		}
	}
}

func TestWorkerLocalGitPreflightCachesSuccessfulRepositoryBeforeClaims(t *testing.T) {
	t.Setenv(localGitTokenEnv, "")
	t.Setenv("CONVEYOR_WORKER_TOKEN", "enrolled-worker")
	configPath := writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{"true"})
	var checked, claims atomic.Int32
	var specClaimed, reviewClaimed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/worker/heartbeat":
			_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
		case "/v1/worker/work-orders":
			orders := []workerservice.DispatchOrder{}
			for _, stage := range []core.Stage{core.StageSpec, core.StageReview} {
				if (stage == core.StageSpec && specClaimed.Load()) || (stage == core.StageReview && reviewClaimed.Load()) {
					continue
				}
				orders = append(orders, workerservice.DispatchOrder{Task: core.Task{ID: string(stage), Repo: "repo"}, Repository: config.Repo{URL: "https://example.test/repo.git"}, Order: core.WorkOrder{ID: string(stage), Stage: stage, ReviewSeat: 1, Claimable: true}})
			}
			_ = json.NewEncoder(w).Encode(orders)
		default:
			if strings.HasSuffix(r.URL.Path, "/claim") {
				if checked.Load() != 1 {
					t.Error("claimed before successful preflight")
				}
				claims.Add(1)
				if strings.Contains(r.URL.Path, "/spec/") {
					specClaimed.Store(true)
				} else {
					reviewClaimed.Store(true)
				}
				http.Error(w, "another claimant won", http.StatusConflict)
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo", gitPreflight: func(context.Context, workerservice.DispatchOrder, []string) error { checked.Add(1); return nil }}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := runWorkerWithPolicyAndConfig(ctx, c, "", "test", true, defaultWorkerReconnectPolicy, configPath); err != nil {
		t.Fatal(err)
	}
	if checked.Load() != 1 || claims.Load() != 2 {
		t.Fatalf("checks=%d claims=%d", checked.Load(), claims.Load())
	}
}

func TestLocalGitPreflightCommandAndCancellation(t *testing.T) {
	t.Setenv(localGitTokenEnv, "")
	directory := t.TempDir()
	script := `#!/bin/sh
[ "$1" = ls-remote ] && [ "$2" = --heads ] && [ "$3" = https://example.test/repo.git ] && [ "$4" = release ] || exit 31
[ "$GIT_TERMINAL_PROMPT" = 0 ] || exit 32
if [ "$CONVEYOR_PREFLIGHT_SLEEP" = 1 ]; then sleep 30 & wait; fi
`
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	c, err := (&client{}).withLocalGitCredential()
	if err != nil {
		t.Fatal(err)
	}
	item := workerservice.DispatchOrder{Task: core.Task{Repo: "repo", BaseBranch: "release"}, Repository: config.Repo{URL: "https://example.test/repo.git", Base: "main"}, Order: core.WorkOrder{Stage: core.StageSpec}}
	if err := c.preflightLocalGitCredential(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONVEYOR_PREFLIGHT_SLEEP", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = c.preflightLocalGitCredential(ctx, item)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestLocalGitScrubberRemovesJSONEscapedHarnessOutput(t *testing.T) {
	const token = "quoted-\"local\nsecret"
	g := &localGitCredential{token: token}
	event, err := json.Marshal(map[string]string{"message": token})
	if err != nil {
		t.Fatal(err)
	}
	for split := 0; split <= len(event); split++ {
		var output bytes.Buffer
		writer := g.outputWriter(&output, g.redactor())
		_, _ = writer.Write(event[:split])
		_, _ = writer.Write(event[split:])
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]string
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(decoded["message"], token) || decoded["message"] != "[REDACTED:exact]" {
			t.Fatalf("unsafe decoded event at split %d", split)
		}
	}
}
