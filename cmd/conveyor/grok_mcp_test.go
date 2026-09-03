package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestValidateGrokConfigSourceRequiresExactEnvironmentTemplates(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"valid", `[mcp_servers.conveyor]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
`, ""},
		{"literal URL", `[mcp_servers.conveyor]
url = "https://example.test/mcp"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
`, "environment-backed URL"},
		{"literal token", `[mcp_servers.conveyor]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer persisted-token" }
`, "environment-backed authorization"},
		{"wrong server", `[mcp_servers.other]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
`, "environment-backed URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateGrokConfigSource(path, "conveyor")
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGrokEnvironmentReadinessRejectsCompatSourceAndUnhealthyDoctor(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(`[mcp_servers.conveyor]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := grokTestHarness("grok")
	env := grokTestEnvironment("http://127.0.0.1:9999/mcp")
	if err := validateGrokEnvironmentAttachmentWithRunner(t.Context(), harness, []string{"CONVEYOR_ADDR=http://127.0.0.1:9999/mcp"}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "complete child launch identity") {
		t.Fatalf("missing child identity error=%v", err)
	}
	runner := func(sourceType string, healthy bool) grokJSONRunner {
		return func(_ context.Context, _ string, _ []string, _ string, args []string, target any) error {
			switch args[0] {
			case "inspect":
				payload := map[string]any{"mcpServers": []any{map[string]any{"name": "conveyor", "transport": "http", "target": "http://127.0.0.1:9999/mcp", "source": map[string]any{"type": sourceType, "path": configPath}}}}
				data, _ := json.Marshal(payload)
				return json.Unmarshal(data, target)
			case "mcp":
				payload := map[string]any{"healthy_count": 0, "failing_count": 1, "servers": []any{map[string]any{"name": "conveyor", "transport": "http", "target": "http://127.0.0.1:9999/mcp", "source": "config", "healthy": healthy}}}
				if healthy {
					payload["healthy_count"], payload["failing_count"] = 1, 0
				}
				data, _ := json.Marshal(payload)
				return json.Unmarshal(data, target)
			default:
				t.Fatalf("unexpected Grok command: %v", args)
				return nil
			}
		}
	}
	if err := validateGrokEnvironmentAttachmentWithRunner(t.Context(), harness, env, t.TempDir(), runner("claudeCompat", true)); err == nil || !strings.Contains(err.Error(), "intended") {
		t.Fatalf("compat source error=%v", err)
	}
	missing := func(_ context.Context, _ string, _ []string, _ string, _ []string, target any) error {
		data, _ := json.Marshal(map[string]any{"mcpServers": []any{}})
		return json.Unmarshal(data, target)
	}
	if err := validateGrokEnvironmentAttachmentWithRunner(t.Context(), harness, env, t.TempDir(), missing); err == nil || !strings.Contains(err.Error(), "intended") {
		t.Fatalf("missing registration error=%v", err)
	}
	if err := validateGrokEnvironmentAttachmentWithRunner(t.Context(), harness, env, t.TempDir(), runner("configToml", false)); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("unhealthy doctor error=%v", err)
	}
	if err := validateGrokEnvironmentAttachmentWithRunner(t.Context(), harness, env, t.TempDir(), runner("configToml", true)); err != nil {
		t.Fatalf("healthy readiness: %v", err)
	}
}

func TestGrokEnvironmentReadinessUsesInstalledNoModelTurnDoctor(t *testing.T) {
	grok, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("grok is not installed; skipping real no-model-turn readiness coverage")
	}
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err = os.WriteFile(configPath, []byte(`[mcp_servers.conveyor]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", home)
	serverConfig := httpapi.NewServer(store.NewMemory())
	serverConfig.BearerToken = "worker-test-token"
	server := httptest.NewServer(serverConfig.Handler())
	defer server.Close()

	harness := grokTestHarness(grok)
	harness.ProbeTimeout = 30 * time.Second
	if err = validateGrokEnvironmentAttachment(t.Context(), harness, grokTestEnvironment(server.URL+"/mcp"), t.TempDir()); err != nil {
		t.Fatalf("installed Grok readiness: %v", err)
	}
}

func TestCursorEnvironmentReadinessRequiresLifecycleTools(t *testing.T) {
	harness := config.HarnessTemplates()[3].Harness
	env := grokTestEnvironment("http://127.0.0.1:9999/mcp")
	directory := t.TempDir()
	ready := strings.Join(cursorLifecycleTools, "\n")
	tests := []struct {
		name    string
		output  string
		runErr  error
		wantErr bool
	}{
		{name: "ready listing", output: ready},
		{name: "missing server", output: "No MCP server named conveyor", wantErr: true},
		{name: "non-zero exit", output: ready, runErr: errors.New("exit status 1"), wantErr: true},
		{name: "incomplete listing", output: strings.Join(cursorLifecycleTools[:len(cursorLifecycleTools)-1], "\n"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := func(_ context.Context, gotDirectory string, gotEnv []string, binary string, args []string) ([]byte, error) {
				if gotDirectory != directory || strings.Join(gotEnv, "\x00") != strings.Join(env, "\x00") {
					t.Fatalf("directory=%q env=%v", gotDirectory, gotEnv)
				}
				if binary != "cursor-agent" || strings.Join(args, " ") != "mcp list-tools conveyor" {
					t.Fatalf("binary=%q args=%v", binary, args)
				}
				return []byte(test.output), test.runErr
			}
			err := validateCursorEnvironmentAttachmentWithRunner(t.Context(), harness, env, directory, runner)
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
			if test.wantErr && (err == nil || !strings.Contains(err.Error(), `attachment "conveyor"`) || !strings.Contains(err.Error(), "~/.cursor/mcp.json") || !strings.Contains(err.Error(), "project-level .cursor/mcp.json")) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCursorEnvironmentReadinessRejectsIncompleteChildIdentity(t *testing.T) {
	harness := config.HarnessTemplates()[3].Harness
	err := validateCursorEnvironmentAttachmentWithRunner(t.Context(), harness, []string{"CONVEYOR_ADDR=http://127.0.0.1:9999/mcp"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "child launch identity is incomplete") {
		t.Fatalf("error=%v", err)
	}
}

func grokTestHarness(binary string) config.Harness {
	return config.Harness{
		Name: "grok", MCPTransport: config.MCPTransportEnvironment, MCPAttachment: "conveyor",
		Command:   []string{binary, "--single", "{prompt}", "--permission-mode", "bypassPermissions", "--no-plan"},
		ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--reasoning-effort", "high"}},
		ProbeCommand: []string{binary, "--version"}, ProbeTimeoutText: "30s",
	}
}

func grokTestEnvironment(address string) []string {
	return isolatedChildEnvironment(os.Environ(), map[string]string{
		"CONVEYOR_API_TOKEN": "worker-test-token", "CONVEYOR_ADDR": address, "CONVEYOR_WORKSPACE": "demo",
		"CONVEYOR_WORK_ORDER_ID": "order", "CONVEYOR_SESSION_ID": "session", "CONVEYOR_CLIENT_TOKEN": "client",
	})
}
