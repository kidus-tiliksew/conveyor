import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus } from 'lucide-react'
import { useDashboardSession } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'
import { fetchPersonalAccessTokens, issuePersonalAccessToken, revokePersonalAccessToken } from '../../lib/api'
import { errorMessage } from '../../lib/errors'

function lifecycleSummary(created: string, lastUsed?: string) {
  const createdText = `Created ${new Date(created).toLocaleString()}`
  return lastUsed ? `${createdText} · Last used ${new Date(lastUsed).toLocaleString()}` : `${createdText} · Never used`
}

const deploymentRevokeConfirmation = 'REVOKE DEPLOYMENT CREDENTIAL'

/**
 * The signed-in person's own access tokens.
 *
 * A freshly issued value lives in component state and nowhere else: it is never
 * written to a query cache, never stored, and no listing response carries it,
 * so leaving this screen is the same as losing it.
 */
export function PersonalTokensCard() {
  const token = useDashboardSession()
  const queryClient = useQueryClient()
  const [label, setLabel] = useState('')
  const [issuedValue, setIssuedValue] = useState('')
  const [deploymentRevokeID, setDeploymentRevokeID] = useState('')
  const [deploymentRevokeText, setDeploymentRevokeText] = useState('')

  const tokens = useQuery({
    queryKey: ['personal-access-tokens', token],
    queryFn: () => fetchPersonalAccessTokens(),
    enabled: Boolean(token),
  })
  const issue = useMutation({
    mutationFn: () => issuePersonalAccessToken(label.trim()),
    onSuccess: async (created) => {
      setIssuedValue(created.value)
      setLabel('')
      await queryClient.invalidateQueries({ queryKey: ['personal-access-tokens', token] })
    },
  })
  const revoke = useMutation({
    mutationFn: (id: string) => revokePersonalAccessToken(id),
    onSuccess: async () => {
      setDeploymentRevokeID('')
      setDeploymentRevokeText('')
      await queryClient.invalidateQueries({ queryKey: ['personal-access-tokens', token] })
    },
  })

  const deploymentRevokeToken = tokens.data?.find(
    (item) => item.id === deploymentRevokeID && item.deployment_credential && !item.revoked_at,
  )
  const closeDeploymentRevoke = () => {
    if (revoke.isPending) return
    setDeploymentRevokeID('')
    setDeploymentRevokeText('')
  }

  if (!token) return null

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Your access tokens</CardTitle>
        <KeyRound className="size-4 text-faint" />
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-sm leading-6 text-muted">
          Tokens sign in as you, from the CLI or an agent session. They are yours alone — nobody else can see or revoke
          them.
        </p>
        <form
          className="flex flex-wrap items-end gap-2"
          aria-label="Create an access token"
          onSubmit={(event) => {
            event.preventDefault()
            issue.mutate()
          }}
        >
          <Input
            required
            aria-label="Token name"
            placeholder="Laptop CLI"
            className="max-w-xs"
            value={label}
            onChange={(event) => setLabel(event.target.value)}
          />
          <Button type="submit" disabled={!label.trim() || issue.isPending}>
            <Plus />
            {issue.isPending ? 'Creating…' : 'Create token'}
          </Button>
          {issue.error && (
            <p className="basis-full text-xs text-failure">
              {errorMessage(issue.error, 'Could not create that token.')}
            </p>
          )}
        </form>

        {issuedValue && (
          <div className="space-y-2 rounded-lg border border-positive/30 bg-positive-soft p-3">
            <p className="text-sm font-medium text-positive">Copy your token now — it will not be shown again.</p>
            <div className="flex items-center gap-2">
              <p className="min-w-0 flex-1 break-all rounded-md border border-border bg-surface p-2 font-mono text-xs">
                {issuedValue}
              </p>
              <CopyButton value={issuedValue} label="Copy token" />
            </div>
            <Button size="sm" variant="secondary" onClick={() => setIssuedValue('')}>
              Done
            </Button>
          </div>
        )}

        {tokens.error && (
          <p className="text-sm text-failure">{errorMessage(tokens.error, 'Could not load your tokens.')}</p>
        )}
        {tokens.isSuccess && tokens.data.length === 0 && <p className="text-sm text-faint">No tokens yet.</p>}
        {tokens.data?.map((item) => (
          <div key={item.id} className="flex items-center justify-between rounded-md border border-border p-3">
            <div className="min-w-0">
              <div className="flex min-w-0 items-center gap-2">
                <p className="truncate text-sm font-medium">{item.label}</p>
                {item.deployment_credential && (
                  <Badge variant="outline" tabIndex={0} title="The CONVEYOR_API_TOKEN mapping created at first boot.">
                    Deployment credential
                  </Badge>
                )}
              </div>
              <p className="truncate text-xs text-muted" title={lifecycleSummary(item.created_at, item.last_used_at)}>
                {lifecycleSummary(item.created_at, item.last_used_at)}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              {item.revoked_at ? (
                <Badge variant="failure">Revoked</Badge>
              ) : (
                <Button
                  size="sm"
                  variant="destructive"
                  aria-label={`Revoke ${item.label}`}
                  disabled={revoke.isPending}
                  onClick={() => {
                    if (item.deployment_credential) {
                      setDeploymentRevokeID(item.id)
                      setDeploymentRevokeText('')
                    } else {
                      revoke.mutate(item.id)
                    }
                  }}
                >
                  Revoke
                </Button>
              )}
            </div>
          </div>
        ))}
        {revoke.error && (
          <p className="text-sm text-failure">{errorMessage(revoke.error, 'Could not revoke that token.')}</p>
        )}
      </CardContent>
      {deploymentRevokeToken && (
        <Dialog onClose={closeDeploymentRevoke} label="Revoke deployment credential">
          <div className="border-b border-border px-5 py-4">
            <h2 className="text-base font-semibold tracking-tight">Revoke deployment credential?</h2>
            <p className="mt-1 text-sm leading-6 text-muted">
              Revoking this credential blocks the next conveyord start while <code>CONVEYOR_API_TOKEN</code> still
              contains this value. Remove or change the environment variable before that start.
            </p>
          </div>
          <form
            className="space-y-4 px-5 py-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (deploymentRevokeText === deploymentRevokeConfirmation) revoke.mutate(deploymentRevokeToken.id)
            }}
          >
            <p className="text-sm leading-6 text-foreground">
              To retire a possibly leaked value without blocking startup, rotate <code>CONVEYOR_API_TOKEN</code> and
              restart instead. Startup will re-map the deployment credential and record an audit event.
            </p>
            <div className="space-y-1.5">
              <label className="text-sm font-medium" htmlFor="deployment-revoke-confirmation">
                Type <span className="font-mono">{deploymentRevokeConfirmation}</span> to continue
              </label>
              <Input
                id="deployment-revoke-confirmation"
                autoComplete="off"
                value={deploymentRevokeText}
                onChange={(event) => setDeploymentRevokeText(event.target.value)}
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="secondary" disabled={revoke.isPending} onClick={closeDeploymentRevoke}>
                Cancel
              </Button>
              <Button
                type="submit"
                variant="destructive"
                disabled={deploymentRevokeText !== deploymentRevokeConfirmation || revoke.isPending}
              >
                {revoke.isPending ? 'Revoking…' : 'Revoke deployment credential'}
              </Button>
            </div>
          </form>
        </Dialog>
      )}
    </Card>
  )
}
