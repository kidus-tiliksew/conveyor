export type MCPClient = 'cursor' | 'opencode' | 'claude' | 'codex' | 'other'

// How a client signs in to `/mcp`. OAuth is the default: the client opens
// Conveyor's sign-in page in the browser and keeps its own token. `pat` is the
// explicit `conveyor mcp install --auth pat` fallback with a personal access
// token (req-mcp-oauth REQ-5; component-web-dashboard invariant 26).
export type MCPAuthMode = 'oauth' | 'pat'

export interface MCPClientSetup {
  id: MCPClient
  label: string
  description: string
  steps: string[]
  snippetLabel: string
  snippet: string
}

export function mcpEndpoint(origin: string) {
  return new URL('/mcp', origin).toString()
}

// The server address that `conveyor --server` takes: the endpoint's origin.
function serverBase(endpoint: string) {
  return new URL(endpoint).origin
}

/** The default connection settings: the remote endpoint and nothing else. */
export function mcpConnectionConfig(endpoint: string) {
  return `{
  "mcpServers": {
    "conveyor": {
      "url": "${endpoint}"
    }
  }
}`
}

/** The `--auth pat` connection settings, with a placeholder and never a value. */
export function mcpTokenConnectionConfig(endpoint: string) {
  return `{
  "mcpServers": {
    "conveyor": {
      "url": "${endpoint}",
      "headers": { "Authorization": "Bearer <CONVEYOR_API_TOKEN>" }
    }
  }
}`
}

export function mcpClientSetups(endpoint: string, mode: MCPAuthMode = 'oauth'): MCPClientSetup[] {
  return mode === 'oauth' ? oauthClientSetups(endpoint) : tokenClientSetups(endpoint)
}

