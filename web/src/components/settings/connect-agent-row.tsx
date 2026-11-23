import { PlugZap } from 'lucide-react'
import { mcpConnectionConfig, mcpTokenConnectionConfig } from '../../lib/mcp'
import { CopyButton } from '../ui/copy-button'
import { SettingsRow } from './settings-row'

// How a coding-agent session reaches this deployment's MCP endpoint. The
// default is browser sign-in, with only the endpoint in the configuration.
// The access token setup is the fallback for a client without OAuth sign-in
// and carries a placeholder, never a token value (component-web-dashboard invariant 26;
// req-mcp-oauth REQ-5). The closed row copies the endpoint without opening.
export function ConnectAgentRow({ endpoint }: { endpoint: string }) {
  const config = mcpConnectionConfig(endpoint)
  const tokenConfig = mcpTokenConnectionConfig(endpoint)
  const server = new URL(endpoint).origin
  return (
    <SettingsRow
      icon={PlugZap}
      title="Connect a coding agent"
      action="Show"
      summary={<code className="break-all font-mono text-xs text-foreground">{endpoint}</code>}
      aside={<CopyButton value={endpoint} label="Copy MCP endpoint" />}
    >
      <div className="space-y-3 text-sm leading-6 text-muted">
        <p>
          Add this endpoint to your coding agent. When the agent connects, it opens Conveyor in your browser, and you
          approve it once. Start a new session for each review, because Conveyor refuses a review claim from the session
          that did the work.
        </p>
        <div className="flex items-center gap-2 rounded-md border border-border bg-background p-2">
          <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{endpoint}</code>
          <CopyButton value={endpoint} label="Copy endpoint" />
        </div>
        <div className="flex items-start gap-2 rounded-md border border-border bg-background p-2">
          <pre className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{config}</pre>
          <CopyButton value={config} label="Copy connection configuration" />
        </div>
        <div className="space-y-2 border-t border-border pt-3" data-testid="connect-agent-token-option">
          <p className="font-medium text-foreground">Access token option</p>
          <p>
            For a client without browser sign-in, run{' '}
            <code className="font-mono text-xs">conveyor auth login --server {server}</code>, then{' '}
            <code className="font-mono text-xs">conveyor mcp install --server {server}</code>. The current CLI installs
            an access token entry. With a CLI version that supports <code className="font-mono text-xs">--auth</code>,
            add <code className="font-mono text-xs">--auth pat</code>. To edit the file yourself, use these settings and
            replace the placeholder with one of your access tokens.
          </p>
          <div className="flex items-start gap-2 rounded-md border border-border bg-background p-2">
            <pre className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{tokenConfig}</pre>
            <CopyButton value={tokenConfig} label="Copy access token configuration" />
          </div>
        </div>
      </div>
    </SettingsRow>
  )
}
