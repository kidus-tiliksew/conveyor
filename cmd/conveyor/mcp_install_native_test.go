package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the actual CLI entrypoint in native helpers without building a
// second implementation of token printing or putting secrets in a script.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		main()
		os.Exit(0)
	}
}

func TestMCPTokenBridgeStoredRotationAndMissing(t *testing.T) {
	server := "https://one.example"
	setMCPTestCredentials(t, server, "fixture-first")
	store, err := localAuthConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for _, value := range []string{"fixture-first", "fixture-rotated", ""} {
		if err := updateLocalServerConfig(server, func(e *localServerConfig) { e.Token = value }); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "auth", "token", "--server", server, "--credentials-file", store, "--format", "http-headers")
		cmd.Dir = home
		cmd.Env = append(mcpIsolatedEnvironment(home), "CONVEYOR_ADDR=https://elsewhere.example", "CONVEYOR_API_TOKEN=ambient-fixture")
		var output, diagnostic bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &diagnostic
		err := cmd.Run()
		if value == "" {
			if err == nil || output.Len() != 0 || !strings.Contains(diagnostic.String(), "no stored credential") {
				t.Fatal("missing credential did not fail cleanly")
			}
			continue
		}
		if err != nil {
			t.Fatalf("helper failed: %v", err)
		}
		var headers map[string]string
		if err := json.Unmarshal(output.Bytes(), &headers); err != nil || headers["Authorization"] != "Bearer "+value {
			t.Fatal("helper did not read selected saved credential")
		}
		if diagnostic.Len() != 0 {
			t.Fatal("helper wrote a diagnostic on success")
		}
	}
}

type nativeMCPFixture struct {
	mu                            sync.Mutex
	initialized, listed, rejected int
	token                         string
	toolCalls                     []nativeMCPToolCall
}

type nativeMCPToolCall struct {
	Name      string
	Arguments map[string]any
	Meta      map[string]any
}

