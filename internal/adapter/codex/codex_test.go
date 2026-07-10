package codex

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		kind adapter.EventKind
		text string
		tool string
	}{
		{
			name: "agent message",
			line: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}`,
			kind: adapter.EventAssistantText,
			text: "done",
		},
		{
			name: "command start",
			line: `{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"ls"}}`,
			kind: adapter.EventToolCall,
			tool: "command_execution",
		},
		{
			name: "command end",
			line: `{"type":"item.completed","item":{"id":"item_2","type":"command_execution","exit_code":0}}`,
			kind: adapter.EventToolResult,
			tool: "command_execution",
		},
		{
			name: "usage on turn end",
			line: `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300}}`,
			kind: adapter.EventTokenUsage,
		},
		{
			name: "turn failure",
			line: `{"type":"turn.failed","error":{"message":"boom"}}`,
			kind: adapter.EventError,
		},
		{
			name: "item_type field variant",
			line: `{"type":"item.completed","item":{"id":"item_3","item_type":"agent_message","text":"alt"}}`,
			kind: adapter.EventAssistantText,
			text: "alt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := parseLine([]byte(tc.line))
			if ev.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s", ev.Kind, tc.kind)
			}
			if tc.text != "" && ev.Text != tc.text {
				t.Fatalf("text = %q, want %q", ev.Text, tc.text)
			}
			if tc.tool != "" && ev.Tool != tc.tool {
				t.Fatalf("tool = %q, want %q", ev.Tool, tc.tool)
			}
			if len(ev.Payload) == 0 {
				t.Fatal("raw payload must always be preserved")
			}
		})
	}

	ev := parseLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}`))
	if ev.Usage == nil || ev.Usage.In != 12 || ev.Usage.Out != 3 {
		t.Fatalf("usage = %+v", ev.Usage)
	}
}
