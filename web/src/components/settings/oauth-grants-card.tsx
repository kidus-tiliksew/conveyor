import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Unplug } from 'lucide-react'
import { fetchOAuthGrants, OAuthRequestError, revokeOAuthGrant } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { fullTime } from '../../lib/utils'
import { Button } from '../ui/button'
import { SettingsRow, SummaryLine } from './settings-row'

const oauthGrantsKey = ['oauth-grants'] as const

function grantLifecycle(created: string, lastUsed: string | null) {
  const createdText = `Connected ${fullTime(created)}`
  return lastUsed ? `${createdText} · Last used ${fullTime(lastUsed)}` : `${createdText} · Never used`
}

function clientLabel(name: string) {
  return name.trim() || 'Unnamed MCP client'
}

// A server without OAuth storage turns the grant routes off with 404.
function grantsUnavailable(error: unknown) {
  return error instanceof OAuthRequestError && error.status === 404 && error.code === 'oauth_unavailable'
}

/**
 * The MCP clients that the signed-in person connected through OAuth
 * (req-mcp-oauth AC-4.4, AC-4.5; component-web-dashboard invariant 25). Each
 * row shows the client name, when it was connected, and when it was last used,
 * with a control that revokes the grant. The listing holds no token value or
 * hash, and this card reads none.
 */
export function OAuthGrantsCard() {
  const queryClient = useQueryClient()
  const grants = useQuery({ queryKey: oauthGrantsKey, queryFn: ({ signal }) => fetchOAuthGrants(signal) })
  const revoke = useMutation({
    mutationFn: (id: string) => revokeOAuthGrant(id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: oauthGrantsKey })
    },
  })

  const active = grants.data?.filter((grant) => !grant.revoked_at) ?? []
  const lastUsed = active.reduce<string | undefined>(
    (latest, grant) =>
      grant.last_used_at && (!latest || Date.parse(grant.last_used_at) > Date.parse(latest))
        ? grant.last_used_at
        : latest,
    undefined,
  )
  const unavailable = grantsUnavailable(grants.error)
  const summary = (
    <>
      {grants.isPending && <SummaryLine>Reading your connected clients…</SummaryLine>}
      {grants.isError && (
        <SummaryLine>
          {unavailable ? 'This server does not offer MCP sign-in.' : 'Conveyor could not read your connected clients.'}
        </SummaryLine>
      )}
      {grants.isSuccess && active.length === 0 && <SummaryLine>No MCP client is connected.</SummaryLine>}
      {grants.isSuccess && active.length > 0 && (
        <SummaryLine>
          <span className="text-foreground">{active.length === 1 ? '1 client' : `${active.length} clients`}</span>
          {' · '}
          {lastUsed ? `Last used ${fullTime(lastUsed)}` : 'Never used'}
        </SummaryLine>
      )}
    </>
  )

  return (
    <SettingsRow icon={Unplug} title="Connected MCP clients" action="Manage" summary={summary}>
      <div className="space-y-3">
        <p className="text-sm leading-6 text-muted">
          These coding agents signed in to Conveyor's MCP endpoint as you. Revoke a client to stop it at once. It must
          sign in again to reconnect.
        </p>
        {grants.isError && (
          <p className="text-sm text-failure" role="alert">
            {unavailable
              ? 'This server does not offer MCP sign-in, so no client can connect this way.'
              : errorMessage(grants.error, 'Could not read your connected clients.')}
          </p>
        )}
        {grants.isSuccess && active.length === 0 && <p className="text-sm text-faint">No MCP client is connected.</p>}
        {active.map((grant) => (
          <div
            key={grant.id}
            className="flex items-center justify-between gap-3 rounded-md border border-border p-3"
            data-testid="oauth-grant"
          >
            <div className="min-w-0">
              <p className="truncate text-sm font-medium">{clientLabel(grant.client_name)}</p>
              <p className="break-words text-xs text-muted">{grantLifecycle(grant.created_at, grant.last_used_at)}</p>
            </div>
            <Button
              size="sm"
              variant="destructive"
              className="shrink-0"
              aria-label={`Revoke ${clientLabel(grant.client_name)}`}
              disabled={revoke.isPending}
              onClick={() => revoke.mutate(grant.id)}
            >
              {revoke.isPending && revoke.variables === grant.id ? 'Revoking…' : 'Revoke'}
            </Button>
          </div>
        ))}
        {revoke.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(revoke.error, 'Could not revoke that client.')}
          </p>
        )}
      </div>
    </SettingsRow>
  )
}