func (f *nativeMCPFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.rejected++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&call) != nil {
		w.WriteHeader(400)
		return
	}
	if len(call.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch call.Method {
	case "initialize":
		f.initialized++
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(call.Params, &params)
		result = map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "conveyor-fixture", "version": "1"}}
	case "tools/list":
		f.listed++
		object := func(properties map[string]any, required ...string) map[string]any {
			return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
		}
		str := map[string]any{"type": "string"}
		result = map[string]any{"tools": []any{
			map[string]any{"name": "list_tasks", "description": "Controlled list fixture", "inputSchema": object(map[string]any{"workspace_id": str, "state": str, "limit": map[string]any{"type": "integer"}}, "workspace_id")},
			map[string]any{"name": "get_document", "description": "Controlled document fixture", "inputSchema": object(map[string]any{"workspace_id": str, "kind": str, "document_id": str}, "workspace_id", "kind", "document_id")},
		}}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.toolCalls = append(f.toolCalls, nativeMCPToolCall{Name: params.Name, Arguments: params.Arguments, Meta: params.Meta})
		malformed := false
		if params.Name == "list_tasks" {
			_, malformed = params.Arguments["limit"].(string)
		}
		if params.Name == "get_document" {
			_, malformed = params.Arguments["unexpected"]
		}
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{}`}}, "isError": malformed}
	case "resources/list":
		result = map[string]any{"resources": []any{}}
	case "resources/templates/list":
		result = map[string]any{"resourceTemplates": []any{}}
	case "prompts/list":
		result = map[string]any{"prompts": []any{}}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

func TestMCPNativeClients(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_NATIVE_MCP") != "1" {
		t.Skip("set CONVEYOR_TEST_NATIVE_MCP=1 for installed parser and controlled native runtime checks")
	}
	for _, tool := range []string{"codex", "claude", "opencode", "cursor"} {
		t.Run(tool, func(t *testing.T) {
			lookup := tool
			if tool == "cursor" {
				lookup = "cursor-agent"
			}
			binary, err := exec.LookPath(lookup)
			if err != nil {
				t.Skipf("%s unavailable; no native evidence", lookup)
			}
			version, err := exec.Command(binary, "--version").Output()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("client %s", strings.TrimSpace(string(version)))
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			one := &nativeMCPFixture{token: "fixture-one"}
			two := &nativeMCPFixture{token: "fixture-two"}
			a := httptest.NewServer(http.HandlerFunc(one.serve))
			defer a.Close()
			b := httptest.NewServer(http.HandlerFunc(two.serve))
			defer b.Close()
			credentialPath := filepath.Join(home, "saved credentials.json")
			config := localAuthConfig{Servers: map[string]localServerConfig{a.URL: {Token: one.token}, b.URL: {Token: two.token}}}
			credentialBytes, _ := json.Marshal(config)
			if err := os.WriteFile(credentialPath, credentialBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			helper, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			target := mcpTargets(home, []skillTool{{name: tool}})[0]
			ids := []mcpIdentity{}
			for _, server := range []string{a.URL, b.URL} {
				id, err := newMCPIdentity(server, "", helper, credentialPath)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				result, err := reconcileNamedMCPRegistration(home, target, id, false, true, exec.LookPath)
				if err != nil {
					t.Fatalf("native parser: %v", err)
				}
				t.Logf("%s: %s", tool, result.validation)
			}
			env := append(mcpIsolatedEnvironment(home), "CONVEYOR_ADDR=https://wrong.example", "CONVEYOR_API_TOKEN=wrong-fixture", ids[0].tokenEnv+"="+one.token, ids[1].tokenEnv+"="+two.token)
			run := func(expectRejection bool) {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				if tool == "codex" {
					runNativeCodexFixture(t, ctx, binary, home, env, []string{ids[0].name, ids[1].name}, expectRejection)
					return
				}
				calls := [][]string{{"mcp", "list"}}
				if tool == "cursor" {
					calls = [][]string{{"mcp", "list-tools", ids[0].name}, {"mcp", "list-tools", ids[1].name}}
				}
				for _, args := range calls {
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Env = env
					cmd.Dir = home
					cmd.Stdout = io.Discard
					cmd.Stderr = io.Discard
					cmd.WaitDelay = time.Second
					if err := cmd.Run(); err != nil && !expectRejection {
						t.Fatalf("native client failed (output withheld): %v", err)
					}
					if ctx.Err() != nil {
						t.Fatal("native client timed out")
					}
				}
			}
			run(false)
			for i, f := range []*nativeMCPFixture{one, two} {
				f.mu.Lock()
				initialized, listed, rejected, toolCalls := f.initialized, f.listed, f.rejected, append([]nativeMCPToolCall(nil), f.toolCalls...)
				f.mu.Unlock()
				if initialized == 0 || listed == 0 || rejected != 0 {
					t.Fatalf("server %d native evidence: initialize=%d tools/list=%d rejected=%d", i, initialized, listed, rejected)
				}
				if tool == "codex" {
					seen := map[string]int{}
					for _, call := range toolCalls {
						seen[call.Name]++
						if call.Arguments["workspace_id"] != "demo" || len(call.Meta) == 0 {
							t.Fatalf("server %d native call %s missing bounded arguments or metadata", i, call.Name)
						}
					}
					if seen["list_tasks"] != 2 || seen["get_document"] != 2 {
						t.Fatalf("server %d missing affected native calls: %v", i, seen)
					}
				}
			}
			t.Log("two distinct saved servers: native initialize and tools/list passed; unrelated environment credential never arrived")
			// The next native connection must read rotations from the same file.
			for _, f := range []*nativeMCPFixture{one, two} {
				f.mu.Lock()
				f.token += "-rotated"
				f.initialized = 0
				f.listed = 0
				f.toolCalls = nil
				f.mu.Unlock()
			}
			config.Servers[a.URL] = localServerConfig{Token: one.token}
			config.Servers[b.URL] = localServerConfig{Token: two.token}
			credentialBytes, _ = json.Marshal(config)
			if err := os.WriteFile(credentialPath, credentialBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			// Environment clients explicitly refresh their individual exports.
			env = append(mcpIsolatedEnvironment(home), "CONVEYOR_ADDR=https://wrong.example", "CONVEYOR_API_TOKEN=wrong-fixture", ids[0].tokenEnv+"="+one.token, ids[1].tokenEnv+"="+two.token)
			run(false)
			for _, f := range []*nativeMCPFixture{one, two} {
				f.mu.Lock()
				initialized, listed, rejected := f.initialized, f.listed, f.rejected
				f.mu.Unlock()
				if initialized == 0 || listed == 0 || rejected != 0 {
					t.Fatal("native rotation failed")
				}
			}
			// A revoked credential must fail on use, never switch to ambient auth.
			for _, f := range []*nativeMCPFixture{one, two} {
				f.mu.Lock()
				f.token = "revoked-no-longer-valid"
				f.initialized = 0
				f.listed = 0
				f.mu.Unlock()
			}
			run(true)
			for _, f := range []*nativeMCPFixture{one, two} {
				f.mu.Lock()
				initialized, listed, rejected := f.initialized, f.listed, f.rejected
				f.mu.Unlock()
				if initialized != 0 || listed != 0 || rejected == 0 {
					t.Fatal("native revocation was not enforced")
				}
			}
			t.Log("native credential rotation and rejection passed")
		})
	}
}

func runNativeCodexFixture(t *testing.T, ctx context.Context, binary, home string, env, servers []string, expectRejection bool) {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "app-server", "--stdio")
	cmd.Env = env
	cmd.Dir = home
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	send := func(id int, method string, params any) {
		b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		_, _ = fmt.Fprintln(input, string(b))
	}
	receive := func(id int) json.RawMessage {
		for scanner.Scan() {
			var reply struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(scanner.Bytes(), &reply) == nil && reply.ID == id {
				if len(reply.Error) > 0 {
					t.Fatalf("native Codex %d request rejected (output withheld)", id)
				}
				return reply.Result
			}
		}
		t.Fatalf("native Codex stopped before response %d: %v", id, ctx.Err())
		return nil
	}
	send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "conveyor-fixture", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	_ = receive(1)
	_, _ = fmt.Fprintln(input, `{"method":"initialized","params":{}}`)
	send(2, "mcpServerStatus/list", map[string]any{})
	_ = receive(2)
	if expectRejection {
		return
	}
	send(3, "thread/start", map[string]any{"cwd": home, "ephemeral": true})
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(receive(3), &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("native Codex thread start: %v", err)
	}
	id := 4
	for _, server := range servers {
		for _, call := range []struct {
			tool      string
			arguments map[string]any
			wantError bool
		}{
			{tool: "list_tasks", arguments: map[string]any{"workspace_id": "demo", "state": "all", "limit": 2}},
			{tool: "list_tasks", arguments: map[string]any{"workspace_id": "demo", "limit": "2"}, wantError: true},
			{tool: "get_document", arguments: map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": "component-mcp-protocol"}},
			{tool: "get_document", arguments: map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": "component-mcp-protocol", "unexpected": true}, wantError: true},
		} {
			send(id, "mcpServer/tool/call", map[string]any{"server": server, "threadId": started.Thread.ID, "tool": call.tool, "arguments": call.arguments, "_meta": map[string]any{"progressToken": id}})
			var result struct {
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(receive(id), &result); err != nil || result.IsError != call.wantError {
				t.Fatalf("native Codex %s response classification: isError=%t want=%t error=%v", call.tool, result.IsError, call.wantError, err)
			}
			id++
		}
	}
}
