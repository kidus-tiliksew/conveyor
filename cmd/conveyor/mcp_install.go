package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	mcpOwnerVersion   = "v1"
	mcpTokenEnv       = "CONVEYOR_API_TOKEN"
	mcpBridgeGuidance = "export CONVEYOR_API_TOKEN=$(conveyor auth token)"
	mcpAddressEnv     = "CONVEYOR_ADDR"
	codexOwnerMarker  = "# conveyor:mcp-install owner=" + mcpOwnerVersion
	claudeOwnerKey    = "_conveyor_mcp_install"
	claudeOwnerValue  = "owner=" + mcpOwnerVersion
)

type mcpInstallTarget struct {
	tool string
	path string
}

type mcpInstallResult struct {
	tool       string
	status     string
	path       string
	validation string
}

func mcpCmd() *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "Manage native MCP client registrations"}
	command.AddCommand(mcpInstallCmd())
	return command
}

// mcpInstallCmd installs every detected editor's native registration while
// keeping stored credentials out of editor-owned files
// (req-260811-0ee057 REQ-13/AC-13.5).
func mcpInstallCmd() *cobra.Command { return mcpInstallCmdWithLookPath(exec.LookPath) }

func mcpInstallCmdWithLookPath(lookPath func(string) (string, error)) *cobra.Command {
	var list, adopt bool
	var selectedTool string
	command := &cobra.Command{
		Use: "install", Short: "Install Conveyor MCP registrations for detected tools", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tools, err := selectSkillTools(selectedTool, lookPath)
			// An explicit OpenCode install can prepare a machine before the binary is installed.
			if strings.EqualFold(strings.TrimSpace(selectedTool), "opencode") {
				tools, err = []skillTool{{name: "opencode", binary: "opencode"}}, nil
			}
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve MCP destination: %w", err)
			}
			home, err = filepath.Abs(home)
			if err != nil {
				return fmt.Errorf("resolve MCP destination: %w", err)
			}
			targets := mcpTargets(home, tools)
			if len(tools) == 1 && len(targets) == 0 {
				return fmt.Errorf("no native MCP registration target for %s", tools[0].name)
			}
			server, err := selectedStoredMCPServer()
			if err != nil {
				return err
			}
			results := make([]mcpInstallResult, 0, len(targets))
			needsAddressBridge := false
			for _, target := range targets {
				needsAddressBridge = needsAddressBridge || (target.tool == "cursor" || target.tool == "opencode")
				result, reconcileErr := reconcileMCPRegistrationWithLookPath(home, target, server+"/mcp", adopt, !list, lookPath)
				if reconcileErr != nil {
					return reconcileErr
				}
				if list && result.status == "created" {
					result.status = "not installed (would create)"
				}
				results = append(results, result)
			}
			for _, tool := range tools {
				supported := false
				for _, target := range targets {
					if target.tool == tool.name {
						supported = true
						break
					}
				}
				if !supported {
					results = append(results, mcpInstallResult{tool: tool.name, status: "unsupported", path: "no native MCP registration target"})
				}
			}
			for _, result := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s", result.tool, result.status, result.path)
				if result.validation != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "\t%s", result.validation)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			if strings.TrimSpace(os.Getenv(mcpTokenEnv)) == "" {
				fmt.Fprintln(cmd.OutOrStdout(), mcpBridgeGuidance)
			}
			if needsAddressBridge && !strings.HasSuffix(strings.TrimRight(strings.TrimSpace(os.Getenv(mcpAddressEnv)), "/"), "/mcp") {
				fmt.Fprintf(cmd.OutOrStdout(), "export %s=%s/mcp\n", mcpAddressEnv, server)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&list, "list", false, "list native registration state without writing")
	command.Flags().StringVar(&selectedTool, "tool", "", "install only for one detected tool (claude, codex, cursor, or opencode)")
	command.Flags().BoolVar(&adopt, "adopt", false, "adopt an unmarked existing Conveyor registration")
	return command
}

