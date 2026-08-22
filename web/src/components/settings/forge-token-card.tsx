import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound } from 'lucide-react'
import { useDashboardSession } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'
import { deleteForgeToken, fetchForgeToken, storeForgeToken } from '../../lib/api'
import { errorMessage } from '../../lib/errors'

function storedSummary(storedAt?: string) {
  return storedAt ? `Stored ${new Date(storedAt).toLocaleString()}` : 'Stored time unavailable'
}

export function ForgeTokenCard() {
  const token = useDashboardSession()
  const queryClient = useQueryClient()
  const [forgeToken, setForgeToken] = useState('')

  const status = useQuery({
    queryKey: ['forge-token', token],
    queryFn: () => fetchForgeToken(),
    enabled: Boolean(token),
    retry: false,
  })
  const store = useMutation({
    mutationFn: (value: string) => storeForgeToken(value),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['forge-token', token] })
    },
    onSettled: () => setForgeToken(''),
  })
  const remove = useMutation({
    mutationFn: () => deleteForgeToken(),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['forge-token', token] })
    },
  })

  if (!token) return null

  const pending = store.isPending || remove.isPending
  const current = status.data
  const configured = current?.configured === true

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>GitHub token</CardTitle>
        <KeyRound className="size-4 text-faint" />
      </CardHeader>
      <CardContent className="space-y-4">
        {status.isPending && <p className="text-sm text-muted">Loading your GitHub token status…</p>}
        {status.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(status.error, 'Could not load your GitHub token status.')}
          </p>
        )}
        {status.isSuccess && !configured && (
          <div className="space-y-1">
            <p className="text-sm font-medium text-attention">
              A GitHub token is required before you can execute tasks.
            </p>
            <p className="text-sm leading-6 text-muted">
              Create a fine-grained token with repository permissions for Contents read and write and Pull requests read
              and write.
            </p>
          </div>
        )}
        {configured && current && (
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border p-3">
            <div>
              <p className="text-sm font-medium">Connected as {current.forge_login}</p>
              <p className="text-xs text-muted" title={storedSummary(current.stored_at)}>
                {storedSummary(current.stored_at)}
              </p>
            </div>
            <Button type="button" size="sm" variant="destructive" disabled={pending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete token'}
            </Button>
          </div>
        )}
        {status.isSuccess && (
          <form
            className="flex flex-wrap items-end gap-2"
            aria-label={configured ? 'Replace GitHub token' : 'Store GitHub token'}
            onSubmit={(event) => {
              event.preventDefault()
              store.mutate(forgeToken)
            }}
          >
            <Input
              required
              type="password"
              autoComplete="off"
              aria-label="GitHub token"
              placeholder="github_pat_…"
              className="max-w-sm font-mono"
              value={forgeToken}
              disabled={pending}
              onChange={(event) => setForgeToken(event.target.value)}
            />
            <Button type="submit" disabled={!forgeToken.trim() || pending}>
              {store.isPending
                ? configured
                  ? 'Replacing…'
                  : 'Storing…'
                : configured
                  ? 'Replace token'
                  : 'Store token'}
            </Button>
            {store.error && (
              <p className="basis-full text-sm text-failure" role="alert">
                {errorMessage(store.error, 'Could not store that GitHub token.')}
              </p>
            )}
          </form>
        )}
        {remove.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(remove.error, 'Could not delete your GitHub token.')}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
