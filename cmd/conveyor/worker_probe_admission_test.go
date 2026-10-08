package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type workerProbeRefusal struct {
	order string
	err   error
}

// captureWorkerProbeRefusals records every probe-admission refusal; onRefusal,
// when set, runs after each one is recorded.
func captureWorkerProbeRefusals(t *testing.T, onRefusal func(workerProbeRefusal)) func() []workerProbeRefusal {
	t.Helper()
	var mu sync.Mutex
	var refusals []workerProbeRefusal
	previous := reportWorkerProbeRefusal
	reportWorkerProbeRefusal = func(item workerservice.DispatchOrder, err error) {
		refusal := workerProbeRefusal{order: item.Order.ID, err: err}
		mu.Lock()
		refusals = append(refusals, refusal)
		mu.Unlock()
		if onRefusal != nil {
			onRefusal(refusal)
		}
	}
	t.Cleanup(func() { reportWorkerProbeRefusal = previous })
	return func() []workerProbeRefusal {
		mu.Lock()
		defer mu.Unlock()
		return append([]workerProbeRefusal(nil), refusals...)
	}
}

// writeWorkerProbeScript writes a probe that logs each invocation and is
// healthy only while marker exists.
func writeWorkerProbeScript(t *testing.T, directory, log, marker string) string {
	t.Helper()
	path := filepath.Join(directory, "probe")
	script := "#!/bin/sh\nprintf 'probe\\n' >> '" + log + "'\n" +
		"if [ -e '" + marker + "' ]; then printf 'probe ok\\n'; exit 0; fi\nprintf 'harness down\\n'\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeWorkerVerifyConfig writes the example worker setup with a verify route
// so every stage selects local-agent, probed by probe.
func writeWorkerVerifyConfig(t *testing.T, probe string) string {
	t.Helper()
	path := writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{probe})
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	verify := config.ImplementationSettings{Harness: "local-agent", ModelPolicy: "harness_default", TimeoutText: "45m"}
	for index := range cfg.Setups {
		cfg.Setups[index].ExecutionSettings.Verify = verify
	}
	if cfg.ExecutionSettings != nil {
		cfg.ExecutionSettings.Verify = verify
	}
	data, err := config.MarshalWorkspaceDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if route := loaded.Routing.Stages["verify"]; route.Harness != "local-agent" || route.Execution != config.ExecutionMCP {
		t.Fatalf("verify route=%+v", route)
	}
	return path
}

func probeLogLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestWorkerAllStagesRequireHealthyLocalProbe(t *testing.T) {
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		for _, healthy := range []bool{false, true} {
			name := string(stage) + "/unhealthy"
			if healthy {
				name = string(stage) + "/healthy"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("CONVEYOR_WORKER_TOKEN", "worker-credential")
				t.Setenv(localGitTokenEnv, "")
				directory := t.TempDir()
				log, marker := filepath.Join(directory, "probe.log"), filepath.Join(directory, "healthy")
				if healthy {
					if err := os.WriteFile(marker, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				configPath := writeWorkerVerifyConfig(t, writeWorkerProbeScript(t, directory, log, marker))
				refusals := captureWorkerProbeRefusals(t, nil)
				item := workerservice.DispatchOrder{
					Task:       core.Task{ID: "task", Repo: "repo", BaseBranch: "main"},
					Repository: config.Repo{URL: "https://example.test/repo.git"},
					Order:      core.WorkOrder{ID: "order-" + string(stage), Stage: stage, ReviewSeat: 1, Claimable: true, State: core.WorkOrderQueued},
				}
				var mu sync.Mutex
				var claims, probesAtClaim, heartbeats int
				var unhealthyReported bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/v1/worker/heartbeat":
						heartbeats++
						var body struct {
							Probes []core.HarnessProbe `json:"probes"`
						}
						_ = json.NewDecoder(r.Body).Decode(&body)
						for _, probe := range body.Probes {
							if !probe.Healthy {
								unhealthyReported = true
							}
						}
						_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
					case "/v1/worker/work-orders":
						if claims > 0 {
							_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{})
							return
						}
						_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{item})
					case "/v1/worker/work-orders/" + item.Order.ID + "/claim":
						claims++
						probesAtClaim = probeLogLines(t, log)
						http.Error(w, "another claimant won", http.StatusConflict)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				c := &client{base: server.URL, workspace: "demo", gitPreflight: func(context.Context, workerservice.DispatchOrder, []string) error { return nil }}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := runWorkerWithPolicyAndConfig(ctx, c, "", "test", true, defaultWorkerReconnectPolicy, configPath); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				if !healthy {
					if claims != 0 || !unhealthyReported || heartbeats == 0 {
						t.Fatalf("unhealthy probe claims=%d unhealthy heartbeat=%t heartbeats=%d", claims, unhealthyReported, heartbeats)
					}
					got := refusals()
					if len(got) != 1 || got[0].order != item.Order.ID {
						t.Fatalf("refusals=%+v", got)
					}
					for _, want := range []string{`harness "local-agent" failed its local probe`, "harness down", "left queued", "nothing was claimed", localExecutionSetupCommand + " --config " + configPath} {
						if !strings.Contains(got[0].err.Error(), want) {
							t.Fatalf("refusal %v does not name %q", got[0].err, want)
						}
					}
					return
				}
				// The initially loaded setup was validated and probed before the
				// first claim of every stage.
				if claims != 1 || probesAtClaim == 0 || len(refusals()) != 0 || unhealthyReported {
					t.Fatalf("healthy probe claims=%d probes before claim=%d refusals=%+v", claims, probesAtClaim, refusals())
				}
			})
		}
	}
}

