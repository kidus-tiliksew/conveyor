package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

const (
	mcpOwnerVersion   = "v1"
	mcpTokenEnv       = "CONVEYOR_API_TOKEN"
	mcpBridgeGuidance = "export CONVEYOR_API_TOKEN=$(conveyor auth token)"
	codexOwnerMarker  = "# conveyor:mcp-install owner=" + mcpOwnerVersion
	claudeOwnerKey    = "_conveyor_mcp_install"
	claudeOwnerValue  = "owner=" + mcpOwnerVersion
)

type mcpInstallTarget struct {
	tool string
	path string
}

type mcpInstallResult struct {
	tool   string
	status string
	path   string
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
			server, err := selectedStoredMCPServer()
			if err != nil {
				return err
			}
			tools, err := selectSkillTools(selectedTool, lookPath)
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
			results := make([]mcpInstallResult, 0, len(targets))
			for _, target := range targets {
				result, reconcileErr := reconcileMCPRegistration(home, target, server+"/mcp", adopt, !list)
				if reconcileErr != nil {
					return reconcileErr
				}
				if list && result.status == "created" {
					result.status = "not installed (would create)"
				}
				results = append(results, result)
			}
			for _, result := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", result.tool, result.status, result.path)
			}
			if strings.TrimSpace(os.Getenv(mcpTokenEnv)) == "" {
				fmt.Fprintln(cmd.OutOrStdout(), mcpBridgeGuidance)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&list, "list", false, "list native registration state without writing")
	command.Flags().StringVar(&selectedTool, "tool", "", "install only for one detected tool (claude or codex)")
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
		}
	}
	return targets
}

func reconcileMCPRegistration(home string, target mcpInstallTarget, endpoint string, adopt, write bool) (mcpInstallResult, error) {
	if err := ensureSafeInstallPath(home, target.path); err != nil {
		return mcpInstallResult{}, err
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
	default:
		err = fmt.Errorf("unsupported MCP tool %q", target.tool)
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
	if err = atomicWriteMCPConfig(home, target.path, next, mode); err != nil {
		return mcpInstallResult{}, err
	}
	return mcpInstallResult{tool: target.tool, status: status, path: target.path}, nil
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
