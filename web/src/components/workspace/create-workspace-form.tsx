import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { createWorkspace } from '../../lib/api'
import type { WorkspaceConfigDocument, WorkspaceRecord } from '../../lib/types'
import { useOperatorToken, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'

function parseInitialDocument(value: string): Partial<WorkspaceConfigDocument> | undefined {
  if (!value.trim()) return undefined
  const document: unknown = JSON.parse(value)
  if (!document || typeof document !== 'object' || Array.isArray(document)) throw new Error('Initial configuration must be a JSON object.')
  return document as Partial<WorkspaceConfigDocument>
}

// Workspace creation (spec §21.10): id + display name, with an optional
// initial config document; omitted fields inherit deployment defaults.
export function CreateWorkspaceForm({ onCreated }: { onCreated?: (created: WorkspaceRecord) => void }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const { setWorkspace } = useWorkspaceSelection()
  const [newID, setNewID] = useState('')
  const [newName, setNewName] = useState('')
  const [newDocument, setNewDocument] = useState('')
  const create = useMutation({
    mutationFn: () => createWorkspace(token, { id: newID.trim(), name: newName.trim(), document: parseInitialDocument(newDocument) }),
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({ queryKey: ['workspaces'] })
      setWorkspace(created.id)
      onCreated?.(created)
    },
  })

  if (!token) {
    return (
      <p className="rounded-lg border border-border p-3 text-sm text-muted">
        Set the operator token in{' '}
        <Link to="/settings" className="text-primary hover:underline">
          Settings
        </Link>{' '}
        to create a workspace.
      </p>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Create workspace</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 md:grid-cols-2">
        <Field label="Workspace ID">
          <Input value={newID} placeholder="engineering" onChange={(event) => setNewID(event.target.value.toLowerCase())} />
        </Field>
        <Field label="Display name">
          <Input value={newName} placeholder="Engineering" onChange={(event) => setNewName(event.target.value)} />
        </Field>
        <div className="md:col-span-2">
          <Field label="Initial configuration (optional JSON)">
            <textarea
              className="min-h-36 w-full rounded-lg border border-edge bg-background px-3 py-2 font-mono text-xs text-foreground placeholder:text-faint outline-none focus:border-primary"
              value={newDocument}
              placeholder={'{\n  "max_bounces": 3,\n  "work_order_queue_timeout": "48h",\n  "repos": []\n}'}
              onChange={(event) => setNewDocument(event.target.value)}
            />
            <span className="mt-1 block text-xs text-faint">All workspace document fields are accepted; omitted fields inherit deployment defaults.</span>
          </Field>
        </div>
        <Button disabled={!newID.trim() || !newName.trim() || create.isPending} onClick={() => create.mutate()}>
          <Plus />
          {create.isPending ? 'Creating…' : 'Create workspace'}
        </Button>
        {create.error != null && <p className="text-sm text-failure">{String(create.error)}</p>}
      </CardContent>
    </Card>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label>
      <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>
      {children}
    </label>
  )
}