func TestWorkerProbeAdmissionRecoversOnlyThroughAHealthyReprobe(t *testing.T) {
	harness := config.Harness{Name: "local-agent", Command: []string{"agent", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"agent", "--version"}}
	document := workerservice.WorkerConfig{WorkspaceDocument: config.WorkspaceDocument{Harnesses: []config.Harness{harness}}}
	healthy := false
	runs := 0
	probes := newWorkerHarnessProbes()
	probes.run = func(_ context.Context, targets []workerservice.HarnessProbeTarget) []core.HarnessProbe {
		runs++
		result := make([]core.HarnessProbe, 0, len(targets))
		for _, target := range targets {
			result = append(result, core.HarnessProbe{Harness: target.Harness.Name, Fingerprint: target.Fingerprint, Healthy: healthy, Message: "probe output"})
		}
		return result
	}
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := probes.admit(harness, start); err == nil || !strings.Contains(err.Error(), "no local probe result") {
		t.Fatalf("missing observation admitted: %v", err)
	}
	probes.probe(t.Context(), document, start)
	if err := probes.admit(harness, start); err == nil || !strings.Contains(err.Error(), "failed its local probe") || !strings.Contains(err.Error(), workerProbeRetryInitial.String()) {
		t.Fatalf("unhealthy observation admitted: %v", err)
	}
	// Recovery inside the unhealthy backoff window is not observed, so it
	// cannot admit; the next due probe can.
	healthy = true
	probes.probe(t.Context(), document, start.Add(workerProbeRetryInitial-time.Second))
	if runs != 1 {
		t.Fatalf("probe ran inside its backoff window: runs=%d", runs)
	}
	if err := probes.admit(harness, start.Add(workerProbeRetryInitial-time.Second)); err == nil {
		t.Fatal("unhealthy observation admitted before its retry")
	}
	recovered := start.Add(workerProbeRetryInitial)
	probes.probe(t.Context(), document, recovered)
	if runs != 2 {
		t.Fatalf("due probe did not run: runs=%d", runs)
	}
	if err := probes.admit(harness, recovered.Add(time.Second)); err != nil {
		t.Fatalf("healthy recovery was refused: %v", err)
	}
}

