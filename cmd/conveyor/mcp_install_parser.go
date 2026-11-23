package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Parser acceptance is separate from runtime credential retrieval. Never run
// a native connection (or a credential helper) in the publication check.
// Under --auth oauth, Codex must report the registered URL. Under --auth pat,
// Codex must report http_headers_helper (component-cli-onboarding invariant
// 11; req-mcp-oauth AC-5.3).
//
// `codex mcp get --json` only prints the parsed entry. `claude mcp get`
// connects to an enabled entry and runs its headersHelper. It connects to no
// entry that is disabled for the project, so claudeParserConfig disables the
// entry in the isolated copy, for the isolated directory only. The staged
// bytes never change. The directory is its own git root, because Claude
// keys project settings by the git root. A closed proxy is a second barrier
// against a connection. Output that does not report the disabled state
// refuses, because the check cannot then be shown to be parser-only.
func validateMCPParser(binary, tool, staged, name string, id mcpIdentity) error {
	content, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	isolated, err := os.MkdirTemp(filepath.Dir(staged), ".conveyor-parser-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(isolated)
	configPath := filepath.Join(isolated, ".claude.json")
	args := []string{"mcp", "get", name}
	helperField := "headersHelper"
	if tool == "codex" {
		configPath = filepath.Join(isolated, ".codex", "config.toml")
		args = append(args, "--json")
		helperField = "http_headers_helper"
	}
	environment := mcpIsolatedEnvironment(isolated)
	if tool == "claude" {
		if content, err = claudeParserConfig(content, isolated, name); err != nil {
			return errors.New("claude parser check could not isolate the staged registration (output withheld)")
		}
		if err = os.Mkdir(filepath.Join(isolated, ".git"), 0o700); err != nil {
			return err
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			environment = append(environment, key+"=http://127.0.0.1:9")
		}
	}
	if err = os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(configPath, content, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = isolated
	cmd.Env = environment
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%s parser rejected the staged registration; upgrade the client or check its configuration (output withheld)", tool)
	}
	if tool != "codex" {
		if !claudeReportsDisabled(output.Bytes()) {
			return errors.New("claude mcp get did not report the registration as disabled for the check, so the check can contact the server (output withheld). Upgrade Claude Code")
		}
		return nil
	}
	var parsed map[string]any
	_ = json.Unmarshal(output.Bytes(), &parsed)
	// hasField reports whether the object, or an object inside it, holds the
	// key with a string value that accepts.
	var hasField func(map[string]any, string, func(string) bool) bool
	hasField = func(object map[string]any, key string, accepts func(string) bool) bool {
		if value, ok := object[key].(string); ok && accepts(value) {
			return true
		}
		for _, value := range object {
			if child, ok := value.(map[string]any); ok && hasField(child, key, accepts) {
				return true
			}
		}
		return false
	}
	if id.oauth() {
		// Codex reports the endpoint in transport.url. Another field named
		// url, such as a custom header, is not the endpoint.
		transport, _ := parsed["transport"].(map[string]any)
		if transport["url"] != id.endpoint {
			return errors.New("Codex parser did not report the registered URL (output withheld). Upgrade Codex or check its configuration")
		}
		return nil
	}
	if !hasField(parsed, helperField, func(value string) bool { return value != "" }) {
		return errors.New("Codex parser did not expose http_headers_helper; upgrade Codex before installing a saved-credential connection")
	}
	return nil
}

// claudeParserConfig returns the staged configuration with the registration
// disabled for the isolated directory. Claude keys a project by its resolved
// path, so both spellings are listed.
func claudeParserConfig(staged []byte, isolated, name string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(isolated)
	if err != nil {
		return nil, err
	}
	object, err := scanJSONObject(staged)
	if err != nil {
		return nil, err
	}
	projects := []byte("{}")
	if member, ok := object.member("projects"); ok {
		projects = staged[member.valueStart:member.valueEnd]
	}
	disabled, err := json.Marshal(map[string][]string{"disabledMcpServers": {name}})
	if err != nil {
		return nil, err
	}
	for _, directory := range []string{isolated, resolved} {
		if projects, err = setJSONObjectMember(projects, directory, disabled); err != nil {
			return nil, err
		}
	}
	return setJSONObjectMember(staged, "projects", projects)
}

func claudeReportsDisabled(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		status, ok := strings.CutPrefix(strings.TrimSpace(line), "Status:")
		if ok && strings.Contains(strings.ToLower(status), "disabled for this project") {
			return true
		}
	}
	return false
}

func mcpIsolatedEnvironment(home string) []string {
	result := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	for _, key := range []string{"TMPDIR", "SystemRoot", "SYSTEMROOT"} {
		if value := os.Getenv(key); strings.TrimSpace(value) != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}

// validateAntigravityMCP runs `agy mcp list` against a copy of the staged
// file in an isolated home before publication (component-cli-onboarding
// invariant 11). `agy mcp list` parses the file and starts no stdio server
// and no remote connection, so the check never runs the bridge or signs in.
// A nonzero exit, a timeout, or a list without a row of the given transport
// for the exact registration name refuses publication. The transport is
// http under --auth oauth and stdio under --auth pat. The client output is
// withheld, as in validateMCPParser.
func validateAntigravityMCP(binary, staged, name, transport string, timeout time.Duration) error {
	content, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	isolated, err := os.MkdirTemp(filepath.Dir(staged), ".conveyor-parser-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(isolated)
	configPath := filepath.Join(isolated, ".gemini", "config", "mcp_config.json")
	if err = os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(configPath, content, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "mcp", "list")
	cmd.Dir = isolated
	cmd.Env = mcpIsolatedEnvironment(isolated)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); ctx.Err() != nil {
		return errors.New("agy mcp list timed out (output withheld)")
	} else if err != nil {
		return errors.New("agy mcp list rejected the staged registration (output withheld). Upgrade agy or check its configuration")
	}
	if !antigravityListsTransport(output.Bytes(), name, transport) {
		return fmt.Errorf("agy mcp list did not list registration %s as a %s server (output withheld)", name, transport)
	}
	return nil
}

// antigravityListsTransport reports whether a row of `agy mcp list` output
// has the exact registration name in its NAME column and the transport in
// its TYPE column. A name that only contains the registration name does not
// match.
func antigravityListsTransport(output []byte, name, transport string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name && fields[1] == transport {
			return true
		}
	}
	return false
}