// Each OAuth setup is a plain remote registration of the endpoint. The client
// finds Conveyor's sign-in page from the endpoint, so no setup holds a
// credential, a header, or a token placeholder.
function oauthClientSetups(endpoint: string): MCPClientSetup[] {
  const server = serverBase(endpoint)
  return [
    {
      id: 'cursor',
      label: 'Cursor',
      description: 'Connect Cursor to this Conveyor server. You sign in with your browser.',
      steps: [
        `Run conveyor mcp install --tool cursor --server ${server}, or merge the configuration below into ~/.cursor/mcp.json.`,
        'Open Cursor. When it asks you to sign in to Conveyor, approve the request in your browser.',
        'Verify with cursor-agent mcp list-tools conveyor.',
      ],
      snippetLabel: '~/.cursor/mcp.json',
      snippet: mcpConnectionConfig(endpoint),
    },
    {
      id: 'opencode',
      label: 'OpenCode',
      description: 'Connect OpenCode to this Conveyor server. You sign in with your browser.',
      steps: [
        `Run conveyor mcp install --tool opencode --server ${server}, or merge the configuration below into ~/.config/opencode/opencode.json.`,
        'Start OpenCode. When it asks you to sign in to Conveyor, approve the request in your browser.',
        'Verify with opencode mcp list. The conveyor line must read connected.',
        'Do not add keys outside mcp. OpenCode refuses unknown top-level keys and does not start.',
      ],
      snippetLabel: '~/.config/opencode/opencode.json',
      snippet: `{
  "mcp": {
    "conveyor": {
      "type": "remote",
      "url": "${endpoint}"
    }
  }
}`,
    },
    {
      id: 'claude',
      label: 'Claude Code',
      description: 'Connect Claude Code to this Conveyor server. You sign in with your browser.',
      steps: [
        `Run conveyor mcp install --tool claude --server ${server}, or merge the configuration below into ~/.claude.json.`,
        'Start a new Claude Code session and run /mcp. Select conveyor and approve the request in your browser.',
        'Verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.claude.json',
      snippet: `{
  "mcpServers": {
    "conveyor": {
      "type": "http",
      "url": "${endpoint}"
    }
  }
}`,
    },
    {
      id: 'codex',
      label: 'Codex',
      description: 'Connect Codex to this Conveyor server. You sign in with your browser.',
      steps: [
        `Run conveyor mcp install --tool codex --server ${server}, or add the configuration below to ~/.codex/config.toml.`,
        'Run codex mcp login conveyor and approve the request in your browser.',
        'Start a new Codex session and verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.codex/config.toml',
      snippet: `[mcp_servers.conveyor]
url = "${endpoint}"`,
    },
    {
      id: 'other',
      label: 'Other',
      description: 'Connect a streamable HTTP MCP client that supports OAuth sign-in.',
      steps: [
        'Open the MCP server configuration of your client.',
        'Add the endpoint below. Do not add an Authorization header.',
        'Connect the client. It opens Conveyor in your browser, where you approve the request.',
      ],
      snippetLabel: 'MCP connection settings',
      snippet: mcpConnectionConfig(endpoint),
    },
  ]
}

// The `--auth pat` setups keep a personal access token out of each file: the
// CLI installer stores a reference, and a pasted file holds a placeholder.
function tokenClientSetups(endpoint: string): MCPClientSetup[] {
  const server = serverBase(endpoint)
  return [
    {
      id: 'cursor',
      label: 'Cursor',
      description: 'Connect Cursor with a personal access token.',
      steps: [
        `Run conveyor auth login --server ${server}, then conveyor mcp install --tool cursor --auth pat --server ${server}.`,
        'In the shell that launches Cursor, run the export line that the installer prints.',
        'Verify with cursor-agent mcp list-tools conveyor.',
      ],
      snippetLabel: '~/.cursor/mcp.json',
      snippet: `{
  "mcpServers": {
    "conveyor": {
      "url": "${endpoint}",
      "headers": { "Authorization": "Bearer \${env:CONVEYOR_API_TOKEN}" }
    }
  }
}`,
    },
    {
      id: 'opencode',
      label: 'OpenCode',
      description: 'Connect OpenCode with a personal access token.',
      steps: [
        `Run conveyor auth login --server ${server}, then conveyor mcp install --tool opencode --auth pat --server ${server}.`,
        'In the shell that launches OpenCode, run the export line that the installer prints.',
        'Verify with opencode mcp list. The conveyor line must read connected.',
      ],
      snippetLabel: '~/.config/opencode/opencode.json',
      snippet: `{
  "mcp": {
    "conveyor": {
      "type": "remote",
      "url": "${endpoint}",
      "oauth": false,
      "headers": { "Authorization": "Bearer {env:CONVEYOR_API_TOKEN}" }
    }
  }
}`,
    },
    {
      id: 'claude',
      label: 'Claude Code',
      description: 'Connect Claude Code with a personal access token.',
      steps: [
        `Run conveyor auth login --server ${server}, then conveyor mcp install --tool claude --auth pat --server ${server}.`,
        'The installer adds a header helper that reads your saved token. To edit the file yourself, replace the placeholder below.',
        'Start a new Claude Code session and verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.claude.json',
      snippet: `{
  "mcpServers": {
    "conveyor": {
      "type": "http",
      "url": "${endpoint}",
      "headers": { "Authorization": "Bearer <CONVEYOR_API_TOKEN>" }
    }
  }
}`,
    },
    {
      id: 'codex',
      label: 'Codex',
      description: 'Connect Codex with a personal access token.',
      steps: [
        `Run conveyor auth login --server ${server}, then conveyor mcp install --tool codex --auth pat --server ${server}.`,
        'The installer adds a header helper that reads your saved token. To edit the file yourself, export CONVEYOR_API_TOKEN where Codex starts.',
        'Start a new Codex session and verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.codex/config.toml',
      snippet: `[mcp_servers.conveyor]
url = "${endpoint}"
bearer_token_env_var = "CONVEYOR_API_TOKEN"`,
    },
    {
      id: 'other',
      label: 'Other',
      description: 'Connect another MCP client with a personal access token.',
      steps: [
        'Add the endpoint and Authorization header below to the MCP server configuration of your client.',
        'Replace the placeholder with one of your access tokens, then reconnect the client.',
        `A client that cannot send a header can run the stdio bridge: conveyor mcp stdio --server ${server} --credentials-file <absolute path>.`,
      ],
      snippetLabel: 'MCP connection settings',
      snippet: mcpTokenConnectionConfig(endpoint),
    },
  ]
}
