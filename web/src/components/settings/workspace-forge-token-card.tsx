import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Building2 } from 'lucide-react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'
import { deleteWorkspaceForgeToken, fetchWorkspaceForgeToken, storeWorkspaceForgeToken } from '../../lib/api'
import { errorMessage } from '../../lib/errors'

function storedSummary(storedAt?: string) {
  return storedAt ? `Stored ${new Date(storedAt).toLocaleString()}` : 'Stored time unavailable'
}

export function WorkspaceForgeTokenCard() {
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [forgeToken, setForgeToken] = useState('')
  const queryKey = ['workspace-forge-token', workspace]

  const status = useQuery({
    queryKey,
    queryFn: () => fetchWorkspaceForgeToken(workspace),
    enabled: canManageWorkspace && Boolean(workspace),
    retry: false,
  })
  const store = useMutation({
    mutationFn: (value: string) => storeWorkspaceForgeToken(workspace, value),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey })
    },
    onSettled: () => setForgeToken(''),
  })
  const remove = useMutation({
    mutationFn: () => deleteWorkspaceForgeToken(workspace),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey })
    },
  })

  if (!canManageWorkspace || !workspace) return null

  const pending = store.isPending || remove.isPending
  const current = status.data
  const configured = current?.configured === true

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Workspace GitHub token</CardTitle>
        <Building2 className="size-4 text-faint" />
      </CardHeader>
      <CardContent className="space-y-4">
        {status.isPending && <p className="text-sm text-muted">Loading the workspace GitHub token status…</p>}
        {status.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(status.error, 'Could not load the workspace GitHub token status.')}
          </p>
        )}
        {status.isSuccess && (
          <div className="space-y-1">
            {!configured && (
              <p className="text-sm font-medium text-attention">
                Store the token used for workspace-level GitHub acts.
              </p>
            )}
            <p className="text-sm leading-6 text-muted">
              Create a fine-grained token with repository permissions for Contents read and write, Pull requests read
              and write, and Issues read and write.
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
            aria-label={configured ? 'Replace workspace GitHub token' : 'Store workspace GitHub token'}
            onSubmit={(event) => {
              event.preventDefault()
              store.mutate(forgeToken)
            }}
          >
            <Input
              required
              type="password"
              autoComplete="off"
              aria-label="Workspace GitHub token"
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
                {errorMessage(store.error, 'Could not store that workspace GitHub token.')}
              </p>
            )}
          </form>
        )}
        {remove.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(remove.error, 'Could not delete the workspace GitHub token.')}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
