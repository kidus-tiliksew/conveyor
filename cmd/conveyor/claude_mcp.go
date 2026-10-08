package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The JSON-file MCP transport is supported only for Claude Code. Its
// registration references the child credential by environment variable and
// the actual child must confirm the connection through its stream-json
// initialization event before the launch is trusted
// (component-harness-execution; component-local-launchers;
// req-security-boundaries AC-2.4, AC-2.6, AC-2.7).
const (
	jsonMCPServerName = "conveyor"
	// Claude Code expands ${VAR} in remote MCP headers loaded through
	// --mcp-config (https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcp-json).
	jsonMCPAuthorizationReference = "Bearer ${CONVEYOR_API_TOKEN}"
	jsonMCPConfigPlaceholder      = "{mcp_config}"
	claudeExecutableName          = "claude"
	claudeStrictMCPConfigFlag     = "--strict-mcp-config"
	claudeMCPConfigFlag           = "--mcp-config"
	claudeOutputFormatFlag        = "--output-format"
	claudeStreamJSONFormat        = "stream-json"
	// claudeReceiptLineLimit bounds one stdout line, excluding its newline,
	// while the launcher waits for the receipt.
	claudeReceiptLineLimit = 1 << 20
	// defaultJSONMCPReceiptTimeout applies when the harness declares no
	// probe_timeout.
	defaultJSONMCPReceiptTimeout = 10 * time.Second
)

type jsonMCPServerEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type jsonMCPDocument struct {
	MCPServers map[string]jsonMCPServerEntry `json:"mcpServers"`
}

// jsonMCPConfig renders the JSON-file registration from the endpoint alone.
// No credential reaches this function, so the bytes are identical for every
// child credential (AC-2.6).
func jsonMCPConfig(endpoint string) ([]byte, error) {
	return json.Marshal(jsonMCPDocument{MCPServers: map[string]jsonMCPServerEntry{
		jsonMCPServerName: {Type: "http", URL: endpoint, Headers: map[string]string{"Authorization": jsonMCPAuthorizationReference}},
	}})
}

func usesJSONMCPTransport(transport string) bool {
	return transport == "" || transport == config.MCPTransportJSONFile
}

// jsonMCPAdapterError refuses a JSON-file harness that cannot use a credential
// reference with a confirmed receipt. It names the harness and the remedy and
// never suggests a literal credential.
type jsonMCPAdapterError struct {
	Harness string
	Detail  string
}

func (e *jsonMCPAdapterError) Error() string {
	return fmt.Sprintf("harness %q cannot use the json_file MCP transport: %s; json_file is supported only for Claude Code with "+
		"[claude, -p, \"{prompt}\", --mcp-config, \"{mcp_config}\", --strict-mcp-config, --output-format, stream-json, --verbose, ...]; "+
		"recreate the harness from the built-in claude template with `conveyor config init-execution`, or use the codex toml_override setup "+
		"or a grok, cursor, or opencode environment setup; Conveyor never writes a credential into a json_file registration",
		e.Harness, e.Detail)
}

// admitJSONMCPHarness checks a selected harness before any claim. Identity
// comes from the executable, never from the harness name.
func admitJSONMCPHarness(harness config.Harness) error {
	if !usesJSONMCPTransport(harness.MCPTransport) {
		return nil
	}
	refuse := func(detail string) error { return &jsonMCPAdapterError{Harness: harness.Name, Detail: detail} }
	if len(harness.Command) == 0 || filepath.Base(harness.Command[0]) != claudeExecutableName {
		return refuse("its command does not run the claude executable")
	}
	argv := append([]string(nil), harness.Command...)
	argv = append(argv, harness.ModelArgs...)
	for _, effort := range []string{"low", "medium", "high"} {
		argv = append(argv, harness.EffortArgs[effort]...)
	}
	for effort, args := range harness.EffortArgs {
		if effort != "low" && effort != "medium" && effort != "high" {
			argv = append(argv, args...)
		}
	}
	argv = append(argv, harness.ResumeCommand...)
	if detail := claudeArgvShapeViolation(argv, jsonMCPConfigPlaceholder); detail != "" {
		return refuse(detail)
	}
	return nil
}

