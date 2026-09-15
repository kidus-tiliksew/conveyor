package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMCPInstallSelectedCursorNarrowsTargetAndPreservesOtherServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONVEYOR_ADDR", "")
	t.Setenv("CONVEYOR_API_TOKEN", "bridge-present")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"theme" : "dark", "mcpServers" : {"other":{"command":"other"}}}` + "\n"
	if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
		t.Fatal(err)
	}
	command := mcpInstallCmdWithLookPath(mcpLegacyToolsOnPath)
	command.SetArgs([]string{"--tool", "cursor"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(content, []byte(`"theme" : "dark"`)) || !bytes.Contains(content, []byte(`"other":{"command":"other"}`)) {
		t.Fatalf("Cursor unrelated bytes changed:\n%s\nerr=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("selected Cursor install touched Codex: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("selected Cursor install touched Claude: %v", err)
	}
}

func TestMCPInstallCursorRefusesUnsafeDestinations(t *testing.T) {
	for _, setup := range []func(string) error{
		func(path string) error { return os.Symlink(filepath.Join(t.TempDir(), "target"), path) },
		func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		home := t.TempDir()
		path := filepath.Join(home, ".cursor", "mcp.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := setup(path); err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileMCPRegistration(home, mcpInstallTarget{tool: "cursor", path: path}, "https://factory.example/mcp", false, true); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("unsafe Cursor destination error=%v", err)
		}
	}
}

func TestMCPInstallFixtureIsolatesAmbientServerSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONVEYOR_ADDR", "http://localhost:8080")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")

	command := mcpInstallCmdWithLookPath(func(string) (string, error) {
		return openCodeValidationBinary(t, `echo '{"http_headers_helper":"fixture"}'`), nil
	})
	command.SetArgs([]string{"--tool", "codex"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte(`https://factory.example/mcp`)) || bytes.Contains(content, []byte("localhost:8080")) {
		t.Fatalf("ambient server leaked into MCP registration:\n%s", content)
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
	t.Setenv("CONVEYOR_ADDR", "")
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

func mcpLegacyToolsOnPath(name string) (string, error) {
	if name == "opencode" {
		return "", exec.ErrNotFound
	}
	return "/tools/" + name, nil
}

func noOpenCodeOnPath(string) (string, error) { return "", exec.ErrNotFound }

func TestOpenCodeMCPTargets(t *testing.T) {
	home := t.TempDir()
	for _, xdg := range []string{"", t.TempDir()} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		root := xdg
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		targets := mcpTargets(home, []skillTool{{name: "opencode"}})
		if len(targets) != 1 || targets[0].path != filepath.Join(root, "opencode", "opencode.json") {
			t.Fatalf("XDG %q targets=%+v", xdg, targets)
		}
		result, err := reconcileMCPRegistrationWithLookPath(home, targets[0], "https://factory.example/mcp", false, true, noOpenCodeOnPath)
		if err != nil || result.status != "created" || !strings.Contains(result.validation, "skipped") {
			t.Fatalf("XDG %q result=%+v err=%v", xdg, result, err)
		}
	}
}

func TestOpenCodeMCPMacOSSystemAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS system alias regression")
	}
	// Pin this fixture to /var even when the test runner sets TMPDIR elsewhere.
	fixture, err := os.MkdirTemp("/var/tmp", "conveyor-opencode-alias-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixture) })
	root, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		t.Fatal(err)
	}
	aliasRoot := strings.TrimPrefix(root, "/private")
	for _, configRoot := range []string{root, aliasRoot} {
		t.Setenv("XDG_CONFIG_HOME", configRoot)
		target := mcpTargets(t.TempDir(), []skillTool{{name: "opencode"}})[0]
		result, err := reconcileMCPRegistrationWithLookPath(t.TempDir(), target, "https://factory.example/mcp", false, true, noOpenCodeOnPath)
		if err != nil || (result.status != "created" && result.status != "unchanged") {
			t.Fatalf("config root %s: result=%+v err=%v", configRoot, result, err)
		}
	}
	// A user-controlled parent below the system alias must still be refused.
	if err := os.Symlink(filepath.Join(root, "opencode"), filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	target := mcpInstallTarget{tool: "opencode", path: filepath.Join(aliasRoot, "redirect", "opencode.json")}
	if _, err := reconcileMCPRegistrationWithLookPath(root, target, "https://factory.example/mcp", true, true, noOpenCodeOnPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("accepted symlink below system alias: %v", err)
	}
}

