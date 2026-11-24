package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	command.SetArgs([]string{"--auth", "pat", "--tool", "cursor"})
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
	command.SetArgs([]string{"--auth", "pat", "--tool", "codex"})
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

// openCodeUncontained runs a stub as a direct child and reports the ready
// state that the namespace helper reports in production.
func openCodeUncontained(binary string, args []string, status *os.File) (*exec.Cmd, error) {
	if _, err := status.Write([]byte{openCodeReady}); err != nil {
		return nil, err
	}
	return exec.Command(binary, args...), nil
}

// openCodeVersionBinary writes an `opencode` stub that answers the version
// probe with version and runs body for each other command.
func openCodeVersionBinary(t *testing.T, version, body string) string {
	t.Helper()
	return openCodeValidationBinary(t, `if [ "$*" = --version ]; then echo '`+version+`'; exit 0; fi
`+body)
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
			binary := openCodeVersionBinary(t, "1.19.6", `
[ "$*" = 'debug config' ]
[ -f "$OPENCODE_CONFIG" ]
[ "$OPENCODE_CONFIG" != "$ORIGINAL_CONFIG" ]
[ "${OPENCODE_CONFIG_CONTENT-unset}" = unset ]
[ "$OPENCODE_DISABLE_PROJECT_CONFIG" = true ]
[ "$HOME" = "$PWD" ]
[ "$XDG_CONFIG_HOME" = "$HOME/config" ]
[ "$XDG_DATA_HOME" = "$HOME/data" ]
[ "$(/usr/bin/readlink "$XDG_DATA_HOME/opencode/log/opencode.log")" = /dev/null ]
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
			} else if err != nil || result.status != "created" || result.validation != "validated with opencode debug config (not a native connection check)" || bytes.Contains(got, []byte("rewritten")) {
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
	id := testMCPOAuthIdentity(t, "https://factory.example", "")
	// The probe and the parser share one deadline.
	for _, binary := range []string{
		openCodeValidationBinary(t, "while :; do :; done"),
		openCodeVersionBinary(t, "1.19.6", "while :; do :; done"),
		openCodeVersionBinary(t, "opencode v2.4.1", "while :; do :; done"),
	} {
		start := time.Now()
		command, err := validateOpenCodeMCP(binary, staged, id.name, id, time.Second)
		if !errors.Is(err, errOpenCodeTimeout) || !strings.Contains(err.Error(), "timed out") || command != "" || time.Since(start) > 10*time.Second {
			t.Fatalf("timeout=%v elapsed=%s", err, time.Since(start))
		}
	}
	var stderr openCodeValidationStderr
	_, _ = stderr.Write([]byte("Unrecognized "))
	_, _ = stderr.Write([]byte("key"))
	if !stderr.invalid {
		t.Fatal("lost marker across stderr chunks")
	}
	for _, marker := range []string{"ConfigInvalid", "Server process exited", "Managed service port"} {
		for split := 1; split < len(marker); split++ {
			var stderr openCodeValidationStderr
			_, _ = stderr.Write([]byte("noise " + marker[:split]))
			_, _ = stderr.Write([]byte(marker[split:] + " noise"))
			if stderr.invalid != (marker == "ConfigInvalid") || stderr.serverStart == stderr.invalid {
				t.Fatalf("marker %q split at %d: %+v", marker, split, stderr)
			}
		}
	}
}

func TestOpenCodeMCPDetectedInstallSuppressesValidationOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CONVEYOR_API_TOKEN", "bridge-present")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")
	t.Setenv("CONVEYOR_ADDR", "https://factory.example/mcp")
	binary := openCodeVersionBinary(t, "1.19.6", "echo resolved-secret; echo resolved-secret >&2")
	cmd := mcpInstallCmdWithLookPath(func(name string) (string, error) {
		if name == "opencode" {
			return binary, nil
		}
		return "", exec.ErrNotFound
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--auth", "pat"})
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

// useOpenCodeContainment runs the check of the test in the production
// containment.
func useOpenCodeContainment(t *testing.T) {
	t.Helper()
	previous := openCodeContain
	openCodeContain = openCodeContained
	t.Cleanup(func() { openCodeContain = previous })
}

// openCodeFixtureArg starts the test binary as a client fixture. The Linux
// tests set the fixture.
const openCodeFixtureArg = "__opencode-fixture"

var openCodeFixture = func([]string) int { return 125 }

func runOpenCodeFixture(args []string) int { return openCodeFixture(args) }

// openCodePortOwner names the process that listens on a loopback TCP port by
// PID, kernel start time, and command line.
func openCodePortOwner(t *testing.T, port string) string {
	t.Helper()
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	table, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != "0A" || !strings.HasSuffix(fields[1], fmt.Sprintf(":%04X", number)) {
			continue
		}
		descriptors, _ := filepath.Glob("/proc/[0-9]*/fd/*")
		for _, descriptor := range descriptors {
			if link, _ := os.Readlink(descriptor); link != "socket:["+fields[9]+"]" {
				continue
			}
			process := filepath.Dir(filepath.Dir(descriptor))
			stat, _ := os.ReadFile(filepath.Join(process, "stat"))
			command, _ := os.ReadFile(filepath.Join(process, "cmdline"))
			after := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
			return fmt.Sprintf("pid=%s start=%s command=%s", filepath.Base(process), after[19], bytes.ReplaceAll(command, []byte{0}, []byte{' '}))
		}
	}
	t.Fatalf("no process listens on port %s", port)
	return ""
}

func TestOpenCodeVersionLine(t *testing.T) {
	for output, want := range map[string]string{
		"1.19.6\n": "1", "opencode v2.4.1\n": "2", "v2.4.1": "2", "opencode 2.4.1\r\n": "2",
		"2.5.0-beta.1\n": "2", "2.5.0+build.7\n": "2", "opencode v2.5.0-rc-1.x+abc\n": "2",
		"0.0.0-beta-19507\n": "0", "3.0.0\n": "3", "12.0.0\n": "12",
		"": "", "\n": "", "opencode\n": "", "2\n": "", "2.4\n": "", "2.4.1.0\n": "", "02.4.1\n": "", "vv2.4.1\n": "",
		"2.4.1\n1.19.6\n": "", "1.19.6 2.4.1\n": "", "notice\n2.4.1\n": "", "2.4.1\n\n": "", " 2.4.1\n": "", "2.4.1-\n": "", "2.4.1-01\n": "",
		"other v2.4.1\n": "", "opencode v2.4.1 resolved-secret\n": "", "\x00\xff2.4.1\n": "",
	} {
		if got := openCodeMajor([]byte(output)); got != want {
			t.Errorf("major(%q)=%q, want %q", output, got, want)
		}
	}
}

// TestOpenCodeMCPVersionDispatch covers the refused probes: each one stops
// before the parser, with no fallback and no publication.
func TestOpenCodeMCPVersionDispatch(t *testing.T) {
	for label, c := range map[string]struct {
		probe string
		want  error
	}{
		"failed":      {"echo 1.19.6; echo resolved-secret >&2; exit 3", errOpenCodeVersion},
		"empty":       {"exit 0", errOpenCodeVersion},
		"stderr only": {"echo 2.4.1 >&2; exit 0", errOpenCodeVersion},
		"two lines":   {"echo resolved-secret; echo 2.4.1; exit 0", errOpenCodeVersion},
		"old major":   {"echo 0.15.0; exit 0", errOpenCodeVersion},
		"new major":   {"echo opencode v3.0.0; exit 0", errOpenCodeVersion},
		"too large":   {"while :; do echo resolved-secret; done", errOpenCodeOutputSize},
	} {
		t.Run(label, func(t *testing.T) {
			binary := openCodeValidationBinary(t, `echo "$*" >> "$ARGUMENT_LOG"
if [ "$*" = --version ]; then `+c.probe+`; fi`)
			checkOpenCodeRefusal(t, mcpAuthOAuth, binary, c.want, "--version\n")
		})
	}
}

// checkOpenCodeRefusal proves at the install boundary that a check refuses
// with the fixed message alone, keeps prior bytes and mode or an absent
// destination, leaves no staging file, and ran exactly the given commands.
func checkOpenCodeRefusal(t *testing.T, mode, binary string, want error, commands string) {
	t.Helper()
	for _, exists := range []bool{false, true} {
		home := t.TempDir()
		path := filepath.Join(home, "opencode.json")
		prior := []byte(`{"model" : "old"}`)
		if exists {
			if err := os.WriteFile(path, prior, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		log := filepath.Join(t.TempDir(), "arguments")
		t.Setenv("ARGUMENT_LOG", log)
		id := testMCPModeIdentity(t, mode, "https://factory.example", "")
		result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "opencode", path: path}, id, false, true, func(string) (string, error) { return binary, nil })
		if !errors.Is(err, want) || err.Error() != "validate MCP config "+path+": "+want.Error() || result != (mcpInstallResult{}) {
			t.Fatalf("result=%+v err=%v, want %v", result, err, want)
		}
		for _, forbidden := range []string{"resolved-secret", openCodePlaceholder} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("refusal carries %q: %v", forbidden, err)
			}
		}
		after, readErr := os.ReadFile(path)
		if exists {
			info, _ := os.Stat(path)
			if readErr != nil || !bytes.Equal(after, prior) || info.Mode().Perm() != 0o640 {
				t.Fatalf("refusal changed the prior file: %s %v", after, readErr)
			}
		} else if !os.IsNotExist(readErr) {
			t.Fatalf("refusal created a file: %v", readErr)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(home, ".conveyor-*")); len(leftovers) != 0 {
			t.Fatalf("staging leftovers: %v", leftovers)
		}
		if got, _ := os.ReadFile(log); string(got) != commands {
			t.Fatalf("commands=%q, want %q", got, commands)
		}
	}
}

// openCodeV2Binary writes an OpenCode 2 stub. body runs for the standalone
// check. STAGED_COPY and ENTRY_NAME are available to it.
func openCodeV2Binary(t *testing.T, body string) string {
	t.Helper()
	return openCodeValidationBinary(t, `[ -z "${ARGUMENT_LOG-}" ] || echo "$*" >> "$ARGUMENT_LOG"
if [ "$*" = --version ]; then echo 'opencode v2.4.1'; exit 0; fi
[ "$*" = 'debug config --standalone' ]
[ "$HOME" = "$PWD" ]
[ "$(/usr/bin/readlink "$XDG_DATA_HOME/opencode/log/opencode.log")" = /dev/null ]
STAGED_COPY=$OPENCODE_CONFIG
echo 'resolved-secret stderr' >&2
`+body)
}

// openCodeV2Rows prints a result with the given server members. Each other
// row and field is unrelated to the registration.
func openCodeV2Rows(servers string) string {
	return `printf '[{"type":"directory","path":"%s"},{"type":"document","path":"/elsewhere.json","info":{"mcp":{"servers":{}}}},{"type":"document","path":"%s","info":{"model":"resolved-secret","mcp":{"servers":{` + servers + `}}}}]\n' "$XDG_CONFIG_HOME/opencode" "$STAGED_COPY"`
}

func TestOpenCodeMCPV2Accepted(t *testing.T) {
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		for _, prior := range []string{"", `{"unknown_top_level_key":true,"mcp":{"other":{"type":"local","command":["other"]}}}`} {
			t.Run(fmt.Sprintf("%s/prior=%t", mode, prior != ""), func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "opencode.json")
				if prior != "" {
					if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
						t.Fatal(err)
					}
				}
				id := testMCPModeIdentity(t, mode, "https://factory.example", "")
				entry := `"other":{"type":"local","command":["other"]},"` + id.name + `":{"type":"remote","url":"` + id.endpoint + `","disabled":false`
				if mode == mcpAuthPAT {
					entry += `,"headers":{"Authorization":"***"},"oauth":false`
				}
				binary := openCodeV2Binary(t, openCodeV2Rows(entry+"}")+`
echo '{"rewritten":true}' > "$OPENCODE_CONFIG"`)
				result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "opencode", path: path}, id, false, true, func(string) (string, error) { return binary, nil })
				if err != nil || result.status != "created" || result.validation != "validated with opencode debug config --standalone (not a native connection check)" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				got, _ := os.ReadFile(path)
				want, _, _, err := reconcileNamedJSON([]byte(prior), "opencode", id, false)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("published=%s want=%s err=%v", got, want, err)
				}
				if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
					t.Fatalf("mode=%v", info.Mode())
				}
				if leftovers, _ := filepath.Glob(filepath.Join(home, ".conveyor-*")); len(leftovers) != 0 {
					t.Fatalf("staging leftovers: %v", leftovers)
				}
			})
		}
	}
}

// TestOpenCodeMCPV2Refusals covers each refused result of the OpenCode 2
// check at the install boundary (req-mcp-oauth AC-5.3).
func TestOpenCodeMCPV2Refusals(t *testing.T) {
	const ran = "--version\ndebug config --standalone\n"
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		id := testMCPModeIdentity(t, mode, "https://factory.example", "")
		credential, wrongCredential := "", `,"oauth":false`
		if mode == mcpAuthPAT {
			credential, wrongCredential = `,"oauth":false`, ""
		}
		fields := `"type":"remote","url":"` + id.endpoint + `"`
		valid := `"` + id.name + `":{` + fields + credential + `}`
		document := func(row string) string {
			return `printf '[` + row + `]'` + strings.Repeat(` "$STAGED_COPY"`, strings.Count(row, "%s"))
		}
		info := `"info":{"mcp":{"servers":{` + valid + `}}}`
		good := `{"type":"document","path":"%s",` + info + `}`
		for label, c := range map[string]struct {
			body string
			want error
		}{
			"dropped entry":        {document(`{"type":"document","path":"%s","info":{}}`), errOpenCodeNoEntry},
			"no servers":           {document(`{"type":"document","path":"%s","info":{"mcp":{}}}`), errOpenCodeNoEntry},
			"wrong name":           {openCodeV2Rows(`"x` + id.name + `":{` + fields + credential + `}`), errOpenCodeNoEntry},
			"v1 layout":            {document(`{"type":"document","path":"%s","info":{"mcp":{` + valid + `}}}`), errOpenCodeNoEntry},
			"no rows":              {`echo '[]'`, errOpenCodeNoDocument},
			"directory row":        {document(`{"type":"directory","path":"%s",` + info + `}`), errOpenCodeNoDocument},
			"other document":       {document(`{"type":"document","path":"%s.other",` + info + `}`), errOpenCodeNoDocument},
			"base name":            {`printf '[{"type":"document","path":"%s",` + info + `}]' "${STAGED_COPY##*/}"`, errOpenCodeNoDocument},
			"symlink to the copy":  {`/bin/ln -s "$STAGED_COPY" "$HOME/link.json"; printf '[` + good + `]' "$HOME/link.json"`, errOpenCodeNoDocument},
			"path not a string":    {`echo '[{"type":"document","path":5,` + info + `}]'`, errOpenCodeNoDocument},
			"no path":              {`echo '[{"type":"document",` + info + `}]'`, errOpenCodeNoDocument},
			"duplicate document":   {document(good + `,` + good), errOpenCodeAmbiguous},
			"wrong type":           {openCodeV2Rows(`"` + id.name + `":{"type":"local","url":"` + id.endpoint + `"` + credential + `}`), errOpenCodeMismatch},
			"no type":              {openCodeV2Rows(`"` + id.name + `":{"url":"` + id.endpoint + `"` + credential + `}`), errOpenCodeMismatch},
			"wrong endpoint":       {openCodeV2Rows(`"` + id.name + `":{"type":"remote","url":"` + id.endpoint + `/"` + credential + `}`), errOpenCodeMismatch},
			"endpoint not string":  {openCodeV2Rows(`"` + id.name + `":{"type":"remote","url":["` + id.endpoint + `"]` + credential + `}`), errOpenCodeMismatch},
			"wrong OAuth field":    {openCodeV2Rows(`"` + id.name + `":{` + fields + wrongCredential + `}`), errOpenCodeMismatch},
			"null OAuth field":     {openCodeV2Rows(`"` + id.name + `":{` + fields + `,"oauth":null}`), errOpenCodeMismatch},
			"true OAuth field":     {openCodeV2Rows(`"` + id.name + `":{` + fields + `,"oauth":true}`), errOpenCodeMismatch},
			"string OAuth field":   {openCodeV2Rows(`"` + id.name + `":{` + fields + `,"oauth":"false"}`), errOpenCodeMismatch},
			"object OAuth field":   {openCodeV2Rows(`"` + id.name + `":{` + fields + `,"oauth":{}}`), errOpenCodeMismatch},
			"duplicate endpoint":   {openCodeV2Rows(`"` + id.name + `":{` + fields + credential + `,"url":"` + id.endpoint + `"}`), errOpenCodeMismatch},
			"null entry":           {openCodeV2Rows(`"` + id.name + `":null`), errOpenCodeMismatch},
			"duplicate entry":      {openCodeV2Rows(valid + `,` + valid), errOpenCodeMalformed},
			"null info":            {document(`{"type":"document","path":"%s","info":null}`), errOpenCodeMalformed},
			"duplicate row type":   {document(`{"type":"directory","type":"document","path":"%s",` + info + `}`), errOpenCodeMalformed},
			"row not an object":    {document(`5,` + good), errOpenCodeMalformed},
			"null":                 {`echo null`, errOpenCodeMalformed},
			"object":               {`echo '{"resolved-secret":1}'`, errOpenCodeMalformed},
			"no output":            {`:`, errOpenCodeMalformed},
			"not JSON":             {`echo resolved-secret`, errOpenCodeMalformed},
			"truncated":            {`printf '[` + good + `' "$STAGED_COPY"`, errOpenCodeMalformed},
			"trailing data":        {document(good) + `; echo resolved-secret`, errOpenCodeMalformed},
			"second array":         {document(good) + `; echo '[]'`, errOpenCodeMalformed},
			"over the limit":       {document(good) + `; while :; do echo '          resolved-secret          '; done`, errOpenCodeOutputSize},
			"over the limit, exit": {document(good) + `; /usr/bin/head -c 4194305 /dev/zero`, errOpenCodeOutputSize},
			"server start":         {`echo 'Error: Server process exited with code 1 resolved-secret' >&2; exit 1`, errOpenCodeServerStart},
			"managed service port": {`echo 'Error: Managed service port 49374 on 127.0.0.1 is already in use by resolved-secret' >&2; exit 1`, errOpenCodeServerStart},
			"nonzero exit":         {document(good) + `; exit 7`, errOpenCodeV2Failed},
			"v1 marker, exit":      {`echo 'ConfigInvalid resolved-secret' >&2; exit 1`, errOpenCodeV2Failed},
		} {
			t.Run(mode+"/"+label, func(t *testing.T) {
				checkOpenCodeRefusal(t, mode, openCodeV2Binary(t, c.body), c.want, ran)
			})
		}
	}
}

// TestOpenCodeMCPContainmentRefusal covers a host without containment, such
// as macOS or a Linux host that denies namespaces. Both majors refuse before
// a client starts, and the message suggests neither removing the client nor
// weaker host security.
func TestOpenCodeMCPContainmentRefusal(t *testing.T) {
	previous := openCodeContain
	openCodeContain = func(string, []string, *os.File) (*exec.Cmd, error) { return nil, errOpenCodeContainment }
	t.Cleanup(func() { openCodeContain = previous })
	for _, version := range []string{"1.19.6", "opencode v2.4.1"} {
		binary := openCodeValidationBinary(t, `echo "$*" >> "$ARGUMENT_LOG"; echo '`+version+`'`)
		checkOpenCodeRefusal(t, mcpAuthOAuth, binary, errOpenCodeContainment, "")
	}
	for _, forbidden := range []string{"remove", "uninstall", "disable", "sysctl", "sudo", "apparmor", "root"} {
		if strings.Contains(strings.ToLower(errOpenCodeContainment.Error()), forbidden) {
			t.Errorf("containment message suggests %q", forbidden)
		}
	}
	if !strings.Contains(errOpenCodeContainment.Error(), "Linux with user, PID, and network namespaces") {
		t.Errorf("containment message=%v", errOpenCodeContainment)
	}
}

// TestOpenCodeEnvironmentProtectsIsolation proves that a staged reference
// receives a placeholder and never replaces an isolation variable or a loader
// setting.
func TestOpenCodeEnvironmentProtectsIsolation(t *testing.T) {
	t.Setenv("AMBIENT_KEY", "resolved-secret")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "resolved-secret")
	t.Setenv("OPENCODE_API_KEY", "resolved-secret")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/resolved-secret")
	t.Setenv("CONVEYOR_MCP_TOKEN_FIXTURE", "resolved-secret")
	t.Setenv("LD_PRELOAD", "")
	reserved := []string{"HOME", "PWD", "PATH", "BASH_ENV", "TMPDIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_DISABLE_PROJECT_CONFIG", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "BUN_OPTIONS", mcpAddressEnv}
	values := []string{"AMBIENT_KEY", "OPENCODE_API_KEY", "NODE_AUTH_TOKEN", "CONVEYOR_MCP_TOKEN_FIXTURE", "UNSET_REFERENCE", mcpTokenEnv}
	var content strings.Builder
	for _, name := range append(append([]string{}, reserved...), values...) {
		fmt.Fprintf(&content, "{env:%s} ", name)
	}
	cmd := exec.Command("env")
	cmd.Env = openCodeEnvironment("/isolated", "/config/copy.json", []byte(content.String()))
	resolved := map[string]string{}
	for _, item := range cmd.Environ() {
		key, value, _ := strings.Cut(item, "=")
		resolved[key] = value
	}
	for key, want := range map[string]string{
		"HOME": "/isolated", "PWD": "/isolated", "PATH": os.Getenv("PATH"), "LD_PRELOAD": "",
		"XDG_CONFIG_HOME": "/isolated/config", "XDG_DATA_HOME": "/isolated/data", "XDG_STATE_HOME": "/isolated/state", "XDG_CACHE_HOME": "/isolated/cache",
		"OPENCODE_CONFIG": "/config/copy.json", "OPENCODE_DISABLE_PROJECT_CONFIG": "true", mcpAddressEnv: "https://conveyor.invalid/mcp",
	} {
		if resolved[key] != want {
			t.Errorf("%s=%q, want %q", key, resolved[key], want)
		}
	}
	for _, key := range values {
		if resolved[key] != openCodePlaceholder {
			t.Errorf("%s=%q, want the placeholder", key, resolved[key])
		}
	}
	for key, value := range resolved {
		if strings.Contains(value, "resolved-secret") || (value == openCodePlaceholder && openCodeReservedEnv(key)) {
			t.Errorf("%s=%q crossed the isolation", key, value)
		}
	}
}

// TestOpenCodeMCPInstalledBinary checks a locally installed OpenCode 1 or 2
// in the real containment. Opt in with an absolute binary path. The ordinary
// gate uses the deterministic subprocess fixtures above.
//
// CONVEYOR_TEST_OPENCODE_SERVICE_PORT names the port of an OpenCode service
// that the caller started outside the check. The test then proves that the
// same process holds the port before and after each install.
func TestOpenCodeMCPInstalledBinary(t *testing.T) {
	binary := os.Getenv("CONVEYOR_TEST_OPENCODE_BIN")
	if binary == "" {
		t.Skip("set CONVEYOR_TEST_OPENCODE_BIN for isolated native validation")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("CONVEYOR_TEST_OPENCODE_BIN must be absolute")
	}
	useOpenCodeContainment(t)
	version, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	major := openCodeMajor(version)
	t.Logf("client opencode %s (major %s)", strings.TrimSpace(string(version)), major)
	if port := os.Getenv("CONVEYOR_TEST_OPENCODE_SERVICE_PORT"); port != "" {
		before := openCodePortOwner(t, port)
		t.Logf("service before: %s", before)
		t.Cleanup(func() {
			after := openCodePortOwner(t, port)
			t.Logf("service after: %s", after)
			if after != before {
				t.Errorf("service on port %s changed from %q to %q", port, before, after)
			}
		})
	}
	lookup := func(string) (string, error) { return binary, nil }
	command := map[string]string{"1": openCodeConfigCommand, "2": openCodeConfigCommandV2}[major]
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run("create/"+mode, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "opencode.json")
			id := testMCPModeIdentity(t, mode, "https://factory.example", "")
			result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "opencode", path: path}, id, false, true, lookup)
			if err != nil || result.status != "created" || result.validation != "validated with "+command+" (not a native connection check)" {
				t.Fatalf("native create=%+v err=%v", result, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(got, []byte("$schema")) || bytes.Contains(got, []byte(openCodePlaceholder)) {
				t.Fatalf("native loader changes or resolved values published: %s", got)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(home, ".conveyor-*")); len(leftovers) != 0 {
				t.Fatalf("staging leftovers: %v", leftovers)
			}
		})
	}
	// install runs the transaction on a prior file and returns the bytes after it.
	install := func(t *testing.T, prior string) (string, error) {
		path := filepath.Join(t.TempDir(), "prior.json")
		if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := reconcileMCPRegistrationWithLookPath(filepath.Dir(path), mcpInstallTarget{tool: "opencode", path: path}, "https://factory.example/mcp", false, true, lookup)
		if err != nil && !strings.Contains(err.Error(), path) {
			t.Fatalf("refusal names no path: %v", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(after), err
	}
	// OpenCode 1 up to 1.18.0 refuses an unknown top-level key, and 1.18.35
	// and 1.19.6 ignore it. So the check of a major 1 client must agree with
	// the client itself. OpenCode 2 drops the key and keeps the registration.
	t.Run("unknown top-level key", func(t *testing.T) {
		const prior = `{"unknown_top_level_key":true}`
		refuses := false
		if major == "1" {
			isolated := t.TempDir()
			config := filepath.Join(isolated, "config.json")
			if err := os.WriteFile(config, []byte(prior), 0o600); err != nil {
				t.Fatal(err)
			}
			control := exec.Command(binary, "debug", "config")
			control.Dir, control.Env = isolated, openCodeEnvironment(isolated, config, nil)
			var stderr openCodeValidationStderr
			control.Stderr = &stderr
			refuses = control.Run() != nil || stderr.invalid
			t.Logf("client refuses an unknown top-level key: %t", refuses)
		}
		after, err := install(t, prior)
		if refuses {
			if err == nil || after != prior {
				t.Fatalf("native invalid config accepted: err=%v after=%s", err, after)
			}
			return
		}
		if err != nil || !strings.Contains(after, "unknown_top_level_key") || !strings.Contains(after, "https://factory.example/mcp") {
			t.Fatalf("native ignored key refused: err=%v after=%s", err, after)
		}
	})
	if major == "1" {
		t.Run("invalid value", func(t *testing.T) {
			const prior = `{"model":5}`
			if after, err := install(t, prior); err == nil || after != prior {
				t.Fatalf("native invalid config accepted: err=%v after=%s", err, after)
			}
		})
	}
	if major != "2" {
		return
	}
	// The staged files of the transaction are valid, so the refusals read
	// staged files that OpenCode 2 drops or resolves to another value.
	id := testMCPOAuthIdentity(t, "https://factory.example", "conveyor")
	pat := testMCPModeIdentity(t, mcpAuthPAT, "https://factory.example", "conveyor")
	valid := `{"mcp":{"conveyor":{"type":"remote","url":"https://factory.example/mcp"}}}`
	for label, c := range map[string]struct {
		staged string
		id     mcpIdentity
		want   error
	}{
		"accepted":       {valid, id, nil},
		"number URL":     {`{"mcp":{"conveyor":{"type":"remote","url":5}}}`, id, errOpenCodeNoEntry},
		"malformed JSON": {`{"mcp":`, id, errOpenCodeNoDocument},
		"missing entry":  {`{"mcp":{"other":{"type":"remote","url":"https://factory.example/mcp"}}}`, id, errOpenCodeNoEntry},
		"wrong endpoint": {`{"mcp":{"conveyor":{"type":"remote","url":"https://other.example/mcp"}}}`, id, errOpenCodeMismatch},
		"local type":     {`{"mcp":{"conveyor":{"type":"local","command":["true"]}}}`, id, errOpenCodeMismatch},
		"OAuth as PAT":   {valid, pat, errOpenCodeMismatch},
		"PAT as OAuth":   {`{"mcp":{"conveyor":{"type":"remote","url":"https://factory.example/mcp","oauth":false}}}`, id, errOpenCodeMismatch},
	} {
		t.Run("staged "+label, func(t *testing.T) {
			staged := filepath.Join(t.TempDir(), "staged.json")
			if err := os.WriteFile(staged, []byte(c.staged), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := validateOpenCodeMCP(binary, staged, c.id.name, c.id, openCodeCheckTimeout); !errors.Is(err, c.want) {
				t.Fatalf("native check=%v, want %v", err, c.want)
			}
		})
	}
}

// antigravityListBinary writes an `agy` stub for the parser check. It checks
// the isolated home, reads no standard input, and prints a stdio row for each
// name whose bridge entry the staged copy holds and an http row for each name
// whose serverUrl entry it holds. extra runs after the rows.
func antigravityListBinary(t *testing.T, extra string, names ...string) string {
	t.Helper()
	body := `
[ "$*" = 'mcp list' ]
[ "$HOME" = "$PWD" ]
[ "$XDG_CONFIG_HOME" = "$HOME/config" ]
[ "${CONVEYOR_API_TOKEN-unset}" = unset ]
if read -r ignored; then exit 8; fi
staged="$HOME/.gemini/config/mcp_config.json"
[ -f "$staged" ]
echo 'NAME  TYPE  STATUS  COMMAND/URL'
`
	for _, name := range names {
		body += `if /usr/bin/grep -q '"` + name + `":{"args"' "$staged"; then echo '` + name + `  stdio  enabled  conveyor mcp stdio'; fi` + "\n"
		body += `if /usr/bin/grep -qE '"` + name + `":\{[^{}]*"serverUrl"' "$staged"; then echo '` + name + `  http  enabled  remote'; fi` + "\n"
	}
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+extra+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func onlyAgyOnPath(agy string) func(string) (string, error) {
	return func(name string) (string, error) {
		if name == "agy" {
			return agy, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestMCPAntigravityInstallCommandCreatesBridgeAndRerunIsUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONVEYOR_API_TOKEN", "ambient-secret")
	setMCPTestCredentials(t, "https://factory.example", "stored-secret")
	id := testMCPIdentity(t, "https://factory.example", "")
	agy := antigravityListBinary(t, "", id.name)
	path := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	run := func(args ...string) string {
		t.Helper()
		cmd := mcpInstallCmdWithLookPath(onlyAgyOnPath(agy))
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output.String())
		}
		return output.String()
	}
	output := run("--auth", "pat", "--tool", "antigravity")
	for _, want := range []string{
		"antigravity\tcreated\t" + path + "\n",
		"Registration: " + id.name + "\n",
		"Endpoint: https://factory.example/mcp\n",
		"Credential source: stored credential via the stdio bridge `conveyor mcp stdio`, which reads the credential file for each request\n",
		"Parser: accepted by agy mcp list (not a native connection check)\n",
		"Restart: restart the client to load this registration\n",
		"Native initialize/tools-list: not run; config acceptance is not a connection check\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"export ", "stored-secret", "ambient-secret", "CONVEYOR_MCP_TOKEN_", "auth token"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("output carries %q:\n%s", forbidden, output)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	for _, forbidden := range []string{"stored-secret", "ambient-secret", "CONVEYOR_MCP_TOKEN_", "Bearer"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("file carries %q:\n%s", forbidden, data)
		}
	}
	binary, _ := os.Executable()
	credentials, _ := localAuthConfigPath()
	var root struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &root); err != nil || len(root.Servers) != 1 {
		t.Fatalf("entries: %v %v", root.Servers, err)
	}
	entry := root.Servers[id.name]
	if len(entry) != 2 || entry["command"] != binary {
		t.Fatalf("entry: %v", entry)
	}
	args, _ := json.Marshal(entry["args"])
	wantArgs, _ := json.Marshal([]string{"mcp", "stdio", "--server", "https://factory.example", "--credentials-file", credentials})
	if !bytes.Equal(args, wantArgs) {
		t.Fatalf("args %s, want %s", args, wantArgs)
	}
	output = run("--auth", "pat", "--tool", "antigravity")
	after, _ := os.ReadFile(path)
	if !strings.Contains(output, "antigravity\tunchanged\t"+path+"\n") || !bytes.Equal(after, data) {
		t.Fatalf("rerun changed the file or status:\n%s", output)
	}
	output = run("--auth", "pat", "--tool", "antigravity", "--list")
	if !strings.Contains(output, "antigravity\tunchanged\t") || !strings.Contains(output, "Restart: not requested (read-only listing)") {
		t.Fatalf("list output:\n%s", output)
	}
}

func TestMCPAntigravityDetectionAndToolSelection(t *testing.T) {
	t.Run("detected", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		setMCPTestCredentials(t, "https://factory.example", "stored-secret")
		agy := antigravityListBinary(t, "", testMCPIdentity(t, "https://factory.example", "").name)
		cmd := mcpInstallCmdWithLookPath(onlyAgyOnPath(agy))
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs([]string{"--auth", "pat"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "antigravity\tcreated\t") || strings.Contains(output.String(), "unsupported") {
			t.Fatalf("detected agy was not registered:\n%s", output.String())
		}
	})
	t.Run("selected among detected tools", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
		setMCPTestCredentials(t, "https://factory.example", "stored-secret")
		agy := antigravityListBinary(t, "", testMCPIdentity(t, "https://factory.example", "").name)
		// Every other tool is detected, so --tool alone narrows the target.
		cmd := mcpInstallCmdWithLookPath(func(name string) (string, error) {
			if name == "agy" {
				return agy, nil
			}
			return "/tools/" + name, nil
		})
		cmd.SetOut(io.Discard)
		cmd.SetArgs([]string{"--auth", "pat", "--tool", "antigravity"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{".claude.json", ".codex", ".cursor", filepath.Join("config", "opencode")} {
			if _, err := os.Stat(filepath.Join(home, other)); !os.IsNotExist(err) {
				t.Fatalf("selected Antigravity install touched %s: %v", other, err)
			}
		}
	})
	t.Run("selected without agy", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		setMCPTestCredentials(t, "https://factory.example", "stored-secret")
		cmd := mcpInstallCmdWithLookPath(func(string) (string, error) { return "", exec.ErrNotFound })
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--tool", "antigravity"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not available on PATH") {
			t.Fatalf("selected Antigravity without agy: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
			t.Fatalf("refusal created files: %v", err)
		}
	})
}

func TestMCPAntigravityParserRefusalPublishesNothing(t *testing.T) {
	id := testMCPIdentity(t, "https://one.example", "")
	cases := map[string]string{
		"nonzero exit":             "exit 7",
		"no row":                   "exit 0",
		"name inside another name": "echo 'x" + id.name + "  stdio  enabled  conveyor'",
		"name in another column":   "echo 'other  stdio  enabled  /bin/" + id.name + "'",
		"http row":                 "echo '" + id.name + "  http  enabled  https://one.example/mcp'",
		"type before name":         "echo 'stdio  " + id.name + "  enabled'",
	}
	for label, behavior := range cases {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exists=%t", label, exists), func(t *testing.T) {
				home := t.TempDir()
				path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
				prior := []byte(`{"mcpServers":{"other":{"serverUrl":"https://other.example/mcp"}}}`)
				if exists {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, prior, 0o640); err != nil {
						t.Fatal(err)
					}
				}
				// The stub prints its rows only for the "no row" case, so
				// each refusal comes from the case's own output or exit.
				body := "echo 'parser-output-marker' >&2\n"
				if label == "nonzero exit" {
					body += "echo '" + id.name + "  stdio  enabled  conveyor'\n"
				}
				agy := antigravityListBinary(t, body+behavior)
				result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, false, true, onlyAgyOnPath(agy))
				if err == nil {
					t.Fatalf("parser refusal published: %+v", result)
				}
				if strings.Contains(err.Error(), "parser-output-marker") || !strings.Contains(err.Error(), "output withheld") {
					t.Fatalf("error does not withhold output: %v", err)
				}
				want := "agy mcp list did not list registration " + id.name + " as a stdio server"
				if label == "nonzero exit" {
					want = "agy mcp list rejected the staged registration"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal for the wrong reason: %v", err)
				}
				after, readErr := os.ReadFile(path)
				if exists {
					info, _ := os.Stat(path)
					if readErr != nil || !bytes.Equal(after, prior) || info.Mode().Perm() != 0o640 {
						t.Fatalf("refusal changed the prior file: %s %v", after, readErr)
					}
				} else if !os.IsNotExist(readErr) {
					t.Fatalf("refusal created a file: %v", readErr)
				}
				if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".conveyor-*")); len(leftovers) != 0 {
					t.Fatalf("staging leftovers: %v", leftovers)
				}
			})
		}
	}
}

func TestMCPAntigravityParserTimeoutAndMissingClient(t *testing.T) {
	home := t.TempDir()
	staged := filepath.Join(home, "staged.json")
	if err := os.WriteFile(staged, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agy := antigravityListBinary(t, "while :; do :; done")
	start := time.Now()
	err := validateAntigravityMCP(agy, staged, "conveyor", "stdio", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout=%v elapsed=%s", err, time.Since(start))
	}
	// A missing client reports the check skipped and still publishes, as
	// invariant 11 states for every client.
	id := testMCPIdentity(t, "https://one.example", "")
	path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
	result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, false, true, func(string) (string, error) { return "", exec.ErrNotFound })
	if err != nil || result.status != "created" || result.validation != "skipped: agy is not on PATH" {
		t.Fatalf("missing client result=%+v err=%v", result, err)
	}
}

func TestMCPAntigravityListRunsNoParserAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
	id := testMCPIdentity(t, "https://one.example", "")
	lookup := func(string) (string, error) { t.Fatal("list looked up the parser"); return "", nil }
	result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, false, false, lookup)
	if err != nil || result.status != "created" || result.validation != "not run (no write)" {
		t.Fatalf("list result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
		t.Fatalf("list wrote files: %v", err)
	}
}

func TestMCPAntigravityRefusesSymlinkedDestination(t *testing.T) {
	for _, kind := range []string{"file", "config directory", "gemini directory"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
			outside := t.TempDir()
			original := filepath.Join(outside, "mcp_config.json")
			if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			case "config directory":
				if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(home, ".gemini", "config")); err != nil {
					t.Fatal(err)
				}
			case "gemini directory":
				if err := os.MkdirAll(filepath.Join(outside, "config"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(home, ".gemini")); err != nil {
					t.Fatal(err)
				}
			}
			id := testMCPIdentity(t, "https://one.example", "")
			agy := antigravityListBinary(t, "", id.name)
			for _, write := range []bool{false, true} {
				_, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, true, write, onlyAgyOnPath(agy))
				if err == nil || !strings.Contains(err.Error(), "refusing") {
					t.Fatalf("write=%t symlinked destination accepted: %v", write, err)
				}
			}
			if got, _ := os.ReadFile(original); string(got) != "original" {
				t.Fatal("modified the symlink target")
			}
			if entries, _ := os.ReadDir(outside); len(entries) > 2 {
				t.Fatalf("wrote through the symlink: %v", entries)
			}
		})
	}
}

// A failed read of the existing file and a failed write of the parser's
// staged copy both stop the install before publication. A process that can
// read or write past file modes, such as one that holds CAP_DAC_OVERRIDE,
// cannot set up these failures, so the case skips there.
func TestMCPAntigravityReadAndStagingFailuresPublishNothing(t *testing.T) {
	t.Run("unreadable existing file", func(t *testing.T) {
		home := t.TempDir()
		path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		prior := []byte(`{"mcpServers":{"other":{"serverUrl":"https://other.example/mcp"}}}`)
		if err := os.WriteFile(path, prior, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if _, err := os.ReadFile(path); err == nil {
			t.Skip("this process can read a mode 000 file")
		}
		id := testMCPIdentity(t, "https://one.example", "")
		agy := antigravityListBinary(t, "", id.name)
		_, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, false, true, onlyAgyOnPath(agy))
		if err == nil || !strings.Contains(err.Error(), "read "+path) {
			t.Fatalf("unreadable file error=%v", err)
		}
		_ = os.Chmod(path, 0o600)
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, prior) {
			t.Fatalf("read failure changed the file: %s", after)
		}
	})
	t.Run("staged copy cannot be written", func(t *testing.T) {
		directory := t.TempDir()
		staged := filepath.Join(directory, "staged.json")
		if err := os.WriteFile(staged, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
		if probe, err := os.MkdirTemp(directory, "probe-*"); err == nil {
			_ = os.Remove(probe)
			t.Skip("this process can write to a mode 0500 directory")
		}
		agy := antigravityListBinary(t, "touch \"$HOME/../agy-ran\"", "conveyor")
		if err := validateAntigravityMCP(agy, staged, "conveyor", "stdio", 30*time.Second); err == nil {
			t.Fatal("parser check passed without an isolated copy")
		}
		if _, err := os.Stat(filepath.Join(directory, "agy-ran")); !os.IsNotExist(err) {
			t.Fatalf("agy ran without an isolated copy: %v", err)
		}
	})
}

// codexGetBinary writes a `codex` stub for the parser check. It checks the
// arguments `mcp get <name> --json` for one of the names, the isolated home,
// and empty standard input. By default it prints, as `codex mcp get --json`
// does, the url of the staged table and whether the table has
// http_headers_helper. A non-empty body replaces that output.
func codexGetBinary(t *testing.T, body string, names ...string) string {
	t.Helper()
	script := `
[ "$#" = 4 ]
[ "$1 $2 $4" = 'mcp get --json' ]
case "$3" in
`
	for _, name := range names {
		script += "'" + name + "') ;;\n"
	}
	script += `*) exit 5 ;;
esac
[ "$HOME" = "$PWD" ]
[ "$CODEX_HOME" = "$HOME/.codex" ]
[ "${CONVEYOR_API_TOKEN-unset}" = unset ]
if read -r ignored; then exit 8; fi
staged="$CODEX_HOME/config.toml"
[ -f "$staged" ]
`
	if body == "" {
		body = `block=$(/usr/bin/awk -v t="[mcp_servers.$3]" '$0==t{f=1;next} /^\[/{f=0} f{print}' "$staged")
url=$(printf '%s\n' "$block" | /usr/bin/sed -n "s/^url = ['\"]\(.*\)['\"]$/\1/p")
[ -n "$url" ]
helper=''
if printf '%s\n' "$block" | /usr/bin/grep -q '^http_headers_helper = '; then helper=',"http_headers_helper":"present"'; fi
printf '{"name":"%s","enabled":true,"transport":{"type":"streamable_http","url":"%s"%s}}\n' "$3" "$url" "$helper"`
	}
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// claudeGetBinary writes a `claude` stub. It checks the arguments
// `mcp get <name>`, the isolated home, and that the staged entry has a
// headersHelper only under PAT. It also checks the barriers against a
// connection: the entry is disabled for the isolated directory, that
// directory is a git root, and every proxy is closed. body runs last and
// sets the result. An empty body reports the disabled state, as Claude does.
func claudeGetBinary(t *testing.T, mode, body string, names ...string) string {
	t.Helper()
	script := `
[ "$#" = 3 ]
[ "$1 $2" = 'mcp get' ]
case "$3" in
`
	for _, name := range names {
		script += "'" + name + "') ;;\n"
	}
	script += `*) exit 5 ;;
esac
[ "$HOME" = "$PWD" ]
[ "${CONVEYOR_API_TOKEN-unset}" = unset ]
if read -r ignored; then exit 8; fi
staged="$HOME/.claude.json"
[ -f "$staged" ]
/usr/bin/grep -qF "\"$PWD\":{\"disabledMcpServers\":[\"$3\"]}" "$staged"
[ -d "$PWD/.git" ]
for proxy in "$HTTP_PROXY" "$HTTPS_PROXY" "$ALL_PROXY" "$http_proxy" "$https_proxy" "$all_proxy"; do [ "$proxy" = 'http://127.0.0.1:9' ]; done
`
	if body == "" {
		body = "printf '%s:\\n  Status: Disabled for this project (re-enable via /mcp)\\n' \"$3\""
	}
	switch mode {
	case mcpAuthOAuth:
		script += "if /usr/bin/grep -q headersHelper \"$staged\"; then exit 6; fi\n"
	case mcpAuthPAT:
		script += "/usr/bin/grep -q headersHelper \"$staged\"\n"
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// openCodeModeBinary writes an `opencode` stub that checks `debug config`
// and the staged entry's credential fields for the mode.
func openCodeModeBinary(t *testing.T, mode, body string) string {
	t.Helper()
	script := `[ "$*" = 'debug config' ]
[ -f "$OPENCODE_CONFIG" ]
`
	switch mode {
	case mcpAuthOAuth:
		script += `if /usr/bin/grep -q -e '{env:' -e '"oauth":false' "$OPENCODE_CONFIG"; then exit 6; fi
/usr/bin/grep -q '"type":"remote"' "$OPENCODE_CONFIG"
`
	case mcpAuthPAT:
		script += `/usr/bin/grep -q '{env:CONVEYOR_MCP_TOKEN_' "$OPENCODE_CONFIG"
/usr/bin/grep -q '"oauth":false' "$OPENCODE_CONFIG"
`
	}
	return openCodeVersionBinary(t, "1.19.6", script+body)
}

// mcpModeLookup returns parser stubs for every client that accept the staged
// registrations of the names under the mode.
func mcpModeLookup(t *testing.T, mode string, names ...string) func(string) (string, error) {
	t.Helper()
	stubs := map[string]string{
		"codex":    codexGetBinary(t, "", names...),
		"claude":   claudeGetBinary(t, mode, "", names...),
		"opencode": openCodeModeBinary(t, mode, ""),
		"agy":      antigravityListBinary(t, "", names...),
	}
	return func(name string) (string, error) {
		if stub, ok := stubs[name]; ok {
			return stub, nil
		}
		return "/tools/" + name, nil
	}
}

func runMCPInstall(t *testing.T, lookup func(string) (string, error), args ...string) (string, error) {
	t.Helper()
	cmd := mcpInstallCmdWithLookPath(lookup)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}

// setMCPTestConfigRoot points the credential file at an empty directory and
// returns the credential file path, which does not exist yet.
func setMCPTestConfigRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	previous := userConfigDir
	userConfigDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { userConfigDir = previous })
	return filepath.Join(root, "conveyor", "credentials.json")
}

func setMCPTestServer(t *testing.T, server string) {
	t.Helper()
	serverFlag, serverFlagExplicit = server, server != ""
	t.Cleanup(func() { serverFlag, serverFlagExplicit = "", false })
}

// TestMCPInstallAuthFlagRefusesOtherValuesBeforeWork covers
// component-cli-onboarding invariant 6: --auth accepts only oauth and pat.
// Each other spelling refuses before tool detection, server selection, or a
// write.
func TestMCPInstallAuthFlagRefusesOtherValuesBeforeWork(t *testing.T) {
	for _, value := range []string{"", " ", "OAuth", "OAUTH", "PAT", "Pat", " pat", "oauth ", "oauth2", "token", "none", "bearer", "oauth,pat"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(mcpAddressEnv, "")
			credentials := setMCPTestConfigRoot(t)
			setMCPTestServer(t, "https://factory.example")
			lookup := func(name string) (string, error) {
				t.Fatalf("--auth %q looked up %s before refusal", value, name)
				return "", nil
			}
			for _, tool := range [][]string{{}, {"--tool", "cursor"}} {
				_, err := runMCPInstall(t, lookup, append([]string{"--auth", value}, tool...)...)
				if err == nil || err.Error() != "--auth must be oauth or pat" {
					t.Fatalf("--auth %q error=%v", value, err)
				}
			}
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Fatalf("refusal wrote %v", entries)
			}
			if _, err := os.Stat(credentials); !os.IsNotExist(err) {
				t.Fatalf("refusal touched the credential file: %v", err)
			}
		})
	}
}

// TestMCPInstallDefaultAndExplicitOAuthMatchAllClients covers req-mcp-oauth
// AC-5.1 and AC-5.3 for all five clients: no --auth and --auth oauth write
// the same credential-free registration from --server without any credential
// file, print no export line, and name the client's sign-in step.
func TestMCPInstallDefaultAndExplicitOAuthMatchAllClients(t *testing.T) {
	signIn := map[string]string{
		"codex":       "Sign-in: run `codex mcp login %s`\n",
		"claude":      "Sign-in: run `claude mcp login %s`\n",
		"opencode":    "Sign-in: run `opencode mcp auth %s`\n",
		"cursor":      "Sign-in: use the Connect control for %s in the Cursor MCP settings\n",
		"antigravity": "Sign-in: use the Authenticate control for %s in Antigravity, or run /mcp\n",
	}
	validation := map[string]string{
		"codex":       "Parser: accepted by codex mcp get (not a native connection check)\n",
		"claude":      "Parser: accepted by claude mcp get (not a native connection check)\n",
		"opencode":    "Parser: validated with opencode debug config (not a native connection check)\n",
		"cursor":      "Parser: skipped: cursor has no parser check\n",
		"antigravity": "Parser: accepted by agy mcp list (not a native connection check)\n",
	}
	for _, tool := range mcpAuthTools {
		t.Run(tool, func(t *testing.T) {
			id := testMCPOAuthIdentity(t, "https://factory.example", "")
			var files [][]byte
			for _, args := range [][]string{{"--tool", tool}, {"--auth", "oauth", "--tool", tool}} {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
				t.Setenv(mcpAddressEnv, "https://ignored.example")
				t.Setenv(mcpTokenEnv, "ambient-secret")
				credentials := setMCPTestConfigRoot(t)
				setMCPTestServer(t, "HTTPS://Factory.Example/mcp/")
				output, err := runMCPInstall(t, mcpModeLookup(t, mcpAuthOAuth, id.name), args...)
				if err != nil {
					t.Fatalf("%v: %v\n%s", args, err, output)
				}
				path := mcpTargets(home, []skillTool{{name: tool}})[0].path
				for _, want := range []string{
					tool + "\tcreated\t" + path + "\n",
					"Registration: " + id.name + "\n",
					"Endpoint: https://factory.example/mcp\n",
					"Credential source: OAuth 2.1 sign-in in the client. The registration holds no token.\n",
					fmt.Sprintf(signIn[tool], id.name),
					validation[tool],
					"Restart: restart the client to load this registration\n",
					"Native initialize/tools-list: not run; config acceptance is not a connection check\n",
				} {
					if !strings.Contains(output, want) {
						t.Fatalf("%v: output lacks %q:\n%s", args, want, output)
					}
				}
				for _, forbidden := range []string{"export ", "ambient-secret", "CONVEYOR_MCP_TOKEN_", "auth token", "stored credential"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("%v: output carries %q:\n%s", args, forbidden, output)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				checkMCPModeEntry(t, tool, data, id.name, id, nil)
				if bytes.Contains(data, []byte("ambient-secret")) || bytes.Contains(data, []byte("ignored.example")) {
					t.Fatalf("registration carries ambient values:\n%s", data)
				}
				if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
					t.Fatalf("mode %o", info.Mode().Perm())
				}
				if _, err := os.Stat(credentials); !os.IsNotExist(err) {
					t.Fatalf("OAuth install created or touched the credential file: %v", err)
				}
				files = append(files, data)
				output, err = runMCPInstall(t, mcpModeLookup(t, mcpAuthOAuth, id.name), args...)
				if after, _ := os.ReadFile(path); err != nil || !strings.Contains(output, tool+"\tunchanged\t") || !bytes.Equal(after, data) {
					t.Fatalf("rerun changed the file or status: %v\n%s", err, output)
				}
			}
			if !bytes.Equal(files[0], files[1]) {
				t.Fatalf("default and explicit OAuth differ:\n%s\n%s", files[0], files[1])
			}
		})
	}
}

// TestMCPInstallServerSelectionBothModes covers component-cli-onboarding
// invariant 7 and req-mcp-oauth AC-5.1, AC-5.2: flag precedence, environment
// selection without a stored token, the sole stored server, normalization,
// and refusal before any write for invalid, absent, or ambiguous selection.
func TestMCPInstallServerSelectionBothModes(t *testing.T) {
	type selection struct {
		flag, env, config, mode, want, err string
	}
	cases := map[string]selection{
		"oauth flag without credential file":            {flag: "https://factory.example", config: "none", mode: "oauth", want: "https://factory.example"},
		"oauth flag with an unreadable credential file": {flag: "https://factory.example", config: "malformed", mode: "oauth", want: "https://factory.example"},
		"pat flag with an unreadable credential file":   {flag: "https://factory.example", config: "malformed", mode: "pat", err: "parse "},
		"oauth environment without credential":          {env: "https://env.example", config: "none", mode: "oauth", want: "https://env.example"},
		"oauth environment normalized":                  {env: " HTTPS://Env.Example:443/base/mcp/ ", config: "none", mode: "oauth", want: "https://env.example:443/base"},
		"oauth flag normalized":                         {flag: "HTTPS://Factory.Example/mcp", config: "none", mode: "oauth", want: "https://factory.example"},
		"oauth flag before environment":                 {flag: "https://factory.example", env: "https://env.example", config: "none", mode: "oauth", want: "https://factory.example"},
		"pat flag before environment":                   {flag: "https://factory.example", env: "https://env.example", config: "two", mode: "pat", want: "https://factory.example"},
		"oauth sole stored server without token":        {config: "sole without token", mode: "oauth", want: "https://stored.example"},
		"pat sole stored server without token":          {config: "sole without token", mode: "pat", err: "no stored Conveyor credential"},
		"oauth sole stored server with token":           {config: "sole with token", mode: "oauth", want: "https://stored.example"},
		"pat sole stored server with token":             {config: "sole with token", mode: "pat", want: "https://stored.example"},
		"pat environment without stored token":          {env: "https://env.example", config: "sole with token", mode: "pat", err: "no stored credential for https://env.example"},
		"oauth two stored servers":                      {config: "two", mode: "oauth", err: "multiple stored Conveyor servers"},
		"pat two stored servers":                        {config: "two", mode: "pat", err: "multiple stored Conveyor servers"},
		"oauth no server":                               {config: "none", mode: "oauth", err: "no Conveyor server selected"},
		"pat no server":                                 {config: "none", mode: "pat", err: "no stored Conveyor credential"},
		"oauth invalid flag":                            {flag: "ftp://factory.example", config: "none", mode: "oauth", err: "http or https"},
		"pat invalid flag":                              {flag: "ftp://factory.example", config: "two", mode: "pat", err: "http or https"},
		"oauth blank flag":                              {flag: "   ", config: "sole with token", mode: "oauth", err: "server URL is required"},
		"oauth invalid environment":                     {env: "factory.example", config: "none", mode: "oauth", err: "absolute http or https URL"},
		"oauth flag with credentials in the URL":        {flag: "https://user:secret@factory.example", config: "none", mode: "oauth", err: "must not contain credentials"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(mcpAddressEnv, c.env)
			credentials := setMCPTestConfigRoot(t)
			switch c.config {
			case "malformed":
				if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(credentials, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "sole without token":
				setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://stored.example": {Workspace: "demo"}}})
			case "sole with token":
				setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://stored.example": {Token: "stored-secret"}}})
			case "two":
				setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://factory.example": {Token: "first-secret"}, "https://stored.example": {Token: "second-secret"}}})
			}
			credentials, _ = localAuthConfigPath()
			before, _ := os.ReadFile(credentials)
			if c.flag != "" {
				setMCPTestServer(t, c.flag)
			} else {
				setMCPTestServer(t, "")
			}
			output, err := runMCPInstall(t, mcpLegacyToolsOnPath, "--auth", c.mode, "--tool", "cursor")
			path := filepath.Join(home, ".cursor", "mcp.json")
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("error=%v, want %q\n%s", err, c.err, output)
				}
				if _, statErr := os.Stat(filepath.Join(home, ".cursor")); !os.IsNotExist(statErr) {
					t.Fatalf("refusal wrote the registration: %v", statErr)
				}
			} else {
				if err != nil {
					t.Fatalf("%v\n%s", err, output)
				}
				data, _ := os.ReadFile(path)
				id := testMCPModeIdentity(t, c.mode, c.want, "")
				if c.mode == mcpAuthPAT {
					binary, _ := os.Executable()
					id, _ = newMCPIdentity(c.want, "", binary, credentials)
				}
				checkMCPModeEntry(t, "cursor", data, id.name, id, nil)
				if !strings.Contains(output, "Endpoint: "+c.want+"/mcp\n") {
					t.Fatalf("output endpoint:\n%s", output)
				}
			}
			after, _ := os.ReadFile(credentials)
			if !bytes.Equal(before, after) {
				t.Fatal("the install changed the credential file")
			}
			for _, secret := range []string{"stored-secret", "first-secret", "second-secret", "secret@"} {
				if strings.Contains(output, secret) {
					t.Fatalf("output carries %q", secret)
				}
			}
		})
	}
}

// TestMCPInstallSwitchesModesAllClients covers req-mcp-oauth AC-5.3 through
// the command for all five clients. An installed conveyor executable wrote
// the first PAT registration. The default OAuth install then removes its
// helper, header reference, or bridge. --auth pat writes them again for the
// running test executable, whose file name is not conveyor, and the next
// OAuth install removes them again with no stored credential. Each switch
// reports refreshed and keeps the one entry, and only the PAT output names
// the PAT credential source (component-cli-onboarding invariant 9).
func TestMCPInstallSwitchesModesAllClients(t *testing.T) {
	for _, tool := range mcpAuthTools {
		t.Run(tool, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			setMCPTestCredentials(t, "https://factory.example", "stored-secret")
			credentials, _ := localAuthConfigPath()
			installed, err := newMCPIdentity("https://factory.example", "", "/usr/local/bin/conveyor", credentials)
			if err != nil {
				t.Fatal(err)
			}
			path := mcpTargets(home, []skillTool{{name: tool}})[0].path
			data, _, _, err := testMCPReconcile(tool, nil, installed, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			binary, _ := os.Executable()
			pat, err := newMCPIdentity("https://factory.example", "", binary, credentials)
			if err != nil {
				t.Fatal(err)
			}
			oauth := testMCPOAuthIdentity(t, "https://factory.example", "")
			for step, mode := range []string{mcpAuthOAuth, mcpAuthPAT, mcpAuthOAuth} {
				args := []string{"--tool", tool}
				id := oauth
				if mode == mcpAuthPAT {
					args = append([]string{"--auth", "pat"}, args...)
					id = pat
				}
				if step == 2 {
					// OAuth needs no stored credential to switch the entry
					// that this executable wrote.
					setMCPTestCredentials(t, "https://other.example", "other-secret")
					setMCPTestServer(t, "https://factory.example")
				}
				output, err := runMCPInstall(t, mcpModeLookup(t, mode, id.name), args...)
				if err != nil {
					t.Fatalf("step %d %s: %v\n%s", step, mode, err, output)
				}
				if !strings.Contains(output, tool+"\trefreshed\t"+path+"\n") {
					t.Fatalf("step %d status:\n%s", step, output)
				}
				data, _ := os.ReadFile(path)
				checkMCPModeEntry(t, tool, data, id.name, id, nil)
				if len(testMCPEntries(t, tool, data)) != 1 {
					t.Fatalf("step %d entries:\n%s", step, data)
				}
				if strings.Contains(output, "stored-secret") || bytes.Contains(data, []byte("stored-secret")) || bytes.Contains(data, []byte("/usr/local/bin/conveyor")) {
					t.Fatalf("step %d: token or old executable remains", step)
				}
				if mode == mcpAuthOAuth && bytes.Contains(data, []byte(binary)) {
					t.Fatalf("step %d: the OAuth entry names the executable:\n%s", step, data)
				}
				hasExport := strings.Contains(output, "export ")
				if hasExport != (mode == mcpAuthPAT && (tool == "cursor" || tool == "opencode")) {
					t.Fatalf("step %d export line=%t:\n%s", step, hasExport, output)
				}
				if (mode == mcpAuthOAuth) != strings.Contains(output, "Sign-in: ") {
					t.Fatalf("step %d sign-in line:\n%s", step, output)
				}
			}
		})
	}
}

// TestMCPParserChecksBothModesAllClients covers component-cli-onboarding
// invariant 11 for all five clients in both modes: the accepted check, each
// refusal with no publication, a missing client, Cursor's skip, and --list
// with no write and no parser.
func TestMCPParserChecksBothModesAllClients(t *testing.T) {
	type parserCase struct {
		stub    func(t *testing.T, mode string, id mcpIdentity) string
		refusal string
	}
	accepted := map[string]string{
		"codex":       "accepted by codex mcp get (not a native connection check)",
		"claude":      "accepted by claude mcp get (not a native connection check)",
		"opencode":    "validated with opencode debug config (not a native connection check)",
		"cursor":      "skipped: cursor has no parser check",
		"antigravity": "accepted by agy mcp list (not a native connection check)",
	}
	missing := map[string]string{
		"codex":       "skipped: codex is not on PATH",
		"claude":      "skipped: claude is not on PATH",
		"opencode":    "validation skipped: opencode is not on PATH",
		"cursor":      "skipped: cursor has no parser check",
		"antigravity": "skipped: agy is not on PATH",
	}
	binaryName := map[string]string{"codex": "codex", "claude": "claude", "opencode": "opencode", "antigravity": "agy"}
	cases := map[string]map[string]parserCase{
		"codex": {
			"accepted": {stub: func(t *testing.T, mode string, id mcpIdentity) string { return codexGetBinary(t, "", id.name) }},
			"nonzero exit": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, "echo parser-output-marker; exit 1", id.name)
			}, refusal: "codex parser rejected the staged registration"},
			"wrong name": {stub: func(t *testing.T, mode string, id mcpIdentity) string { return codexGetBinary(t, "", "other-name") }, refusal: "codex parser rejected the staged registration"},
			"wrong URL": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"transport":{"url":"https://wrong.example/mcp","http_headers_helper":"present"},"marker":"parser-output-marker"}'`, id.name)
			}},
			"unrelated matching URL": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"url":"`+id.endpoint+`","transport":{"url":"https://wrong.example/mcp","http_headers":{"url":"`+id.endpoint+`"},"http_headers_helper":"present"},"marker":"parser-output-marker"}'`, id.name)
			}},
			"URL outside transport": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"url":"`+id.endpoint+`","transport":{"http_headers":{"url":"`+id.endpoint+`"},"http_headers_helper":"present"}}'`, id.name)
			}},
			"no URL": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"transport":{"http_headers_helper":"present"}}'`, id.name)
			}},
			"no helper": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"transport":{"url":"`+id.endpoint+`"}}'`, id.name)
			}},
			"null helper": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, `echo '{"transport":{"url":"`+id.endpoint+`","http_headers_helper":null}}'`, id.name)
			}},
			"not JSON": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return codexGetBinary(t, "echo parser-output-marker", id.name)
			}},
		},
		"claude": {
			"accepted": {stub: func(t *testing.T, mode string, id mcpIdentity) string { return claudeGetBinary(t, mode, "", id.name) }},
			"nonzero exit": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return claudeGetBinary(t, mode, "echo parser-output-marker; exit 1", id.name)
			}, refusal: "claude parser rejected the staged registration"},
			"wrong name": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return claudeGetBinary(t, mode, "", "other-name")
			}, refusal: "claude parser rejected the staged registration"},
			"health check ran": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return claudeGetBinary(t, mode, "printf 'parser-output-marker\\n  Status: Failed to connect\\n'", id.name)
			}, refusal: "claude mcp get did not report the registration as disabled"},
			"no status": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return claudeGetBinary(t, mode, "echo 'Disabled for this project parser-output-marker'", id.name)
			}, refusal: "claude mcp get did not report the registration as disabled"},
		},
		"opencode": {
			"accepted":     {stub: func(t *testing.T, mode string, id mcpIdentity) string { return openCodeModeBinary(t, mode, "") }},
			"nonzero exit": {stub: func(t *testing.T, mode string, id mcpIdentity) string { return openCodeModeBinary(t, mode, "exit 7") }, refusal: "opencode debug config failed"},
			"invalid key": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return openCodeModeBinary(t, mode, "echo 'Unrecognized key parser-output-marker' >&2")
			}, refusal: "opencode debug config reported an invalid configuration"},
		},
		"antigravity": {
			"accepted": {stub: func(t *testing.T, mode string, id mcpIdentity) string { return antigravityListBinary(t, "", id.name) }},
			"nonzero exit": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return antigravityListBinary(t, "exit 7", id.name)
			}, refusal: "agy mcp list rejected the staged registration"},
			"no row": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return antigravityListBinary(t, "echo parser-output-marker >&2")
			}},
			"stdio row": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return antigravityListBinary(t, "echo '"+id.name+"  stdio  enabled  conveyor mcp stdio'")
			}},
			"http row": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return antigravityListBinary(t, "echo '"+id.name+"  http  enabled  remote'")
			}},
			"name inside another name": {stub: func(t *testing.T, mode string, id mcpIdentity) string {
				return antigravityListBinary(t, "echo 'x"+id.name+"  http  enabled'; echo 'x"+id.name+"  stdio  enabled'")
			}},
		},
		"cursor": {"no parser": {}},
	}
	for _, tool := range mcpAuthTools {
		for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
			id := testMCPModeIdentity(t, mode, "https://one.example", "")
			for label, c := range cases[tool] {
				// Decide whether this case refuses under this mode.
				refusal := c.refusal
				switch tool + "/" + label {
				case "codex/wrong URL", "codex/no URL", "codex/not JSON", "codex/unrelated matching URL", "codex/URL outside transport":
					if mode == mcpAuthOAuth {
						refusal = "Codex parser did not report the registered URL"
					} else if label == "not JSON" {
						refusal = "Codex parser did not expose http_headers_helper"
					}
				case "codex/no helper", "codex/null helper":
					if mode == mcpAuthPAT {
						refusal = "Codex parser did not expose http_headers_helper"
					}
				case "antigravity/no row", "antigravity/name inside another name":
					refusal = "did not list registration " + id.name + " as a " + map[string]string{mcpAuthOAuth: "http", mcpAuthPAT: "stdio"}[mode] + " server"
				case "antigravity/stdio row":
					if mode == mcpAuthOAuth {
						refusal = "did not list registration " + id.name + " as a http server"
					}
				case "antigravity/http row":
					if mode == mcpAuthPAT {
						refusal = "did not list registration " + id.name + " as a stdio server"
					}
				}
				for _, exists := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/exists=%t", tool, mode, label, exists), func(t *testing.T) {
						home := t.TempDir()
						path := mcpTargets(home, []skillTool{{name: tool}})[0].path
						if tool == "opencode" {
							path = filepath.Join(home, "opencode", "opencode.json")
						}
						prior := []byte(`{"theme":"dark"}`)
						if tool == "codex" {
							prior = []byte("model = 'x'\n")
						}
						if exists {
							if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(path, prior, 0o640); err != nil {
								t.Fatal(err)
							}
						}
						lookup := func(name string) (string, error) {
							if tool == "cursor" {
								t.Fatalf("cursor looked up %s", name)
							}
							if name != binaryName[tool] {
								return "", exec.ErrNotFound
							}
							return c.stub(t, mode, id), nil
						}
						result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: tool, path: path}, id, false, true, lookup)
						after, readErr := os.ReadFile(path)
						if refusal == "" {
							if err != nil || result.status != "created" || result.validation != accepted[tool] {
								t.Fatalf("result=%+v err=%v", result, err)
							}
							checkMCPModeEntry(t, tool, after, id.name, id, nil)
							return
						}
						if err == nil || !strings.Contains(err.Error(), refusal) {
							t.Fatalf("error=%v, want %q", err, refusal)
						}
						if strings.Contains(err.Error(), "parser-output-marker") {
							t.Fatalf("error carries parser output: %v", err)
						}
						if exists {
							info, _ := os.Stat(path)
							if readErr != nil || !bytes.Equal(after, prior) || info.Mode().Perm() != 0o640 {
								t.Fatalf("refusal changed the prior file: %s %v", after, readErr)
							}
						} else if !os.IsNotExist(readErr) {
							t.Fatalf("refusal created a file: %v", readErr)
						}
						if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".conveyor-*")); len(leftovers) != 0 {
							t.Fatalf("staging leftovers: %v", leftovers)
						}
					})
				}
			}
			t.Run(tool+"/"+mode+"/missing client", func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "config")
				result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: tool, path: path}, id, false, true, func(string) (string, error) { return "", exec.ErrNotFound })
				if err != nil || result.status != "created" || result.validation != missing[tool] {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			})
			t.Run(tool+"/"+mode+"/list", func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "config")
				lookup := func(name string) (string, error) { t.Fatalf("list looked up %s", name); return "", nil }
				result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: tool, path: path}, id, false, false, lookup)
				if err != nil || result.status != "created" || result.validation != "not run (no write)" {
					t.Fatalf("list result=%+v err=%v", result, err)
				}
				if entries, _ := os.ReadDir(home); len(entries) != 0 {
					t.Fatalf("list wrote %v", entries)
				}
			})
		}
	}
}