// claudeJSONMCPLaunchArgv repeats the shape check on a final launch argv and
// makes the generated file the child's only MCP source by inserting
// --strict-mcp-config after it when absent. Only the transient argv changes.
func claudeJSONMCPLaunchArgv(harness config.Harness, argv []string, configPath string) ([]string, error) {
	if len(argv) == 0 || filepath.Base(argv[0]) != claudeExecutableName {
		return nil, &jsonMCPAdapterError{Harness: harness.Name, Detail: "its command does not run the claude executable"}
	}
	if detail := claudeArgvShapeViolation(argv, configPath); detail != "" {
		return nil, &jsonMCPAdapterError{Harness: harness.Name, Detail: detail}
	}
	for _, element := range argv {
		if element == claudeStrictMCPConfigFlag {
			return append([]string(nil), argv...), nil
		}
	}
	result := make([]string, 0, len(argv)+1)
	for index, element := range argv {
		result = append(result, element)
		if element == claudeMCPConfigFlag && index+1 < len(argv) {
			result = append(result, argv[index+1], claudeStrictMCPConfigFlag)
			result = append(result, argv[index+2:]...)
			break
		}
	}
	return result, nil
}

// claudeArgvShapeViolation returns a fixed reason when argv is not a headless
// stream-json Claude launch whose only MCP configuration is configToken.
func claudeArgvShapeViolation(argv []string, configToken string) string {
	print, verbose, streamJSON := false, false, false
	configs := 0
	for index, element := range argv {
		switch {
		case element == "-p" || element == "--print":
			print = true
		case element == "--verbose":
			verbose = true
		case element == claudeMCPConfigFlag:
			configs++
			if index+1 >= len(argv) || argv[index+1] != configToken {
				return "--mcp-config must be followed by the generated " + jsonMCPConfigPlaceholder + " file"
			}
			if index+2 < len(argv) && !strings.HasPrefix(argv[index+2], "-") {
				return "the element after the generated MCP config must be a flag so no other configuration joins it"
			}
		case strings.HasPrefix(element, claudeMCPConfigFlag+"="):
			return "an additional --mcp-config source is not allowed"
		case element == claudeOutputFormatFlag:
			if index+1 >= len(argv) || argv[index+1] != claudeStreamJSONFormat {
				return "the output format must be stream-json"
			}
			streamJSON = true
		case strings.HasPrefix(element, claudeOutputFormatFlag+"="):
			if strings.TrimPrefix(element, claudeOutputFormatFlag+"=") != claudeStreamJSONFormat {
				return "the output format must be stream-json"
			}
			streamJSON = true
		}
	}
	switch {
	case configs != 1:
		return "exactly one --mcp-config naming the generated " + jsonMCPConfigPlaceholder + " file is required"
	case !print:
		return "headless -p output is required"
	case !streamJSON:
		return "--output-format stream-json is required for the MCP receipt"
	case !verbose:
		return "--verbose is required with stream-json output"
	}
	return ""
}

// claudeStageTools names the lifecycle tools a child must see for its stage.
func claudeStageTools(stage core.Stage) []string {
	tools := []string{"get_work_order", "release_work_order"}
	switch stage {
	case core.StageSpec:
		tools = append(tools, "submit_plan")
	case core.StageImplement:
		tools = append(tools, "submit_for_review")
	case core.StageReview:
		tools = append(tools, "submit_review_verdict")
	case core.StageVerify:
		tools = append(tools, "submit_verification")
	}
	return tools
}

var claudeReceiptStatusPattern = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// errClaudeReceiptMissing reports a child that produced no receipt.
var errClaudeReceiptMissing = errors.New("the child exited without a Claude MCP initialization receipt")

// claudeMCPReceipt reads redacted child stdout until the first stream-json
// init event decides the receipt. It holds at most claudeReceiptLineLimit
// bytes, never fails a write, and delivers exactly one result.
type claudeMCPReceipt struct {
	mu       sync.Mutex
	required []string
	buffer   []byte
	decided  bool
	result   chan error
	onDecide func()
}

