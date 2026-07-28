package config

// HarnessTemplate is one deployment-static starting point for a workspace
// harness configuration. Callers may customize the returned Harness before
// persisting it.
type HarnessTemplate struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Harness     Harness `json:"harness"`
}

// HarnessTemplates returns the built-in harness catalog used by the operator
// UI. The catalog is intentionally not applied during workspace creation.
func HarnessTemplates() []HarnessTemplate {
	return []HarnessTemplate{
		{
			ID:          "codex",
			Label:       "Codex CLI",
			Description: "OpenAI's coding agent",
			// Codex uses the whole-argument TOML override transport (spec §21.20).
			Harness: Harness{
				Name:             "codex",
				MCPTransport:     MCPTransportTOMLOverride,
				Command:          []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"},
				ModelArgs:        []string{"--model", "{model}"},
				EffortArgs:       map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}},
				ProbeCommand:     []string{"codex", "--version"},
				ProbeTimeoutText: "10s",
				StallTimeoutText: DefaultHarnessStallTimeoutText,
			},
		},
		{
			ID:          "claude",
			Label:       "Claude Code",
			Description: "Anthropic's coding agent",
			Harness: Harness{
				Name:             "claude",
				MCPTransport:     MCPTransportJSONFile,
				Command:          []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--allowedTools", "mcp__conveyor__*"},
				ModelArgs:        []string{"--model", "{model}"},
				EffortArgs:       map[string][]string{"high": {"--effort", "high"}},
				ProbeCommand:     []string{"claude", "--version"},
				ProbeTimeoutText: "10s",
				StallTimeoutText: DefaultHarnessStallTimeoutText,
			},
		},
		{
			ID:          "grok",
			Label:       "Grok CLI",
			Description: "xAI's coding agent",
			// Grok receives Conveyor through its child-scoped environment attachment;
			// this argv is pinned by the accepted amendment (spec §21.29).
			Harness: Harness{
				Name:          "grok",
				MCPTransport:  MCPTransportEnvironment,
				MCPAttachment: "conveyor",
				Command:       []string{"grok", "--single", "{prompt}", "--permission-mode", "bypassPermissions", "--no-plan"},
				ModelArgs:     []string{"--model", "{model}"},
				EffortArgs: map[string][]string{
					"low":    {"--reasoning-effort", "low"},
					"medium": {"--reasoning-effort", "medium"},
					"high":   {"--reasoning-effort", "high"},
				},
				ProbeCommand:     []string{"grok", "--version"},
				ProbeTimeoutText: "30s",
				StallTimeoutText: DefaultHarnessStallTimeoutText,
			},
		},
	}
}