// TestMCPInstallServerFlagPresenceBothModes covers component-cli-onboarding
// invariant 7 through the real root command, so cobra parses --server as a
// person types it. A --server flag on the command line is the explicit
// source even when it is empty, blank, or invalid. It refuses before any
// write and never falls back to CONVEYOR_ADDR or a stored server. An empty
// or blank CONVEYOR_ADDR counts as unset. Each row runs in both modes.
func TestMCPInstallServerFlagPresenceBothModes(t *testing.T) {
	type row struct {
		flag               *string
		env                string
		stored             bool
		oauthWant, patWant string
		err                string
	}
	str := func(s string) *string { return &s }
	rows := map[string]row{
		"empty flag with valid environment":        {flag: str(""), env: "https://env.example", err: "server URL is required"},
		"empty flag with one stored server":        {flag: str(""), stored: true, err: "server URL is required"},
		"empty flag with both":                     {flag: str(""), env: "https://env.example", stored: true, err: "server URL is required"},
		"blank flag with valid environment":        {flag: str("   "), env: "https://env.example", err: "server URL is required"},
		"blank flag with one stored server":        {flag: str("   "), stored: true, err: "server URL is required"},
		"invalid scheme with valid environment":    {flag: str("ftp://factory.example"), env: "https://env.example", err: "http or https"},
		"invalid scheme with one stored server":    {flag: str("ftp://factory.example"), stored: true, err: "http or https"},
		"relative URL with valid environment":      {flag: str("factory.example"), env: "https://env.example", err: "absolute http or https URL"},
		"relative URL with one stored server":      {flag: str("factory.example"), stored: true, err: "absolute http or https URL"},
		"URL with a query with one stored server":  {flag: str("https://factory.example?x=1"), stored: true, err: "must not contain credentials, a query, or a fragment"},
		"empty environment with one stored server": {env: "", stored: true, oauthWant: "https://stored.example", patWant: "https://stored.example"},
		"blank environment with one stored server": {env: "   ", stored: true, oauthWant: "https://stored.example", patWant: "https://stored.example"},
		"valid flag with valid environment":        {flag: str("https://factory.example"), env: "https://env.example", oauthWant: "https://factory.example", err: "no stored credential for https://factory.example"},
		"valid flag with one stored server":        {flag: str("HTTPS://Factory.Example/mcp/"), stored: true, oauthWant: "https://factory.example", err: "no stored credential for https://factory.example"},
		"valid flag naming the stored server":      {flag: str("https://stored.example"), env: "https://env.example", stored: true, oauthWant: "https://stored.example", patWant: "https://stored.example"},
		"valid environment with one stored server": {env: "https://env.example/mcp", stored: true, oauthWant: "https://env.example", err: "no stored credential for https://env.example"},
		"one stored server only":                   {stored: true, oauthWant: "https://stored.example", patWant: "https://stored.example"},
	}
	for label, r := range rows {
		for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
			t.Run(label+"/"+mode, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
				// No client binary is on PATH, so the parser check is skipped
				// and --tool opencode needs no binary.
				t.Setenv("PATH", t.TempDir())
				t.Setenv(mcpAddressEnv, r.env)
				credentials := setMCPTestConfigRoot(t)
				if r.stored {
					setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://stored.example": {Token: "stored-secret"}}})
					credentials, _ = localAuthConfigPath()
				}
				before, _ := os.ReadFile(credentials)
				t.Cleanup(func() { serverFlag, serverFlagExplicit = "", false })
				args := []string{}
				if r.flag != nil {
					args = append(args, "--server="+*r.flag)
				}
				args = append(args, "mcp", "install", "--auth", mode, "--tool", "opencode")
				root := newRootCmd()
				var output bytes.Buffer
				root.SetOut(&output)
				root.SetErr(&output)
				root.SetArgs(args)
				err := root.Execute()
				want := r.oauthWant
				if mode == mcpAuthPAT {
					want = r.patWant
				}
				path := filepath.Join(home, "config", "opencode", "opencode.json")
				if want == "" {
					if err == nil || !strings.Contains(err.Error(), r.err) {
						t.Fatalf("%v: error=%v, want %q\n%s", args, err, r.err, output.String())
					}
					if _, statErr := os.Stat(filepath.Join(home, "config")); !os.IsNotExist(statErr) {
						t.Fatalf("refusal wrote the registration: %v", statErr)
					}
				} else {
					if err != nil {
						t.Fatalf("%v: %v\n%s", args, err, output.String())
					}
					data, _ := os.ReadFile(path)
					if !bytes.Contains(data, []byte(`"url":"`+want+`/mcp"`)) || !strings.Contains(output.String(), "Endpoint: "+want+"/mcp\n") {
						t.Fatalf("%v: want %s:\n%s\n%s", args, want, data, output.String())
					}
				}
				if after, _ := os.ReadFile(credentials); !bytes.Equal(before, after) {
					t.Fatal("the install changed the credential file")
				}
				if strings.Contains(output.String(), "stored-secret") {
					t.Fatal("output carries the stored token")
				}
			})
		}
	}
}