func newClaudeMCPReceipt(stage core.Stage) *claudeMCPReceipt {
	return &claudeMCPReceipt{required: claudeStageTools(stage), result: make(chan error, 1)}
}

// Result delivers the single decision.
func (r *claudeMCPReceipt) Result() <-chan error { return r.result }

func (r *claudeMCPReceipt) Write(p []byte) (int, error) {
	written := len(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for !r.decided && len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		if newline < 0 {
			if len(r.buffer)+len(p) > claudeReceiptLineLimit {
				r.decide(errors.New("a stdout line exceeded 1 MiB before the MCP receipt"))
				break
			}
			r.buffer = append(r.buffer, p...)
			break
		}
		if len(r.buffer)+newline > claudeReceiptLineLimit {
			r.decide(errors.New("a stdout line exceeded 1 MiB before the MCP receipt"))
			break
		}
		line := append(r.buffer, p[:newline]...)
		r.buffer = nil
		p = p[newline+1:]
		if done, err := evaluateClaudeReceiptLine(line, r.required); done {
			r.decide(err)
		}
	}
	return written, nil
}

func (r *claudeMCPReceipt) decide(err error) {
	r.decided = true
	r.buffer = nil
	r.result <- err
	if r.onDecide != nil {
		r.onDecide()
	}
}

type claudeInitEvent struct {
	Type       string            `json:"type"`
	Subtype    string            `json:"subtype"`
	MCPServers []json.RawMessage `json:"mcp_servers"`
	Tools      []string          `json:"tools"`
}

type claudeInitServer struct {
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
	Source *string         `json:"source"`
}

// evaluateClaudeReceiptLine reports whether line decides the receipt and, if
// so, its outcome. Diagnostics carry only fixed text and a recognized status.
func evaluateClaudeReceiptLine(line []byte, required []string) (bool, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false, nil
	}
	var event claudeInitEvent
	if err := json.Unmarshal(trimmed, &event); err != nil {
		return true, errors.New("the child printed a line that is not stream-json before the MCP receipt")
	}
	if event.Type != "system" || event.Subtype != "init" {
		return false, nil
	}
	var selected *claudeInitServer
	for _, raw := range event.MCPServers {
		var server claudeInitServer
		if err := json.Unmarshal(raw, &server); err != nil {
			return true, errors.New("the initialization event has a malformed MCP server entry")
		}
		if server.Name != jsonMCPServerName {
			continue
		}
		if selected != nil {
			return true, errors.New("the initialization event reports the conveyor MCP server more than once")
		}
		copied := server
		selected = &copied
	}
	if selected == nil {
		return true, errors.New("the initialization event does not report the conveyor MCP server")
	}
	if selected.Status != "connected" {
		status := "unrecognized"
		if claudeReceiptStatusPattern.MatchString(selected.Status) {
			status = selected.Status
		}
		return true, fmt.Errorf("the conveyor MCP server status is %s, not connected", status)
	}
	if errorValue := bytes.TrimSpace(selected.Error); len(errorValue) > 0 && !bytes.Equal(errorValue, []byte("null")) && !bytes.Equal(errorValue, []byte(`""`)) {
		return true, errors.New("the conveyor MCP server reports an error")
	}
	if selected.Source != nil && *selected.Source != "dynamic" {
		return true, errors.New("the conveyor MCP server did not come from the generated --mcp-config registration")
	}
	listed := make(map[string]bool, len(event.Tools))
	for _, tool := range event.Tools {
		listed[tool] = true
	}
	for _, tool := range required {
		if !listed["mcp__"+jsonMCPServerName+"__"+tool] {
			return true, fmt.Errorf("the conveyor MCP server does not provide the lifecycle tool %s", tool)
		}
	}
	return true, nil
}

func jsonMCPReceiptTimeout(harness config.Harness) time.Duration {
	timeout := harness.ProbeTimeout
	if timeout <= 0 {
		timeout, _ = time.ParseDuration(harness.ProbeTimeoutText)
	}
	if timeout <= 0 {
		timeout = defaultJSONMCPReceiptTimeout
	}
	return timeout
}