func TestOpenCodeMCPRefusesCommentsAndMalformedJSON(t *testing.T) {
	for _, prior := range []string{
		"{\n// keep my comment\n}",
		`{"mcp":{/* keep */}}`,
		`{"other":{"nested":[1, /* keep */ 2]}}`,
		`{"mcp":{},}`, `{"mcp":null}`, `{"mcp":{},"mcp":{}}`,
	} {
		home := t.TempDir()
		path := filepath.Join(home, "opencode.json")
		if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := reconcileMCPRegistrationWithLookPath(home, mcpInstallTarget{tool: "opencode", path: path}, "https://factory.example/mcp", true, true, noOpenCodeOnPath)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("prior=%s err=%v", prior, err)
		}
		if strings.Contains(prior, "keep") && !strings.Contains(err.Error(), "comment") {
			t.Fatalf("unclear error: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != prior {
			t.Fatalf("changed invalid file: %s", after)
		}
	}
}

func TestOpenCodeMCPRefusesUnsafePaths(t *testing.T) {
	for _, kind := range []string{"destination symlink", "parent symlink", "directory", "relative XDG"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "opencode.json")
			other := filepath.Join(t.TempDir(), "original")
			if err := os.WriteFile(other, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "destination symlink":
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if err := os.Symlink(filepath.Dir(other), path); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "child.json")
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "relative XDG":
				path = "relative/opencode/opencode.json"
			}
			_, err := reconcileMCPRegistrationWithLookPath(home, mcpInstallTarget{tool: "opencode", path: path}, "https://factory.example/mcp", true, true, noOpenCodeOnPath)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("unsafe path accepted: %v", err)
			}
			got, _ := os.ReadFile(other)
			if string(got) != "original" {
				t.Fatal("modified symlink target")
			}
		})
	}
}

func openCodeValidationBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenCodeMCPStagedValidation(t *testing.T) {
	for _, failure := range []string{"", "exit 7", "echo 'Unrecognized key resolved-secret' >&2", "echo 'ConfigInvalid resolved-secret' >&2"} {
		for _, exists := range []bool{false, true} {
			home := t.TempDir()
			path := filepath.Join(home, "opencode.json")
			prior := `{"model" : "old", "mcp":{"other":{"type":"local","command":["other"]}}}`
			if exists {
				if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ORIGINAL_CONFIG", path)
			t.Setenv("OPENCODE_CONFIG_CONTENT", `{"invalid":"must not be loaded"}`)
			binary := openCodeValidationBinary(t, `
[ "$*" = 'debug config' ]
[ -f "$OPENCODE_CONFIG" ]
[ "$OPENCODE_CONFIG" != "$ORIGINAL_CONFIG" ]
[ "${OPENCODE_CONFIG_CONTENT-unset}" = unset ]
[ "$OPENCODE_DISABLE_PROJECT_CONFIG" = true ]
[ "$HOME" = "$PWD" ]
[ "$XDG_CONFIG_HOME" = "$HOME/config" ]
[ "$XDG_DATA_HOME" = "$HOME/data" ]
if read -r ignored; then exit 8; fi
/usr/bin/grep -q '"type":"remote"' "$OPENCODE_CONFIG"
/usr/bin/grep -q '{env:CONVEYOR_MCP_TOKEN_' "$OPENCODE_CONFIG"
if [ -f "$ORIGINAL_CONFIG" ]; then
  /usr/bin/grep -q '"model" : "old"' "$ORIGINAL_CONFIG"
  if /usr/bin/grep -q '_conveyor_mcp_install' "$ORIGINAL_CONFIG"; then exit 9; fi
fi
echo 'resolved-secret stdout'
echo 'resolved-secret stderr' >&2
# OpenCode can rewrite its validation copy. Never publish those bytes.
echo '{"rewritten":true}' > "$OPENCODE_CONFIG"
`+failure)
			lookPath := func(string) (string, error) { return binary, nil }
			result, err := reconcileMCPRegistrationWithLookPath(home, mcpInstallTarget{tool: "opencode", path: path}, "https://factory.example/mcp", false, true, lookPath)
			got, readErr := os.ReadFile(path)
			if failure != "" {
				if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "resolved-secret") {
					t.Fatalf("validation error=%v", err)
				}
				if exists && (readErr != nil || string(got) != prior) {
					t.Fatalf("failed validation replaced original: %s %v", got, readErr)
				}
				if !exists && !os.IsNotExist(readErr) {
					t.Fatalf("failed validation created file: %v", readErr)
				}
			} else if err != nil || result.status != "created" || result.validation != "validated with opencode debug config" || bytes.Contains(got, []byte("rewritten")) {
				t.Fatalf("validation result=%+v err=%v content=%s", result, err, got)
			}
			leftovers, _ := filepath.Glob(filepath.Join(home, ".conveyor-*"))
			if len(leftovers) != 0 {
				t.Fatalf("staging leftovers: %v", leftovers)
			}
		}
	}
}