// TestMCPInstallKeepsCanonicalBaseAllClients covers component-cli-onboarding
// invariant 7: the selected server is normalized once, so a base that ends
// in an MCP path segment keeps its endpoint for every client, in both modes,
// and from each source. A rerun is a no-op, and a switch of modes keeps the
// endpoint. The PAT helper and bridge name a --server value that their own
// normalization turns into the same base.
func TestMCPInstallKeepsCanonicalBaseAllClients(t *testing.T) {
	bases := map[string]string{
		"https://example.test":              "https://example.test",
		"https://example.test/mcp":          "https://example.test",
		"https://example.test/team/mcp/":    "https://example.test/team",
		"https://example.test/team/mcp/mcp": "https://example.test/team/mcp",
		"https://example.test/mcp/mcp":      "https://example.test/mcp",
		"https://example.test/mcp/mcp/mcp/": "https://example.test/mcp/mcp",
	}
	for _, tool := range mcpAuthTools {
		for input, base := range bases {
			for _, source := range []string{"flag", "environment", "stored"} {
				t.Run(tool+"/"+source+"/"+strings.TrimPrefix(input, "https://"), func(t *testing.T) {
					home := t.TempDir()
					t.Setenv("HOME", home)
					t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
					// The credential file key is the base normalized once.
					setMCPTestCredentials(t, base, "stored-secret")
					switch source {
					case "flag":
						setMCPTestServer(t, input)
					case "environment":
						t.Setenv(mcpAddressEnv, input)
					}
					path := mcpTargets(home, []skillTool{{name: tool}})[0].path
					binary, _ := os.Executable()
					credentials, _ := localAuthConfigPath()
					for step, mode := range []string{mcpAuthOAuth, mcpAuthPAT, mcpAuthPAT, mcpAuthOAuth} {
						id, err := newMCPOAuthIdentityForBase(base, "")
						if mode == mcpAuthPAT {
							id, err = newMCPPATIdentityForBase(base, "", binary, credentials)
						}
						if err != nil {
							t.Fatal(err)
						}
						if id.endpoint != base+"/mcp" {
							t.Fatalf("endpoint %s", id.endpoint)
						}
						if again, err := normalizeServerURL(id.serverArgument); err != nil || again != base {
							t.Fatalf("--server %s names %s, want %s", id.serverArgument, again, base)
						}
						output, err := runMCPInstall(t, mcpModeLookup(t, mode, id.name), "--auth", mode, "--tool", tool)
						if err != nil {
							t.Fatalf("step %d %s: %v", step, mode, err)
						}
						want := []string{"created", "refreshed", "unchanged", "refreshed"}[step]
						if !strings.Contains(output, tool+"\t"+want+"\t") || !strings.Contains(output, "Endpoint: "+base+"/mcp\n") {
							t.Fatalf("step %d %s output:\n%s", step, mode, output)
						}
						data, _ := os.ReadFile(path)
						if tool == "antigravity" && mode == mcpAuthPAT {
							entry := testAntigravityEntry(t, data, id.name)
							if server, ok := antigravityBridgeServer(entry); !ok || canonicalMCPEndpoint(server) != base+"/mcp" || antigravityEntryEndpoint(entry) != base+"/mcp" {
								t.Fatalf("the bridge names another endpoint: %v", entry)
							}
							continue
						}
						checkMCPModeEntry(t, tool, data, id.name, id, nil)
					}
				})
			}
		}
	}
}

