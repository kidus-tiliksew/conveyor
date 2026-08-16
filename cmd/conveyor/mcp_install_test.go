package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInstallWritesNativeShapesWithoutTokenAndPreservesContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONVEYOR_API_TOKEN", "")
	setMCPTestCredentials(t, "https://factory.example/base", "literal-secret-never-write")
	codexPath := filepath.Join(home, ".codex", "config.toml")
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	codexOther := "model = \"gpt-5.6\"\n\n[mcp_servers.other]\nurl = \"https://other.example/mcp\"\n"
	if err := os.WriteFile(codexPath, []byte(codexOther), 0o640); err != nil {
		t.Fatal(err)
	}
	claudeOther := `{"theme" : "dark", "mcpServers" : {"other":{"type":"stdio","command":"other"}}, "custom": [1, 2, 3]}` + "\n"
	if err := os.WriteFile(claudePath, []byte(claudeOther), 0o640); err != nil {
		t.Fatal(err)
	}

	command := mcpInstallCmdWithLookPath(func(name string) (string, error) { return "/tools/" + name, nil })
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\tcreated\t") != 2 || !strings.Contains(output.String(), mcpBridgeGuidance) {
		t.Fatalf("install output:\n%s", output.String())
	}
	for _, path := range []string{codexPath, claudePath} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte("literal-secret-never-write")) {
			t.Fatalf("token leaked to %s", path)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("mode for %s = %v err=%v", path, info.Mode().Perm(), err)
		}
	}
	codex, _ := os.ReadFile(codexPath)
	if !bytes.HasPrefix(codex, []byte(codexOther)) || !bytes.Contains(codex, []byte(`[mcp_servers.conveyor]
url = "https://factory.example/base/mcp"
bearer_token_env_var = "CONVEYOR_API_TOKEN"`)) {
		t.Fatalf("Codex content:\n%s", codex)
	}
	claude, _ := os.ReadFile(claudePath)
	for _, unchanged := range []string{`"theme" : "dark"`, `"other":{"type":"stdio","command":"other"}`, `"custom": [1, 2, 3]`} {
		if !bytes.Contains(claude, []byte(unchanged)) {
			t.Fatalf("Claude unrelated bytes changed; missing %q:\n%s", unchanged, claude)
		}
	}
	var document struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(claude, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.MCPServers["conveyor"]; got.Type != "http" || got.URL != "https://factory.example/base/mcp" || got.Headers["Authorization"] != "Bearer ${CONVEYOR_API_TOKEN}" {
		t.Fatalf("Claude native registration = %+v", got)
	}
}

func TestMCPInstallSkipsUnmarkedAndAdoptsExplicitly(t *testing.T) {
	endpoint := "https://new.example/mcp"
	tests := []struct {
		tool  string
		prior string
	}{
		{"codex", "before = true\n[mcp_servers.conveyor]\nurl = \"https://old.example/mcp\"\nafter = true\n"},
		{"claude", `{"untouched" : true,"mcpServers":{"conveyor":{"type":"http","url":"https://old.example/mcp"}}}` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config")
			if err := os.WriteFile(path, []byte(test.prior), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := reconcileMCPRegistration(home, mcpInstallTarget{tool: test.tool, path: path}, endpoint, false, true)
			if err != nil || result.status != "skipped" {
				t.Fatalf("skip result=%+v err=%v", result, err)
			}
			content, _ := os.ReadFile(path)
			if string(content) != test.prior {
				t.Fatalf("unmarked entry changed:\n%s", content)
			}
			result, err = reconcileMCPRegistration(home, mcpInstallTarget{tool: test.tool, path: path}, endpoint, true, true)
			if err != nil || result.status != "refreshed" {
				t.Fatalf("adopt result=%+v err=%v", result, err)
			}
			content, _ = os.ReadFile(path)
			if bytes.Contains(content, []byte("old.example")) || !bytes.Contains(content, []byte("new.example")) {
				t.Fatalf("adoption did not refresh:\n%s", content)
			}
		})
	}
}

func TestMCPInstallListIsReadOnlyAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONVEYOR_API_TOKEN", "bridge-present")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")
	lookPath := func(name string) (string, error) { return "/tools/" + name, nil }
	list := mcpInstallCmdWithLookPath(lookPath)
	list.SetArgs([]string{"--list"})
	var output bytes.Buffer
	list.SetOut(&output)
	if err := list.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "not installed (would create)") != 2 || strings.Contains(output.String(), mcpBridgeGuidance) {
		t.Fatalf("list output:\n%s", output.String())
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("list wrote home: %v err=%v", entries, err)
	}
	install := mcpInstallCmdWithLookPath(lookPath)
	if err := install.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".claude.json")} {
		before, _ := os.ReadFile(path)
		info, _ := os.Stat(path)
		install = mcpInstallCmdWithLookPath(lookPath)
		output.Reset()
		install.SetOut(&output)
		if err := install.Execute(); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) || info.Mode().Perm() != 0o600 {
			t.Fatalf("idempotent refresh changed %s", path)
		}
	}
}

func TestSelectedStoredMCPServerRequiresUnambiguousStoredCredential(t *testing.T) {
	t.Setenv("CONVEYOR_ADDR", "")
	serverFlag, serverFlagExplicit = "", false
	t.Cleanup(func() { serverFlag, serverFlagExplicit = "", false })
	setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{
		"https://one.example": {Token: "one"},
		"https://two.example": {Token: "two"},
	}})
	if _, err := selectedStoredMCPServer(); err == nil || !strings.Contains(err.Error(), "multiple stored") {
		t.Fatalf("ambiguous error=%v", err)
	}
	serverFlag, serverFlagExplicit = "https://two.example", true
	if server, err := selectedStoredMCPServer(); err != nil || server != "https://two.example" {
		t.Fatalf("selected server=%q err=%v", server, err)
	}
}

func setMCPTestCredentials(t *testing.T, server, token string) {
	t.Helper()
	setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{server: {Token: token}}})
	serverFlag, serverFlagExplicit = "", false
	t.Cleanup(func() { serverFlag, serverFlagExplicit = "", false })
}

func setMCPTestConfig(t *testing.T, config localAuthConfig) {
	t.Helper()
	root := t.TempDir()
	previous := userConfigDir
	userConfigDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { userConfigDir = previous })
	if err := saveLocalAuthConfig(config); err != nil {
		t.Fatal(err)
	}
}
