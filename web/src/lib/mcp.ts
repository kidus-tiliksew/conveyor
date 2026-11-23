export type MCPClient = 'cursor' | 'opencode' | 'claude' | 'codex' | 'other'

// How a client signs in to `/mcp`. OAuth is the default: the client opens
// Conveyor's sign-in page in the browser and keeps its own token. `pat` is the
// personal access token fallback (req-mcp-oauth REQ-5; component-web-dashboard
// invariant 26).
//
// The released `conveyor mcp install` has no `--auth` flag: it needs a saved
// access token and always writes an access token entry. The OAuth installer
// mode comes with a later CLI version. So each OAuth setup leads with the
// manual steps that work with any CLI, and every installer command that needs
// `--auth` is labeled with the CLI version that it needs.
export type MCPAuthMode = 'oauth' | 'pat'

export interface MCPClientSetup {
  id: MCPClient
  label: string
  description: string
  steps: string[]
  snippetLabel: string
  snippet: string
  // An installer command that needs a CLI version with `--auth`, shown apart
  // from the steps, which work with the released CLI.
  installerNote: string
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

/** The access token connection settings, with a placeholder and never a value. */
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
  const installerNote = (tool: string) =>
    `Needs a Conveyor CLI version that supports --auth: conveyor mcp install --tool ${tool} --server ${server} writes this entry for you. The current CLI writes an access token entry instead.`
  return [
    {
      id: 'cursor',
      label: 'Cursor',
      description: 'Connect Cursor to this Conveyor server. You sign in with your browser.',
      steps: [
        'Merge the configuration below into ~/.cursor/mcp.json. It holds only the Conveyor endpoint.',
        'Open Cursor. When it asks you to sign in to Conveyor, approve the request in your browser.',
        'Verify with cursor-agent mcp list-tools conveyor.',
      ],
      snippetLabel: '~/.cursor/mcp.json',
      snippet: mcpConnectionConfig(endpoint),
      installerNote: installerNote('cursor'),
    },
    {
      id: 'opencode',
      label: 'OpenCode',
      description: 'Connect OpenCode to this Conveyor server. You sign in with your browser.',
      steps: [
        'Merge the configuration below into ~/.config/opencode/opencode.json. It holds only the Conveyor endpoint.',
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
      installerNote: installerNote('opencode'),
    },
    {
      id: 'claude',
      label: 'Claude Code',
      description: 'Connect Claude Code to this Conveyor server. You sign in with your browser.',
      steps: [
        'Merge the configuration below into ~/.claude.json. It holds only the Conveyor endpoint.',
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
      installerNote: installerNote('claude'),
    },
    {
      id: 'codex',
      label: 'Codex',
      description: 'Connect Codex to this Conveyor server. You sign in with your browser.',
      steps: [
        'Add the configuration below to ~/.codex/config.toml. It holds only the Conveyor endpoint.',
        'Run codex mcp login conveyor and approve the request in your browser.',
        'Start a new Codex session and verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.codex/config.toml',
      snippet: `[mcp_servers.conveyor]
url = "${endpoint}"`,
      installerNote: installerNote('codex'),
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
      installerNote: '',
    },
  ]
}

// The access token setups use the released CLI: `conveyor auth login` saves
// a token, and `conveyor mcp install` writes an entry that reads it. A file
// edited by hand holds a placeholder or an environment reference, never a
// token value.
function tokenClientSetups(endpoint: string): MCPClientSetup[] {
  const server = serverBase(endpoint)
  const login = `Run conveyor auth login --server ${server} to save an access token.`
  // `auth login` saves a token and selects no server for later commands, so
  // every command names the server of the endpoint beside it. Without it,
  // `auth token` reads the token of CONVEYOR_ADDR or of the CLI default.
  const exportToken = `export CONVEYOR_API_TOKEN=$(conveyor auth token --server ${server})`
  const installerNote =
    'With a Conveyor CLI version that supports --auth, add --auth pat to the install command. Without it, that version installs browser sign-in.'
  return [
    {
      id: 'cursor',
      label: 'Cursor',
      description: 'Connect Cursor with a personal access token.',
      steps: [
        login,
        `Run conveyor mcp install --tool cursor --server ${server}, then run the export line that it prints in the shell that launches Cursor.`,
        `To edit the file yourself, merge the configuration below and run ${exportToken} in the shell that launches Cursor.`,
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
      installerNote,
    },
    {
      id: 'opencode',
      label: 'OpenCode',
      description: 'Connect OpenCode with a personal access token.',
      steps: [
        login,
        `Run conveyor mcp install --tool opencode --server ${server}, then run the export line that it prints in the shell that launches OpenCode.`,
        `To edit the file yourself, merge the configuration below and run ${exportToken} in the shell that launches OpenCode.`,
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
      installerNote,
    },
    {
      id: 'claude',
      label: 'Claude Code',
      description: 'Connect Claude Code with a personal access token.',
      steps: [
        login,
        `Run conveyor mcp install --tool claude --server ${server}. It adds a header helper that reads your saved token.`,
        'To edit the file yourself, merge the configuration below and replace the placeholder with one of your access tokens.',
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
      installerNote,
    },
    {
      id: 'codex',
      label: 'Codex',
      description: 'Connect Codex with a personal access token.',
      steps: [
        login,
        `Run conveyor mcp install --tool codex --server ${server}. It adds a header helper that reads your saved token.`,
        `To edit the file yourself, add the configuration below and run ${exportToken} where Codex starts.`,
        'Start a new Codex session and verify that the Conveyor tools are available.',
      ],
      snippetLabel: '~/.codex/config.toml',
      snippet: `[mcp_servers.conveyor]
url = "${endpoint}"
bearer_token_env_var = "CONVEYOR_API_TOKEN"`,
      installerNote,
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
      installerNote: '',
    },
  ]
}
