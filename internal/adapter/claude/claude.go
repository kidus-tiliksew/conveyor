// Package claude adapts Claude Code's pinned non-interactive stream-json
// interface to Conveyor events (spec §5.1, §19).
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
)

// PinnedVersion is installed by images/base/Dockerfile. The CLI surface and
// stream schema are validated together; silent auto-upgrades are disabled.
const PinnedVersion = "2.1.206"

type Adapter struct {
	Binary              string
	Home                string
	ContainerConfined   bool
	DisableVersionCheck bool
	policy              adapter.ToolPolicy
	workdir             string
}

func New() *Adapter { return &Adapter{Binary: "claude"} }

func (a *Adapter) Name() string { return "claude-code" }

func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		MultiRepo: true, Resume: true, JSONStream: true,
		AuthModes: []string{"personal_sub", "team_sub", "api"}, NativeSandbox: true,
	}
}

func (a *Adapter) Run(ctx context.Context, spec adapter.RunSpec) (<-chan adapter.Event, error) {
	if err := a.checkVersion(ctx); err != nil {
		return nil, err
	}
	args, err := a.args(spec.Prompt, "")
	if err != nil {
		return nil, err
	}
	a.policy = spec.Policy
	a.workdir = spec.Workdir
	args = appendPolicy(args, spec.Policy)
	if spec.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.BudgetUSD, 'f', 2, 64))
	}
	return a.stream(ctx, spec.Workdir, args)
}

func (a *Adapter) Resume(ctx context.Context, sessionRef, feedback string) (<-chan adapter.Event, error) {
	if err := a.checkVersion(ctx); err != nil {
		return nil, err
	}
	if sessionRef == "" {
		return nil, fmt.Errorf("Claude session ref is required")
	}
	args, err := a.args(feedback, sessionRef)
	if err != nil {
		return nil, err
	}
	args = appendPolicy(args, a.policy)
	// Claude sessions are project-scoped. Resume from the same worktree as the
	// main run or the CLI can report "No conversation found" even though the
	// session files are present in CLAUDE_CONFIG_DIR.
	return a.stream(ctx, a.workdir, args)
}

func (a *Adapter) args(prompt, sessionRef string) ([]string, error) {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if sessionRef != "" {
		args = append(args, "--resume", sessionRef)
	}
	if a.ContainerConfined {
		// Tier A is the confinement boundary. Claude deny rules still apply
		// in bypassPermissions mode, including inside a container.
		args = append(args, "--permission-mode", "bypassPermissions")
	} else {
		args = append(args, "--permission-mode", "dontAsk")
	}
	return args, nil
}

func appendPolicy(args []string, policy adapter.ToolPolicy) []string {
	allowed := commandRules(policy.AllowedCommands)
	denied := commandRules(policy.DeniedCommands)
	if len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allowed...)
	}
	if len(denied) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, denied...)
	}
	return args
}

func commandRules(prefixes [][]string) []string {
	var rules []string
	for _, prefix := range prefixes {
		if len(prefix) == 0 {
			continue
		}
		command := strings.Join(prefix, " ")
		rules = append(rules, "Bash("+command+")", "Bash("+command+" *)")
	}
	return rules
}

func (a *Adapter) checkVersion(ctx context.Context) error {
	if a.DisableVersionCheck {
		return nil
	}
	return adapter.CheckVersion(ctx, adapter.VersionSpec{
		Binary: a.Binary, Name: "Claude Code", Want: PinnedVersion,
		Normalize: func(raw string) string {
			fields := strings.Fields(raw)
			if len(fields) == 0 {
				return ""
			}
			return fields[0]
		},
	})
}

func (a *Adapter) stream(ctx context.Context, dir string, args []string) (<-chan adapter.Event, error) {
	var env []string
	if a.Home != "" {
		env = append(env,
			"CLAUDE_CONFIG_DIR="+a.Home,
			"DISABLE_UPDATES=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		)
	}
	parser := newStreamParser()
	return adapter.StreamCommand(ctx, adapter.StreamSpec{
		Binary: a.Binary, Args: args, Dir: dir, Env: env, Name: "Claude Code",
		MaxLineBytes: 16 * 1024 * 1024, Parse: parser.parseLine,
	})
}

type streamMessage struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	CostUSD   float64 `json:"total_cost_usd"`
	Message   struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			Content   any    `json:"content"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func parseLine(line []byte) adapter.Event {
	return newStreamParser().parseLine(line)
}

type streamParser struct {
	usageByMessage map[string]adapter.TokenUsage
}

func newStreamParser() *streamParser {
	return &streamParser{usageByMessage: make(map[string]adapter.TokenUsage)}
}

func (p *streamParser) parseLine(line []byte) adapter.Event {
	ev := adapter.Event{At: time.Now().UTC(), Payload: json.RawMessage(append([]byte(nil), line...))}
	var message streamMessage
	if err := json.Unmarshal(line, &message); err != nil {
		ev.Kind, ev.Text = adapter.EventAssistantText, string(line)
		return ev
	}
	switch message.Type {
	case "system":
		if message.Subtype == "init" && message.SessionID != "" {
			ev.Kind, ev.SessionRef = adapter.EventSessionStart, message.SessionID
		} else {
			ev.Kind = adapter.EventAssistantText
		}
	case "assistant":
		ev.Kind = adapter.EventAssistantText
		for _, block := range message.Message.Content {
			switch block.Type {
			case "tool_use":
				ev.Kind, ev.Tool = adapter.EventToolCall, block.Name
			case "text":
				ev.Text += block.Text
			}
		}
		if message.Message.Usage.InputTokens != 0 || message.Message.Usage.OutputTokens != 0 {
			current := adapter.TokenUsage{In: message.Message.Usage.InputTokens, Out: message.Message.Usage.OutputTokens}
			if message.Message.ID == "" {
				ev.Usage = &current
			} else {
				previous := p.usageByMessage[message.Message.ID]
				delta := adapter.TokenUsage{In: current.In - previous.In, Out: current.Out - previous.Out}
				if delta.In < 0 || delta.Out < 0 {
					delta = current
				}
				p.usageByMessage[message.Message.ID] = current
				if delta.In != 0 || delta.Out != 0 {
					ev.Usage = &delta
				}
			}
		}
	case "user":
		ev.Kind = adapter.EventAssistantText
		for _, block := range message.Message.Content {
			if block.Type == "tool_result" {
				ev.Kind, ev.Tool = adapter.EventToolResult, block.ToolUseID
				break
			}
		}
	case "result":
		ev.CostUSD = message.CostUSD
		ev.Text = message.Result
		if message.IsError || message.Subtype != "success" {
			ev.Kind, ev.Err = adapter.EventError, message.Result
		} else {
			ev.Kind = adapter.EventAssistantText
		}
	default:
		ev.Kind = adapter.EventAssistantText
	}
	return ev
}
