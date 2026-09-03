export type MCPClient = 'cursor' | 'claude' | 'codex' | 'other'

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

export function mcpConnectionConfig(endpoint: string) {
  return `{
  "mcpServers": {
    "conveyor": {
      "url": "${endpoint}",
      "headers": { "Authorization": "Bearer <CONVEYOR_API_TOKEN>" }
    }
  }
}`
}

export function mcpClientSetups(endpoint: string): MCPClientSetup[] {
  const jsonConfig = mcpConnectionConfig(endpoint)
  return [
    {
      id: 'cursor',
      label: 'Cursor',
      description: 'Connect Cursor to this Conveyor deployment.',
      steps: [
        'Run conveyor auth login, then conveyor mcp install --tool cursor, or merge the configuration below into ~/.cursor/mcp.json.',
        `Export CONVEYOR_ADDR=${endpoint} and CONVEYOR_API_TOKEN=$(conveyor auth token) in the shell that launches Cursor.`,
        'Verify with cursor-agent mcp list-tools conveyor.',
      ],
      snippetLabel: '~/.cursor/mcp.json',
      snippet: `{
  "mcpServers": {
    "conveyor": {
      "url": "\${env:CONVEYOR_ADDR}",
      "headers": { "Authorization": "Bearer \${env:CONVEYOR_API_TOKEN}" }
    }
  }
}`,
    },
    {
      id: 'claude',
      label: 'Claude Code',
      description: 'Connect Claude Code to this Conveyor deployment.',
      steps: [
        'Open ~/.claude.json.',
        'Merge in the configuration below and replace the token placeholder with an access token.',
        'Start a fresh Claude Code session and verify the Conveyor tools are available.',
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
      description: 'Connect Codex to this Conveyor deployment.',
      steps: [
        'Export CONVEYOR_API_TOKEN in the environment that launches Codex.',
        'Add the configuration below to ~/.codex/config.toml.',
        'Start a fresh Codex session and verify the Conveyor tools are available.',
      ],
      snippetLabel: '~/.codex/config.toml',
      snippet: `[mcp_servers.conveyor]
url = "${endpoint}"
bearer_token_env_var = "CONVEYOR_API_TOKEN"`,
    },
    {
      id: 'other',
      label: 'Other',
      description: 'Connect any streamable HTTP MCP client to this Conveyor deployment.',
      steps: [
        'Open your client’s MCP server configuration.',
        'Add the endpoint and Authorization header below.',
        'Replace the token placeholder at setup time, then reconnect the client.',
      ],
      snippetLabel: 'MCP connection settings',
      snippet: jsonConfig,
    },
  ]
}