func selectedStoredMCPServer() (string, error) {
	config, err := loadLocalAuthConfig()
	if err != nil {
		return "", err
	}
	explicit := ""
	if serverFlagExplicit || strings.TrimSpace(serverFlag) != "" {
		explicit = serverFlag
	} else if value := strings.TrimSpace(os.Getenv("CONVEYOR_ADDR")); value != "" {
		explicit = value
	}
	if explicit != "" {
		canonical, normalizeErr := normalizeServerURL(explicit)
		if normalizeErr != nil {
			return "", normalizeErr
		}
		if strings.TrimSpace(config.Servers[canonical].Token) == "" {
			return "", fmt.Errorf("no stored credential for %s; run `conveyor auth login --server %s`", canonical, canonical)
		}
		return canonical, nil
	}
	servers := make([]string, 0, len(config.Servers))
	for server, entry := range config.Servers {
		if strings.TrimSpace(entry.Token) != "" {
			servers = append(servers, server)
		}
	}
	sort.Strings(servers)
	switch len(servers) {
	case 0:
		return "", errors.New("no stored Conveyor credential; run `conveyor auth login`")
	case 1:
		return normalizeServerURL(servers[0])
	default:
		return "", errors.New("multiple stored Conveyor servers; select one with --server")
	}
}

func mcpTargets(home string, tools []skillTool) []mcpInstallTarget {
	targets := make([]mcpInstallTarget, 0, len(tools))
	for _, tool := range tools {
		switch tool.name {
		case "codex":
			targets = append(targets, mcpInstallTarget{tool: tool.name, path: filepath.Join(home, ".codex", "config.toml")})
		case "claude":
			targets = append(targets, mcpInstallTarget{tool: tool.name, path: filepath.Join(home, ".claude.json")})
		case "opencode":
			configRoot := os.Getenv("XDG_CONFIG_HOME")
			if configRoot == "" {
				configRoot = filepath.Join(home, ".config")
			}
			targets = append(targets, mcpInstallTarget{tool: tool.name, path: filepath.Join(configRoot, "opencode", "opencode.json")})
		case "cursor":
			targets = append(targets, mcpInstallTarget{tool: tool.name, path: filepath.Join(home, ".cursor", "mcp.json")})
		}
	}
	return targets
}

func reconcileMCPRegistration(home string, target mcpInstallTarget, endpoint string, adopt, write bool) (mcpInstallResult, error) {
	return reconcileMCPRegistrationWithLookPath(home, target, endpoint, adopt, write, exec.LookPath)
}

func reconcileMCPRegistrationWithLookPath(home string, target mcpInstallTarget, endpoint string, adopt, write bool, lookPath func(string) (string, error)) (mcpInstallResult, error) {
	if target.tool == "opencode" {
		if !filepath.IsAbs(target.path) {
			return mcpInstallResult{}, fmt.Errorf("OpenCode MCP destination %s must be absolute; set an absolute XDG_CONFIG_HOME", target.path)
		}
		// XDG may live outside HOME. Inspect every component from the filesystem
		// root so a redirected config root cannot bypass symlink refusal.
		home = filepath.VolumeName(target.path) + string(filepath.Separator)
	}
	if err := ensureSafeInstallPath(home, target.path); err != nil {
		return mcpInstallResult{}, fmt.Errorf("MCP config %s: %w", target.path, err)
	}
	prior, mode, exists, err := readMCPConfig(target.path)
	if err != nil {
		return mcpInstallResult{}, err
	}
	var next []byte
	var status string
	switch target.tool {
	case "codex":
		next, status, err = reconcileCodexMCP(prior, endpoint, adopt)
	case "claude":
		next, status, err = reconcileClaudeMCP(prior, endpoint, adopt)
	case "cursor":
		next, status, err = reconcileCursorMCP(prior, adopt)
	case "opencode":
		next, status, err = reconcileOpenCodeMCP(prior, adopt)
	default:
		err = fmt.Errorf("unsupported MCP tool %q; supported tools: claude, codex, cursor, opencode", target.tool)
	}
	if err != nil {
		return mcpInstallResult{}, fmt.Errorf("%s %s: %w", target.tool, target.path, err)
	}
	if status == "skipped" || status == "unchanged" || !write {
		return mcpInstallResult{tool: target.tool, status: status, path: target.path}, nil
	}
	if !exists {
		mode = 0o600
	}
	var validate func(string) error
	validation := ""
	if target.tool == "opencode" {
		binary, lookupErr := lookPath("opencode")
		if lookupErr != nil {
			validation = "validation skipped: opencode is not on PATH"
		} else {
			validate = func(staged string) error {
				return validateOpenCodeMCP(binary, staged, 30*time.Second)
			}
			validation = "validated with opencode debug config"
		}
	}
	if err = atomicWriteMCPConfigValidated(home, target.path, next, mode, validate); err != nil {
		return mcpInstallResult{}, err
	}
	return mcpInstallResult{tool: target.tool, status: status, path: target.path, validation: validation}, nil
}

