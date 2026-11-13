import { PlugZap } from 'lucide-react'
import { mcpConnectionConfig } from '../../lib/mcp'
import { CopyButton } from '../ui/copy-button'
import { SettingsRow } from './settings-row'

// How a coding-agent session reaches this deployment's MCP endpoint. The
// configuration carries a placeholder, never a token value
// (component-web-dashboard invariant 26). The closed row copies the endpoint
// without opening.
export function ConnectAgentRow({ endpoint }: { endpoint: string }) {
  const config = mcpConnectionConfig(endpoint)
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
          Connect each coding-agent session that you start to this endpoint with one of your access tokens. Start a new
          session for each review, because Conveyor refuses a review claim from the session that did the work.
        </p>
        <div className="flex items-center gap-2 rounded-md border border-border bg-background p-2">
          <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{endpoint}</code>
          <CopyButton value={endpoint} label="Copy endpoint" />
        </div>
        <div className="flex items-start gap-2 rounded-md border border-border bg-background p-2">
          <pre className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{config}</pre>
          <CopyButton value={config} label="Copy connection configuration" />
        </div>
      </div>
    </SettingsRow>
  )
}
