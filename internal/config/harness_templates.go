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
				Name:         "codex",
				MCPTransport: MCPTransportTOMLOverride,
				Command:      []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"},
				ModelArgs:    []string{"--model", "{model}"},
				EffortArgs: map[string][]string{
					"low":    {"--config", `model_reasoning_effort="low"`},
					"medium": {"--config", `model_reasoning_effort="medium"`},
					"high":   {"--config", `model_reasoning_effort="high"`},
				},
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
				Name:         "claude",
				MCPTransport: MCPTransportJSONFile,
				// stream-json keeps stdout flowing from the first event; plain -p
				// buffers until completion and trips first_activity_timeout (§21.42)
				// on any run longer than the liveness window. Headless -p has no
				// interactive approver, so writes need bypassPermissions (the
				// §21.29 grok precedent), and --add-dir .. reaches the §21.8
				// sibling worktrees outside the primary checkout the child
				// starts in.
				Command:   []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--allowedTools", "mcp__conveyor__*", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions", "--add-dir", ".."},
				ModelArgs: []string{"--model", "{model}"},
				EffortArgs: map[string][]string{
					"low":    {"--effort", "low"},
					"medium": {"--effort", "medium"},
					"high":   {"--effort", "high"},
				},
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