func TestWorkerProbeFingerprintReloadCannotUseOldSuccess(t *testing.T) {
	t.Run("observation cache", func(t *testing.T) {
		original := config.Harness{Name: "local-agent", Command: []string{"agent", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"agent", "--version"}}
		reloaded := original
		reloaded.ProbeCommand = []string{"agent-v2", "--version"}
		documentFor := func(harness config.Harness) workerservice.WorkerConfig {
			return workerservice.WorkerConfig{WorkspaceDocument: config.WorkspaceDocument{Harnesses: []config.Harness{harness}}}
		}
		health := map[string]bool{}
		probes := newWorkerHarnessProbes()
		probes.run = func(_ context.Context, targets []workerservice.HarnessProbeTarget) []core.HarnessProbe {
			result := make([]core.HarnessProbe, 0, len(targets))
			for _, target := range targets {
				result = append(result, core.HarnessProbe{Harness: target.Harness.Name, Fingerprint: target.Fingerprint, Healthy: health[target.Fingerprint], Message: "probe output"})
			}
			return result
		}
		start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
		health[workerservice.HarnessFingerprint(original)] = true
		probes.probe(t.Context(), documentFor(original), start)
		if err := probes.admit(original, start.Add(time.Second)); err != nil {
			t.Fatalf("current healthy definition refused: %v", err)
		}
		// An edited definition is a different fingerprint: the old success
		// cannot admit it before the edited definition is itself probed.
		if err := probes.admit(reloaded, start.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "no local probe result for its current definition") {
			t.Fatalf("reloaded definition reused the old success: %v", err)
		}
		// A healthy result expires at its scheduled refresh.
		if err := probes.admit(original, start.Add(workerHealthyProbeInterval)); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired success admitted: %v", err)
		}
		// Probing the reloaded setup retires the old definition's success.
		probes.probe(t.Context(), documentFor(reloaded), start.Add(2*time.Second))
		if err := probes.admit(original, start.Add(2*time.Second)); err == nil {
			t.Fatal("retired definition still admits after reload")
		}
		if err := probes.admit(reloaded, start.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), "failed its local probe") {
			t.Fatalf("unhealthy reloaded definition admitted: %v", err)
		}
		health[workerservice.HarnessFingerprint(reloaded)] = true
		retry := start.Add(2*time.Second + workerProbeRetryInitial)
		probes.probe(t.Context(), documentFor(reloaded), retry)
		if err := probes.admit(reloaded, retry); err != nil {
			t.Fatalf("current healthy reloaded definition refused: %v", err)
		}
	})

	t.Run("worker loop", func(t *testing.T) {
		t.Setenv("CONVEYOR_WORKER_TOKEN", "worker-credential")
		t.Setenv(localGitTokenEnv, "")
		directory := t.TempDir()
		healthyMarker := filepath.Join(directory, "healthy")
		if err := os.WriteFile(healthyMarker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		healthyProbe := writeWorkerProbeScript(t, directory, filepath.Join(directory, "probe.log"), healthyMarker)
		configPath := writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{healthyProbe})
		reloadedDirectory := t.TempDir()
		reloadedProbe := writeWorkerProbeScript(t, reloadedDirectory, filepath.Join(reloadedDirectory, "probe.log"), filepath.Join(reloadedDirectory, "absent"))
		reloadedConfig, err := os.ReadFile(writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{reloadedProbe}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		refusals := captureWorkerProbeRefusals(t, func(refusal workerProbeRefusal) {
			if refusal.order == "second" {
				cancel()
			}
		})
		order := func(id string) workerservice.DispatchOrder {
			return workerservice.DispatchOrder{
				Task:       core.Task{ID: id, Repo: "repo", BaseBranch: "main"},
				Repository: config.Repo{URL: "https://example.test/repo.git"},
				Order:      core.WorkOrder{ID: id, Stage: core.StageSpec, Claimable: true, State: core.WorkOrderQueued},
			}
		}
		var mu sync.Mutex
		var listings int
		var claimed []string
		var heartbeatProbes [][]core.HarnessProbe
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.URL.Path == "/v1/worker/heartbeat":
				var body struct {
					Probes []core.HarnessProbe `json:"probes"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				heartbeatProbes = append(heartbeatProbes, body.Probes)
				_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
			case r.URL.Path == "/v1/worker/work-orders":
				listings++
				if listings == 1 {
					// The configuration changes after this iteration loaded
					// and probed it: the launch below keeps the loaded setup.
					if err := os.WriteFile(configPath, reloadedConfig, 0o600); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{order("first")})
					return
				}
				_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{order("second")})
			case strings.HasSuffix(r.URL.Path, "/claim"):
				claimed = append(claimed, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/worker/work-orders/"), "/claim"))
				http.Error(w, "another claimant won", http.StatusConflict)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		c := &client{base: server.URL, workspace: "demo", gitPreflight: func(context.Context, workerservice.DispatchOrder, []string) error { return nil }}
		if err := runWorkerWithPolicyAndConfig(ctx, c, "", "test", false, defaultWorkerReconnectPolicy, configPath); !errors.Is(err, context.Canceled) {
			t.Fatalf("worker err=%v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(claimed) != 1 || claimed[0] != "first" {
			t.Fatalf("claims=%v", claimed)
		}
		got := refusals()
		if len(got) != 1 || got[0].order != "second" || !strings.Contains(got[0].err.Error(), "harness down") || !strings.Contains(got[0].err.Error(), localExecutionSetupCommand) {
			t.Fatalf("refusals=%+v", got)
		}
		loaded, err := config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		reloadedFingerprint := workerservice.HarnessFingerprint(config.Harness{})
		for _, harness := range loaded.Harnesses {
			if harness.Name == "local-agent" {
				reloadedFingerprint = workerservice.HarnessFingerprint(harness)
			}
		}
		last := heartbeatProbes[len(heartbeatProbes)-1]
		if len(heartbeatProbes) < 2 || len(last) != 1 || last[0].Healthy || last[0].Fingerprint != reloadedFingerprint {
			t.Fatalf("reloaded heartbeat probes=%+v", heartbeatProbes)
		}
		if first := heartbeatProbes[0]; len(first) != 1 || !first[0].Healthy || first[0].Fingerprint == last[0].Fingerprint {
			t.Fatalf("initial heartbeat probes=%+v", first)
		}
	})
}