func readMCPConfig(path string) ([]byte, fs.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("refusing unsafe MCP destination %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	return data, info.Mode().Perm(), true, nil
}

func atomicWriteMCPConfig(home, path string, content []byte, mode fs.FileMode) error {
	return atomicWriteMCPConfigValidated(home, path, content, mode, nil)
}

func atomicWriteMCPConfigValidated(home, path string, content []byte, mode fs.FileMode, validate func(string) error) error {
	if err := ensureSafeInstallPath(home, path); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create MCP config directory: %w", err)
	}
	if err := ensureSafeInstallPath(home, path); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".conveyor-mcp-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(mode.Perm()); err == nil {
		_, err = temporary.Write(content)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	if validate != nil {
		if err = validate(temporaryPath); err != nil {
			return fmt.Errorf("validate OpenCode MCP config %s: %w", path, err)
		}
	}
	if err = ensureSafeInstallPath(home, path); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return os.Chmod(path, mode.Perm())
}

func reconcileCodexMCP(prior []byte, endpoint string, adopt bool) ([]byte, string, error) {
	const section = "[mcp_servers.conveyor]"
	desired := codexOwnerMarker + "\n" + section + "\nurl = " + fmt.Sprintf("%q", endpoint) + "\nbearer_token_env_var = \"" + mcpTokenEnv + "\"\n"
	start, end, found, owned := codexSectionRange(prior, section)
	if !found {
		prefix := prior
		if len(prefix) > 0 && prefix[len(prefix)-1] != '\n' {
			prefix = append(append([]byte(nil), prefix...), '\n')
		}
		if len(prefix) > 0 && !bytes.HasSuffix(prefix, []byte("\n\n")) {
			prefix = append(prefix, '\n')
		}
		return append(prefix, desired...), "created", nil
	}
	if !owned && !adopt {
		return prior, "skipped", nil
	}
	if string(prior[start:end]) == desired {
		return prior, "unchanged", nil
	}
	next := make([]byte, 0, len(prior)-(end-start)+len(desired))
	next = append(next, prior[:start]...)
	next = append(next, desired...)
	next = append(next, prior[end:]...)
	return next, "refreshed", nil
}

func codexSectionRange(content []byte, section string) (start, end int, found, owned bool) {
	lines := bytes.SplitAfter(content, []byte("\n"))
	offset := 0
	for index, line := range lines {
		trimmed := strings.TrimSpace(string(line))
		if trimmed != section {
			offset += len(line)
			continue
		}
		found, start = true, offset
		if index > 0 && strings.TrimSpace(string(lines[index-1])) == codexOwnerMarker {
			owned = true
			start -= len(lines[index-1])
		}
		end = offset + len(line)
		for next := index + 1; next < len(lines); next++ {
			if strings.HasPrefix(strings.TrimSpace(string(lines[next])), "[") {
				break
			}
			end += len(lines[next])
		}
		return
	}
	return
}

func reconcileClaudeMCP(prior []byte, endpoint string, adopt bool) ([]byte, string, error) {
	if len(bytes.TrimSpace(prior)) == 0 {
		prior = []byte("{}\n")
	}
	root, err := scanJSONObject(prior)
	if err != nil {
		return nil, "", fmt.Errorf("parse JSON: %w", err)
	}
	owned := false
	if member, ok := root.member(claudeOwnerKey); ok {
		var marker string
		owned = json.Unmarshal(prior[member.valueStart:member.valueEnd], &marker) == nil && marker == claudeOwnerValue
	}
	serversMember, hasServers := root.member("mcpServers")
	var servers jsonObject
	if hasServers {
		servers, err = scanJSONObject(prior[serversMember.valueStart:serversMember.valueEnd])
		if err != nil {
			return nil, "", errors.New("mcpServers is not a JSON object")
		}
	}
	conveyorMember, exists := servers.member("conveyor")
	if exists && !owned && !adopt {
		return prior, "skipped", nil
	}
	desiredServer, _ := json.Marshal(struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}{Type: "http", URL: endpoint, Headers: map[string]string{"Authorization": "Bearer ${" + mcpTokenEnv + "}"}})
	if exists && owned && jsonEquivalent(prior[serversMember.valueStart+conveyorMember.valueStart:serversMember.valueStart+conveyorMember.valueEnd], desiredServer) {
		return prior, "unchanged", nil
	}
	next := append([]byte(nil), prior...)
	if hasServers {
		updatedServers, updateErr := setJSONObjectMember(prior[serversMember.valueStart:serversMember.valueEnd], "conveyor", desiredServer)
		if updateErr != nil {
			return nil, "", updateErr
		}
		next = replaceBytes(next, serversMember.valueStart, serversMember.valueEnd, updatedServers)
	} else {
		serverObject := append([]byte(`{"conveyor":`), desiredServer...)
		serverObject = append(serverObject, '}')
		next, err = setJSONObjectMember(next, "mcpServers", serverObject)
		if err != nil {
			return nil, "", err
		}
	}
	next, err = setJSONObjectMember(next, claudeOwnerKey, []byte(fmt.Sprintf("%q", claudeOwnerValue)))
	if err != nil {
		return nil, "", err
	}
	status := "created"
	if exists {
		status = "refreshed"
	}
	return next, status, nil
}