func TestOpenCodeMCPValidationTimeoutAndSplitMarkers(t *testing.T) {
	home := t.TempDir()
	staged := filepath.Join(home, "staged.json")
	if err := os.WriteFile(staged, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := openCodeValidationBinary(t, "while :; do :; done")
	start := time.Now()
	err := validateOpenCodeMCP(binary, staged, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout=%v elapsed=%s", err, time.Since(start))
	}
	var stderr openCodeValidationStderr
	_, _ = stderr.Write([]byte("Unrecognized "))
	_, _ = stderr.Write([]byte("key"))
	if !stderr.invalid {
		t.Fatal("lost marker across stderr chunks")
	}
}

func TestOpenCodeMCPDetectedInstallSuppressesValidationOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CONVEYOR_API_TOKEN", "bridge-present")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")
	t.Setenv("CONVEYOR_ADDR", "https://factory.example/mcp")
	binary := openCodeValidationBinary(t, "echo resolved-secret; echo resolved-secret >&2")
	cmd := mcpInstallCmdWithLookPath(func(name string) (string, error) {
		if name == "opencode" {
			return binary, nil
		}
		return "", exec.ErrNotFound
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "opencode\tcreated\t") || !strings.Contains(output.String(), "validated with opencode debug config") {
		t.Fatalf("output=%s", output.String())
	}
	for _, forbidden := range []string{"resolved-secret", "stored-secret", "export CONVEYOR_ADDR", "export CONVEYOR_API_TOKEN"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("unexpected output %q: %s", forbidden, output.String())
		}
	}
}

// Opt in with an absolute binary path to check a locally installed OpenCode
// version. The ordinary gate uses the deterministic subprocess fixtures above.

func TestOpenCodeMCPInstalledBinary(t *testing.T) {
	binary := os.Getenv("CONVEYOR_TEST_OPENCODE_BIN")
	if binary == "" {
		t.Skip("set CONVEYOR_TEST_OPENCODE_BIN for isolated native validation")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("CONVEYOR_TEST_OPENCODE_BIN must be absolute")
	}
	home := t.TempDir()
	path := filepath.Join(home, "opencode.json")
	lookup := func(string) (string, error) { return binary, nil }
	result, err := reconcileMCPRegistrationWithLookPath(home, mcpInstallTarget{tool: "opencode", path: path}, "https://factory.example/mcp", false, true, lookup)
	if err != nil || result.status != "created" {
		t.Fatalf("native create=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("$schema")) || bytes.Contains(got, []byte("conveyor-install-validation")) {
		t.Fatalf("native loader changes or resolved values published: %s", got)
	}
	invalidPath := filepath.Join(home, "invalid.json")
	prior := []byte(`{"unknown_top_level_key":true}`)
	if err := os.WriteFile(invalidPath, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = reconcileMCPRegistrationWithLookPath(home, mcpInstallTarget{tool: "opencode", path: invalidPath}, "https://factory.example/mcp", false, true, lookup)
	if err == nil || !strings.Contains(err.Error(), invalidPath) {
		t.Fatalf("native invalid config accepted: %v", err)
	}
	after, _ := os.ReadFile(invalidPath)
	if !bytes.Equal(prior, after) {
		t.Fatalf("native failure changed prior bytes: %s", after)
	}
}
