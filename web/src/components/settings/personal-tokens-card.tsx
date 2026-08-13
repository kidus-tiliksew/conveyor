import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus } from 'lucide-react'
import { useOperatorToken } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { Input } from '../ui/input'
import { fetchPersonalAccessTokens, issuePersonalAccessToken, revokePersonalAccessToken } from '../../lib/api'
import { errorMessage } from '../../lib/errors'

function lifecycleSummary(created: string, lastUsed?: string) {
  const createdText = `Created ${new Date(created).toLocaleString()}`
  return lastUsed ? `${createdText} · Last used ${new Date(lastUsed).toLocaleString()}` : `${createdText} · Never used`
}

/**
 * The signed-in person's own access tokens.
 *
 * A freshly issued value lives in component state and nowhere else: it is never
 * written to a query cache, never stored, and no listing response carries it,
 * so leaving this screen is the same as losing it.
 */
export function PersonalTokensCard() {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [label, setLabel] = useState('')
  const [issuedValue, setIssuedValue] = useState('')

  const tokens = useQuery({
    queryKey: ['personal-access-tokens', token],
    queryFn: () => fetchPersonalAccessTokens(token),
    enabled: Boolean(token),
  })
  const issue = useMutation({
    mutationFn: () => issuePersonalAccessToken(token, label.trim()),
    onSuccess: async (created) => {
      setIssuedValue(created.value)
      setLabel('')
      await queryClient.invalidateQueries({ queryKey: ['personal-access-tokens', token] })
    },
  })
  const revoke = useMutation({
    mutationFn: (id: string) => revokePersonalAccessToken(token, id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['personal-access-tokens', token] })
    },
  })

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
              <p className="truncate text-sm font-medium">{item.label}</p>
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
                  onClick={() => revoke.mutate(item.id)}
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
    </Card>
  )
}