// TestMCPInstallListMarksOAuthGuidanceAsPreview checks that a listing, which
// writes nothing, does not describe an installed OAuth registration.
func TestMCPInstallListMarksOAuthGuidanceAsPreview(t *testing.T) {
	for _, tool := range mcpAuthTools {
		t.Run(tool, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			setMCPTestCredentials(t, "https://factory.example", "stored-secret")
			lookup := mcpModeLookup(t, mcpAuthPAT, testMCPOAuthIdentity(t, "https://factory.example", "").name)
			if _, err := runMCPInstall(t, lookup, "--auth", "pat", "--tool", tool); err != nil {
				t.Fatal(err)
			}
			output, err := runMCPInstall(t, lookup, "--tool", tool, "--list")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Credential source (preview, applies after installation): OAuth 2.1", "Sign-in (preview, applies after installation): "} {
				if !strings.Contains(output, want) {
					t.Fatalf("listing lacks %q:\n%s", want, output)
				}
			}
			for _, forbidden := range []string{"The registration holds no token.", "\nSign-in: "} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("listing carries %q:\n%s", forbidden, output)
				}
			}
		})
	}
}

// TestMCPClaudeParserConfigOnlyAddsTheProject checks the isolated copy: it
// keeps every other project and setting, and it disables only the
// registration, only for the isolated directory.
func TestMCPClaudeParserConfigOnlyAddsTheProject(t *testing.T) {
	directory := t.TempDir()
	for _, staged := range []string{
		`{"theme" : "dark","mcpServers":{"one":{"type":"http","url":"https://one.example/mcp"}}}`,
		`{"projects" : {"/work":{"disabledMcpServers":["two"],"allowedTools":[]}},"mcpServers":{}}`,
	} {
		got, err := claudeParserConfig([]byte(staged), directory, "one")
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		_ = json.Unmarshal([]byte(staged), &before)
		if err := json.Unmarshal(got, &after); err != nil {
			t.Fatal(err)
		}
		projects := after["projects"].(map[string]any)
		entry, _ := json.Marshal(projects[directory])
		if string(entry) != `{"disabledMcpServers":["one"]}` {
			t.Fatalf("isolated project: %s", entry)
		}
		delete(projects, directory)
		if resolved, _ := filepath.EvalSymlinks(directory); resolved != directory {
			delete(projects, resolved)
		}
		if len(projects) == 0 {
			delete(after, "projects")
		}
		if fmt.Sprint(before) != fmt.Sprint(after) {
			t.Fatalf("copy changed other settings:\n%v\n%v", before, after)
		}
	}
	for _, staged := range []string{`{"projects":[]}`, `{"projects":"x"}`, `{"mcpServers":{`} {
		if _, err := claudeParserConfig([]byte(staged), directory, "one"); err == nil {
			t.Fatalf("accepted %s", staged)
		}
	}
}
