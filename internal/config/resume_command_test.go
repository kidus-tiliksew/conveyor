package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHarnessResumeCommandValidation(t *testing.T) {
	valid := Harness{
		Name: "claude", MCPTransport: MCPTransportJSONFile,
		Command:       []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}"},
		ResumeCommand: []string{"--resume", "{session_id}"}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s",
	}
	if err := ValidateHarness(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"missing session", []string{"--resume", "literal"}},
		{"duplicate session", []string{"--resume", "{session_id}", "{session_id}"}},
		{"embedded session", []string{"--resume={session_id}"}},
		{"unknown placeholder", []string{"--resume", "{conversation_id}"}},
		{"prompt placeholder", []string{"--resume", "{session_id}", "{prompt}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.ResumeCommand = test.argv
			if err := ValidateHarness(candidate); err == nil || !strings.Contains(err.Error(), "resume_command") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestHarnessResumeCommandIsNotServerJSON(t *testing.T) {
	raw, err := json.Marshal(Harness{Name: "claude", ResumeCommand: []string{"--resume", "{session_id}"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resume") || strings.Contains(string(raw), "session_id") {
		t.Fatalf("client-local resume detail leaked into JSON: %s", raw)
	}
}
