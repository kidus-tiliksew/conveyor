import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Building2 } from 'lucide-react'
import { useEffect, useId, useState } from 'react'
import { createWorkspaceGitHubAppManifest, disconnectWorkspaceGitHubApp, fetchWorkspaceGitHubApp } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'

export function WorkspaceGitHubAppCard() {
  const organizationID = useId()
  const canManage = useWorkspaceCapability('manage_workspace')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [organization, setOrganization] = useState('')
  const [confirmDisconnect, setConfirmDisconnect] = useState(false)
  const queryKey = ['workspace-github-app', workspace]
  const status = useQuery({
    queryKey,
    queryFn: () => fetchWorkspaceGitHubApp(workspace),
    enabled: canManage && Boolean(workspace),
    retry: false,
    staleTime: 0,
    refetchOnMount: 'always',
  })
  const { refetch } = status
  useEffect(() => {
    if (!canManage || !workspace) return
    // Refresh even when the browser restores settings from its back/forward cache.
    const refresh = () => void refetch()
    window.addEventListener('pageshow', refresh)
    return () => window.removeEventListener('pageshow', refresh)
  }, [canManage, workspace, refetch])
  const connect = useMutation({
    mutationFn: async (org: string) => {
      const response = await createWorkspaceGitHubAppManifest(workspace)
      // DEC-41: send the public manifest directly to GitHub, never to a query cache or storage.
      const target = org
        ? `https://github.com/organizations/${encodeURIComponent(org)}/settings/apps/new`
        : 'https://github.com/settings/apps/new'
      const form = document.createElement('form')
      form.method = 'POST'
      form.action = `${target}?state=${encodeURIComponent(response.state)}`
      form.target = '_top'
      form.hidden = true
      const manifest = document.createElement('input')
      manifest.type = 'hidden'
      manifest.name = 'manifest'
      manifest.value = JSON.stringify(response.manifest)
      form.append(manifest)
      document.body.append(form)
      try {
        form.submit()
      } finally {
        form.remove()
      }
    },
  })
  const disconnect = useMutation({
    mutationFn: () => disconnectWorkspaceGitHubApp(workspace),
    onSuccess: async () => {
      setConfirmDisconnect(false)
      await queryClient.invalidateQueries({ queryKey })
    },
  })
  if (!canManage || !workspace) return null
  const current = status.data
  const pending = connect.isPending || disconnect.isPending
  const closeDialog = () => {
    if (!disconnect.isPending) setConfirmDisconnect(false)
  }
  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Workspace GitHub App</CardTitle>
        <Building2 className="size-4 text-faint" />
      </CardHeader>
      <CardContent className="space-y-4">
        {status.isPending && <p className="text-sm text-muted">Loading GitHub connection…</p>}
        {status.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(status.error)}
          </p>
        )}
        {status.isSuccess && !current?.connected && (
          <>
            <p className="text-sm leading-6 text-muted">
              Conveyor needs its own GitHub identity for this workspace. Connect an app and choose the repositories it
              can access.
            </p>
            <form
              className="flex flex-wrap items-end gap-2"
              aria-label="Connect GitHub"
              onSubmit={(event) => {
                event.preventDefault()
                connect.mutate(organization.trim())
              }}
            >
              <label htmlFor={organizationID} className="space-y-1 text-sm">
                <span>Organization (optional)</span>
                <Input
                  id={organizationID}
                  value={organization}
                  onChange={(event) => setOrganization(event.target.value)}
                  disabled={pending}
                  autoComplete="off"
                  placeholder="Organization name"
                />
              </label>
              <Button type="submit" disabled={pending}>
                {connect.isPending ? 'Connecting…' : 'Connect GitHub'}
              </Button>
            </form>
          </>
        )}
        {current?.connected && (
          <>
            <div>
              <p className="text-sm font-medium">{current.app_slug}</p>
              <p className="text-sm text-muted">
                {current.installation_account ? `Installed on ${current.installation_account}` : 'Not installed yet.'}
              </p>
            </div>
            <ul className="space-y-2" aria-label="Repository coverage">
              {current.repositories.map((repository) => (
                <li
                  key={repository.name}
                  className="flex flex-wrap items-center gap-2 rounded-md border border-border p-3 text-sm"
                >
                  <span className="font-medium">{repository.name}</span>
                  <span className={repository.covered ? 'text-positive' : 'text-attention'}>
                    {repository.covered ? 'Covered' : 'Not covered'}
                  </span>
                  {!repository.covered && current.installation_url && (
                    <a className="text-primary underline" href={current.installation_url}>
                      Install on GitHub
                    </a>
                  )}
                </li>
              ))}
            </ul>
            {current.repositories.length === 0 && <p className="text-sm text-muted">No repositories registered.</p>}
            <div className="flex items-center gap-3">
              <a
                className="text-sm text-primary underline"
                href={
                  current.installation_url ??
                  `https://github.com/apps/${encodeURIComponent(current.app_slug)}/installations/new`
                }
              >
                Manage on GitHub
              </a>
              <Button
                type="button"
                variant="destructive"
                disabled={pending}
                onClick={() => {
                  disconnect.reset()
                  setConfirmDisconnect(true)
                }}
              >
                Disconnect
              </Button>
            </div>
          </>
        )}
        {connect.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(connect.error)}
          </p>
        )}
      </CardContent>
      {confirmDisconnect && (
        <Dialog label="Disconnect GitHub App" onClose={closeDialog}>
          <div className="space-y-4 p-5">
            <h2 className="text-base font-semibold">Disconnect GitHub App?</h2>
            <p className="text-sm leading-6 text-muted">
              Conveyor will lose access to this workspace’s GitHub repositories. You can connect an app again later.
            </p>
            {disconnect.error && (
              <p className="text-sm text-failure" role="alert">
                {errorMessage(disconnect.error)}
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" disabled={pending} onClick={closeDialog}>
                Cancel
              </Button>
              <Button type="button" variant="destructive" disabled={pending} onClick={() => disconnect.mutate()}>
                {disconnect.isPending ? 'Disconnecting…' : 'Disconnect'}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </Card>
  )
}