func reconcileCursorMCP(prior []byte, adopt bool) ([]byte, string, error) {
	if len(bytes.TrimSpace(prior)) == 0 {
		prior = []byte("{}\n")
	}
	root, err := scanJSONObject(prior)
	if err != nil {
		return nil, "", fmt.Errorf("parse JSON: %w", err)
	}
	owned := false
	if member, ok := root.member(claudeOwnerKey); ok {
		var marker string
		owned = json.Unmarshal(prior[member.valueStart:member.valueEnd], &marker) == nil && marker == claudeOwnerValue
	}
	serversMember, hasServers := root.member("mcpServers")
	var servers jsonObject
	if hasServers {
		servers, err = scanJSONObject(prior[serversMember.valueStart:serversMember.valueEnd])
		if err != nil {
			return nil, "", errors.New("mcpServers is not a JSON object")
		}
	}
	conveyorMember, exists := servers.member("conveyor")
	if exists && !owned && !adopt {
		return prior, "skipped", nil
	}
	desiredServer, _ := json.Marshal(struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}{URL: "${env:" + mcpAddressEnv + "}", Headers: map[string]string{"Authorization": "Bearer ${env:" + mcpTokenEnv + "}"}})
	if exists && owned && jsonEquivalent(prior[serversMember.valueStart+conveyorMember.valueStart:serversMember.valueStart+conveyorMember.valueEnd], desiredServer) {
		return prior, "unchanged", nil
	}
	next := append([]byte(nil), prior...)
	if hasServers {
		updatedServers, updateErr := setJSONObjectMember(prior[serversMember.valueStart:serversMember.valueEnd], "conveyor", desiredServer)
		if updateErr != nil {
			return nil, "", updateErr
		}
		next = replaceBytes(next, serversMember.valueStart, serversMember.valueEnd, updatedServers)
	} else {
		serverObject := append([]byte(`{"conveyor":`), desiredServer...)
		serverObject = append(serverObject, '}')
		next, err = setJSONObjectMember(next, "mcpServers", serverObject)
		if err != nil {
			return nil, "", err
		}
	}
	next, err = setJSONObjectMember(next, claudeOwnerKey, []byte(fmt.Sprintf("%q", claudeOwnerValue)))
	if err != nil {
		return nil, "", err
	}
	status := "created"
	if exists {
		status = "refreshed"
	}
	return next, status, nil
}

