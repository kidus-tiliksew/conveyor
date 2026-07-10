// Package codex adapts the OpenAI Codex CLI — the first Phase 1 harness
// (spec §5.1, §19), chosen so ChatGPT subscription auth is exercised
// from day one.
//
// Invocation flags are pinned per harness version here and verified
// against vendor docs at upgrade time. The adapter runs `codex exec`
// (headless mode) with --json and parses the JSONL event stream.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
)

// PinnedVersion is the Codex CLI version these flags were validated
// against. Bump deliberately; adapters break on silent CLI drift.
const PinnedVersion = "TODO-pin-on-first-integration"

type Adapter struct {
	// Binary is the codex executable; overridable for tests.
	Binary string
	// Home is mapped to CODEX_HOME inside the sandbox so session state
	// lands in the job directory and becomes an artifact by construction
	// (spec §8.3 note 2).
	Home string
	// ContainerConfined disables codex's native sandbox
	// (--sandbox danger-full-access): under Tier A the container is the
	// confinement boundary, and bwrap/Landlock cannot create namespaces
	// inside an unprivileged container anyway. Never set outside a
	// container — Tier B relies on the native sandbox (spec §8.5).
	ContainerConfined bool
}

func New() *Adapter {
	return &Adapter{Binary: "codex"}
}

func (a *Adapter) Name() string { return "codex" }

func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		MultiRepo:  true,
		Resume:     true, // codex exec resume; fidelity measured by the Phase 1 experiment (spec §20.2)
		JSONStream: true,
		AuthModes:  []string{"personal_sub", "team_sub", "api"},
		// Seatbelt (macOS) / Landlock+seccomp (Linux) — candidate for
		// Tier B confinement (spec §8.5).
		NativeSandbox: true,
	}
}

func (a *Adapter) Run(ctx context.Context, spec adapter.RunSpec) (<-chan adapter.Event, error) {
	// TODO(phase1): map spec.Policy into codex's sandbox/approval flags
	// (--sandbox workspace-write, approval mode) — the adapter, not the
	// user's interactive config, owns unattended permission posture
	// (spec §8.5 responsibility seam).
	// TODO(phase1): register a session hook to capture the session ID
	// authoritatively during the run (spec §8.3 note 1).
	args := []string{"exec", "--json", "--cd", spec.Workdir}
	if a.ContainerConfined {
		args = append(args, "--sandbox", "danger-full-access")
	}
	args = append(args, spec.Prompt)
	return a.stream(ctx, spec.Workdir, args)
}

func (a *Adapter) Resume(ctx context.Context, sessionRef string, feedback string) (<-chan adapter.Event, error) {
	args := []string{"exec", "resume", sessionRef, "--json", feedback}
	return a.stream(ctx, "", args)
}

func (a *Adapter) stream(ctx context.Context, dir string, args []string) (<-chan adapter.Event, error) {
	cmd := exec.CommandContext(ctx, a.Binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if a.Home != "" {
		cmd.Env = append(cmd.Environ(), "CODEX_HOME="+a.Home)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}

	events := make(chan adapter.Event, 64)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
		for sc.Scan() {
			events <- parseLine(sc.Bytes())
		}
		if err := cmd.Wait(); err != nil {
			events <- adapter.Event{Kind: adapter.EventError, Err: err.Error(), At: time.Now()}
			return
		}
		events <- adapter.Event{Kind: adapter.EventDone, At: time.Now()}
	}()
	return events, nil
}

// parseLine maps one Codex JSONL thread event onto the normalized event
// set. Current `codex exec --json` emits thread.started, turn.started,
// item.started/updated/completed (item detail nested under "item"), and
// turn.completed (token usage nested under "usage"). Anything
// unrecognized passes through with its raw payload so no information is
// dropped before the transcript store.
// TODO(phase1): verify field names against the pinned CLI version on
// first integration and set PinnedVersion.
func parseLine(line []byte) adapter.Event {
	var probe struct {
		Type string `json:"type"`
		Item struct {
			Type     string `json:"type"`
			ItemType string `json:"item_type"` // older builds nest the type here
			Text     string `json:"text"`
		} `json:"item"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	ev := adapter.Event{At: time.Now(), Payload: json.RawMessage(append([]byte(nil), line...))}
	if err := json.Unmarshal(line, &probe); err != nil {
		ev.Kind = adapter.EventAssistantText
		ev.Text = string(line)
		return ev
	}

	itemType := probe.Item.Type
	if itemType == "" {
		itemType = probe.Item.ItemType
	}

	switch probe.Type {
	case "item.started", "item.updated", "item.completed":
		switch itemType {
		case "agent_message", "reasoning":
			ev.Kind = adapter.EventAssistantText
			ev.Text = probe.Item.Text
		case "error":
			ev.Kind = adapter.EventError
			ev.Err = probe.Item.Text
		default:
			// command_execution, file_change, mcp_tool_call, web_search, …
			ev.Tool = itemType
			if probe.Type == "item.started" {
				ev.Kind = adapter.EventToolCall
			} else {
				ev.Kind = adapter.EventToolResult
			}
		}
	case "turn.completed":
		ev.Kind = adapter.EventTokenUsage
		ev.Usage = &adapter.TokenUsage{In: probe.Usage.InputTokens, Out: probe.Usage.OutputTokens}
	case "turn.failed", "error":
		ev.Kind = adapter.EventError
		ev.Err = probe.Error.Message
	default:
		// thread.started, turn.started, unknown: raw payload only.
		ev.Kind = adapter.EventAssistantText
	}
	return ev
}
