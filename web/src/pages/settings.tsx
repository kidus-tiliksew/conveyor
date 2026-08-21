import { useSearch } from '@tanstack/react-router'
import { CheckCircle2, KeyRound, PlugZap, Terminal } from 'lucide-react'
import { useTokenState } from '../components/app-shell'
import { PersonalTokensCard } from '../components/settings/personal-tokens-card'
import { PasswordCard } from '../components/settings/password-card'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { CopyButton } from '../components/ui/copy-button'
import { Input } from '../components/ui/input'
import { mcpConnectionConfig, mcpEndpoint } from '../lib/mcp'

export function SettingsPage() {
  const { token, setToken } = useTokenState()
  const { welcome } = useSearch({ from: '/settings' })
  const cliCommand = 'export CONVEYOR_API_TOKEN="<paste-your-token>"'
  const endpoint = mcpEndpoint(window.location.origin)
  const mcpConfig = mcpConnectionConfig(endpoint).replace('<CONVEYOR_API_TOKEN>', '<paste-your-token>')
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-2xl px-6 py-8">
        <h1 className="text-xl font-semibold tracking-tight">Settings</h1>
        <p className="mt-1 text-sm text-muted">Your access and connection settings.</p>

        {welcome && (
          <Card className="mt-6 border-primary/30 bg-primary-soft/30">
            <CardHeader>
              <CardTitle>Welcome to Conveyor</CardTitle>
              <CheckCircle2 className="size-4 text-positive" />
            </CardHeader>
            <CardContent className="space-y-4 text-sm leading-6 text-muted">
              <p>
                You’re signed in. Create your first access token below, copy it when it appears, then use these details
                to connect your command line or coding agent.
              </p>
              <div className="space-y-2">
                <p className="font-medium text-foreground">Command line</p>
                <div className="flex items-center gap-2 rounded-md border border-border bg-card p-2">
                  <code className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{cliCommand}</code>
                  <CopyButton value={cliCommand} label="Copy command" />
                </div>
              </div>
              <div className="space-y-2">
                <p className="font-medium text-foreground">Coding-agent connection</p>
                <div className="flex items-start gap-2 rounded-md border border-border bg-card p-2">
                  <pre className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{mcpConfig}</pre>
                  <CopyButton value={mcpConfig} label="Copy connection settings" />
                </div>
              </div>
            </CardContent>
          </Card>
        )}

        <Card className={welcome ? 'mt-4' : 'mt-6'}>
          <CardHeader>
            <CardTitle>Operator fallback</CardTitle>
            <KeyRound className="size-4 text-faint" />
          </CardHeader>
          <CardContent className="space-y-2.5">
            <p className="text-sm leading-6 text-muted">
              Operators and non-durable deployments can use the control plane's{' '}
              <code className="font-mono text-xs">CONVEYOR_API_TOKEN</code>. Multi-workspace reads and writes both
              accept it. The token is kept in session storage and forgotten when the tab closes.
            </p>
            <Input
              type="password"
              aria-label="Operator token"
              placeholder="Paste the API token"
              value={token}
              onChange={(event) => setToken(event.target.value)}
              className="max-w-sm font-mono"
            />
            <p className="text-xs text-faint">
              {token
                ? 'Token set — workspace data and operator actions are enabled.'
                : 'No token — set it to load workspaces.'}
            </p>
          </CardContent>
        </Card>
        <PasswordCard />
        <PersonalTokensCard />
        <Card className="mt-4">
          <CardHeader>
            <CardTitle>MCP work-order server</CardTitle>
            {welcome ? <Terminal className="size-4 text-primary" /> : <PlugZap className="size-4 text-primary" />}
          </CardHeader>
          <CardContent className="space-y-2 text-sm leading-6 text-muted">
            <p>
              Connect each operator-owned coding-agent session to{' '}
              <code className="font-mono text-xs text-foreground">{endpoint}</code> using the bearer token above. Start
              a fresh session for review: Conveyor rejects self-review at claim time.
            </p>
            <pre className="overflow-x-auto rounded-md bg-background p-3 font-mono text-xs text-foreground">
              {mcpConnectionConfig(endpoint)}
            </pre>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