// req-cli-authentication REQ-4/AC-4.1 through AC-4.4;
// component-harness-execution: Native MCP registrations and stored credentials.
func reconcileOpenCodeMCP(prior []byte, adopt bool) ([]byte, string, error) {
	if len(bytes.TrimSpace(prior)) == 0 {
		prior = []byte("{}\n")
	}
	for index := 0; index < len(prior); index++ {
		if prior[index] == '"' {
			end, err := scanJSONString(prior, index)
			if err != nil {
				return nil, "", fmt.Errorf("parse JSON: %w", err)
			}
			index = end - 1
		} else if prior[index] == '/' && index+1 < len(prior) && (prior[index+1] == '/' || prior[index+1] == '*') {
			return nil, "", errors.New("refusing comment-bearing OpenCode config; remove comments before installing to preserve other members")
		}
	}
	if !json.Valid(prior) {
		return nil, "", errors.New("parse JSON: invalid OpenCode config")
	}
	root, err := scanJSONObject(prior)
	if err != nil {
		return nil, "", fmt.Errorf("parse JSON: %w", err)
	}
	member, hasServers := root.member("mcp")
	serverBytes := []byte("{}")
	if hasServers {
		serverBytes = prior[member.valueStart:member.valueEnd]
	}
	servers, err := scanJSONObject(serverBytes)
	if err != nil {
		return nil, "", errors.New("mcp is not a JSON object")
	}
	entry, exists := servers.member("conveyor")
	owned := false
	if exists {
		var fields map[string]json.RawMessage
		var owner string
		if json.Unmarshal(serverBytes[entry.valueStart:entry.valueEnd], &fields) == nil {
			owned = json.Unmarshal(fields[claudeOwnerKey], &owner) == nil && owner == claudeOwnerValue
		}
		if !owned && !adopt {
			return prior, "skipped", nil
		}
	}
	desired := []byte(`{"type":"remote","url":"{env:CONVEYOR_ADDR}","headers":{"Authorization":"Bearer {env:CONVEYOR_API_TOKEN}"},"_conveyor_mcp_install":"owner=` + mcpOwnerVersion + `"}`)
	if exists && owned && jsonEquivalent(serverBytes[entry.valueStart:entry.valueEnd], desired) {
		return prior, "unchanged", nil
	}
	updated, err := setJSONObjectMember(serverBytes, "conveyor", desired)
	if err != nil {
		return nil, "", err
	}
	next, err := setJSONObjectMember(prior, "mcp", updated)
	status := "created"
	if exists {
		status = "refreshed"
	}
	return next, status, err
}

// Validate a copy because OpenCode may add $schema while loading a file.
// Isolation keeps existing global/project configuration and stored accounts out
// of this config check. Only the original staged bytes can be published.
func validateOpenCodeMCP(binary, staged string, timeout time.Duration) error {
	content, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	isolated, err := os.MkdirTemp(filepath.Dir(staged), ".conveyor-opencode-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(isolated)
	// Keep file references relative to the real config directory.
	copy, err := os.CreateTemp(filepath.Dir(staged), ".conveyor-opencode-*.json")
	if err != nil {
		return err
	}
	configPath := copy.Name()
	defer os.Remove(configPath)
	_, err = copy.Write(content)
	closeErr := copy.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "debug", "config")
	cmd.Dir = isolated
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "OPENCODE_") || strings.HasPrefix(key, "XDG_") || key == "HOME" || key == mcpAddressEnv || key == mcpTokenEnv {
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env,
		"HOME="+isolated,
		"XDG_CONFIG_HOME="+filepath.Join(isolated, "config"),
		"XDG_DATA_HOME="+filepath.Join(isolated, "data"),
		"XDG_STATE_HOME="+filepath.Join(isolated, "state"),
		"XDG_CACHE_HOME="+filepath.Join(isolated, "cache"),
		"OPENCODE_CONFIG="+configPath,
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		mcpAddressEnv+"=https://conveyor.invalid/mcp",
		mcpTokenEnv+"=conveyor-install-validation",
	)
	// A nil stdin reads from os.DevNull. Debug stdout resolves env values.
	cmd.Stdout = io.Discard
	var stderr openCodeValidationStderr
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); ctx.Err() != nil {
		return errors.New("opencode debug config timed out")
	} else if err != nil {
		return fmt.Errorf("opencode debug config failed: %w", err)
	} else if stderr.invalid {
		return errors.New("opencode debug config reported an invalid configuration")
	}
	return nil
}

// Retain only enough stderr to recognize markers split across writes. Never
// include debug output in diagnostics, since it can contain resolved values.
type openCodeValidationStderr struct {
	tail    string
	invalid bool
}

func (w *openCodeValidationStderr) Write(p []byte) (int, error) {
	text := w.tail + string(p)
	w.invalid = w.invalid || strings.Contains(text, "Unrecognized key") || strings.Contains(text, "ConfigInvalid")
	if len(text) > 32 {
		text = text[len(text)-32:]
	}
	w.tail = text
	return len(p), nil
}

type jsonMember struct {
	key                  string
	valueStart, valueEnd int
}

type jsonObject struct {
	end     int
	members []jsonMember
}

func (object jsonObject) member(key string) (jsonMember, bool) {
	for _, member := range object.members {
		if member.key == key {
			return member, true
		}
	}
	return jsonMember{}, false
}

