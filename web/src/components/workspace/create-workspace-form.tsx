import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { createWorkspace } from '../../lib/api'
import type { WorkspaceRecord } from '../../lib/types'
import { useDashboardSession, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input } from '../ui/input'

// The immutable workspace id (§21.10: [a-z0-9][a-z0-9-]{0,62}) is derived
// from the display name rather than asked for; config comes later through
// the Workspace page, not at creation time.
function slugify(name: string) {
  return name
    .toLowerCase()
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63)
    .replace(/-+$/, '')
}

export function CreateWorkspaceForm({
  onCreated,
  onCancel,
}: {
  onCreated?: (created: WorkspaceRecord) => void
  onCancel?: () => void
}) {
  const token = useDashboardSession()
  const queryClient = useQueryClient()
  const { setWorkspace } = useWorkspaceSelection()
  const [name, setName] = useState('')
  const id = slugify(name)
  const create = useMutation({
    mutationFn: () => createWorkspace({ id, name: name.trim() }),
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({ queryKey: ['workspaces'] })
      setWorkspace(created.id)
      onCreated?.(created)
    },
  })

  if (!token) {
    return (
      <p className="rounded-md border border-border p-3 text-sm text-muted">
        Sign in with an account that can manage workspaces to create one.
      </p>
    )
  }

  return (
    <div className="space-y-4">
      <label className="block" htmlFor="workspace-name">
        <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">Name</span>
        <Input
          id="workspace-name"
          autoFocus
          value={name}
          placeholder="Engineering"
          onChange={(event) => setName(event.target.value)}
        />
        <span className="mt-1 block text-xs text-faint">
          {id ? (
            <>
              ID <code className="font-mono">{id}</code> — used in URLs and the CLI. Repositories and routing are
              configured afterwards on the Workspace page.
            </>
          ) : (
            'Repositories and routing are configured afterwards on the Workspace page.'
          )}
        </span>
      </label>
      {create.error != null && <p className="text-sm text-failure">{String(create.error)}</p>}
      <div className="flex items-center justify-end gap-2 border-t border-border pt-4">
        {onCancel && (
          <Button variant="secondary" onClick={onCancel}>
            Cancel
          </Button>
        )}
        <Button disabled={!id || !name.trim() || create.isPending} onClick={() => create.mutate()}>
          {create.isPending ? 'Creating…' : 'Create workspace'}
        </Button>
      </div>
    </div>
  )
}
