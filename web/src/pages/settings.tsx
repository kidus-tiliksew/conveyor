import { KeyRound } from 'lucide-react'
import { useTokenState } from '../components/app-shell'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'

export function SettingsPage() {
  const { token, setToken } = useTokenState()
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-2xl px-6 py-8">
        <h1 className="text-xl font-semibold tracking-tight">Settings</h1>
        <p className="mt-1 text-sm text-muted">Operator-local preferences for this browser session.</p>

        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Operator token</CardTitle>
            <KeyRound className="size-4 text-faint" />
          </CardHeader>
          <CardContent className="space-y-2.5">
            <p className="text-sm leading-6 text-muted">
              Mutations — creating tasks, review decisions, redispatch — authenticate with the control plane's{' '}
              <code className="font-mono text-xs">CONVEYOR_API_TOKEN</code> (spec §17.3). Reads never require it. The
              token is kept in session storage and forgotten when the tab closes.
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
              {token ? 'Token set — review actions and task creation are enabled.' : 'No token — the dashboard is read-only.'}
            </p>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