func scanJSONObject(data []byte) (jsonObject, error) {
	index := skipJSONSpace(data, 0)
	if index >= len(data) || data[index] != '{' {
		return jsonObject{}, errors.New("expected JSON object")
	}
	index++
	object := jsonObject{}
	seen := map[string]bool{}
	for {
		index = skipJSONSpace(data, index)
		if index >= len(data) {
			return jsonObject{}, errors.New("unterminated JSON object")
		}
		if data[index] == '}' {
			object.end = index
			if skipJSONSpace(data, index+1) != len(data) {
				return jsonObject{}, errors.New("trailing content after JSON object")
			}
			return object, nil
		}
		keyStart := index
		keyEnd, err := scanJSONString(data, index)
		if err != nil {
			return jsonObject{}, err
		}
		var key string
		if err = json.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return jsonObject{}, err
		}
		if seen[key] {
			return jsonObject{}, fmt.Errorf("duplicate JSON object key %q", key)
		}
		seen[key] = true
		index = skipJSONSpace(data, keyEnd)
		if index >= len(data) || data[index] != ':' {
			return jsonObject{}, errors.New("expected colon after JSON object key")
		}
		valueStart := skipJSONSpace(data, index+1)
		valueEnd, err := scanJSONValue(data, valueStart)
		if err != nil {
			return jsonObject{}, err
		}
		object.members = append(object.members, jsonMember{key: key, valueStart: valueStart, valueEnd: valueEnd})
		index = skipJSONSpace(data, valueEnd)
		if index < len(data) && data[index] == ',' {
			index++
			continue
		}
		if index >= len(data) || data[index] != '}' {
			return jsonObject{}, errors.New("expected comma or object end")
		}
	}
}

func scanJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, errors.New("expected JSON string")
	}
	escaped := false
	for index := start + 1; index < len(data); index++ {
		if escaped {
			escaped = false
			continue
		}
		if data[index] == '\\' {
			escaped = true
			continue
		}
		if data[index] == '"' {
			return index + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func scanJSONValue(data []byte, start int) (int, error) {
	if start >= len(data) {
		return 0, errors.New("missing JSON value")
	}
	if data[start] == '"' {
		return scanJSONString(data, start)
	}
	if data[start] == '{' || data[start] == '[' {
		stack := []byte{data[start]}
		for index := start + 1; index < len(data); index++ {
			if data[index] == '"' {
				end, err := scanJSONString(data, index)
				if err != nil {
					return 0, err
				}
				index = end - 1
				continue
			}
			switch data[index] {
			case '{', '[':
				stack = append(stack, data[index])
			case '}', ']':
				open := stack[len(stack)-1]
				if (open == '{' && data[index] != '}') || (open == '[' && data[index] != ']') {
					return 0, errors.New("mismatched JSON delimiter")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return index + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated JSON value")
	}
	index := start
	for index < len(data) && data[index] != ',' && data[index] != '}' && data[index] != ']' && data[index] != '\n' && data[index] != '\r' {
		index++
	}
	if index == start {
		return 0, errors.New("invalid JSON value")
	}
	return index, nil
}

func skipJSONSpace(data []byte, index int) int {
	for index < len(data) && (data[index] == ' ' || data[index] == '\t' || data[index] == '\n' || data[index] == '\r') {
		index++
	}
	return index
}

func setJSONObjectMember(data []byte, key string, value []byte) ([]byte, error) {
	object, err := scanJSONObject(data)
	if err != nil {
		return nil, err
	}
	if member, ok := object.member(key); ok {
		return replaceBytes(data, member.valueStart, member.valueEnd, value), nil
	}
	encodedKey, _ := json.Marshal(key)
	insertion := append([]byte(nil), encodedKey...)
	insertion = append(insertion, ':')
	insertion = append(insertion, value...)
	if len(object.members) > 0 {
		insertion = append([]byte(","), insertion...)
	}
	return replaceBytes(data, object.end, object.end, insertion), nil
}

func replaceBytes(data []byte, start, end int, replacement []byte) []byte {
	result := make([]byte, 0, len(data)-(end-start)+len(replacement))
	result = append(result, data[:start]...)
	result = append(result, replacement...)
	result = append(result, data[end:]...)
	return result
}

func jsonEquivalent(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && fmt.Sprint(a) == fmt.Sprint(b)
}
