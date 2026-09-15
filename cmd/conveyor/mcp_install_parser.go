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
func validateMCPParser(binary, tool, staged, name string) error {
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
	cmd.Env = mcpIsolatedEnvironment(isolated)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%s parser rejected the staged registration; upgrade the client or check its configuration (output withheld)", tool)
	}
	var parsed map[string]any
	_ = json.Unmarshal(output.Bytes(), &parsed)
	var hasHelper func(map[string]any) bool
	hasHelper = func(object map[string]any) bool {
		if value, ok := object[helperField].(string); ok && value != "" {
			return true
		}
		for _, value := range object {
			if child, ok := value.(map[string]any); ok && hasHelper(child) {
				return true
			}
		}
		return false
	}
	if tool == "codex" && !hasHelper(parsed) {
		return errors.New("Codex parser did not expose http_headers_helper; upgrade Codex before installing a saved-credential connection")
	}
	return nil
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
