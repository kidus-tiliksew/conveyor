import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { fetchRequirements, fetchSystemDesigns, updateTaskContext } from '../../lib/api'
import type { Task } from '../../lib/types'
import { errorMessage } from '../../lib/errors'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { TaskContextPicker } from './task-context-picker'

export function TaskContextAttachmentDialog({
  task,
  token,
  onClose,
}: {
  task: Task
  token: string
  onClose: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const initialRequirements = task.context?.requirements?.map((item) => item.id) ?? []
  const initialDesigns = task.context?.designs?.map((item) => item.id) ?? []
  const [requirements, setRequirements] = useState<string[]>([])
  const [designs, setDesigns] = useState<string[]>([])
  const requirementQuery = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const designQuery = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const mutation = useMutation({
    mutationFn: () =>
      updateTaskContext(token, task.id, {
        add: {
          requirement_ids: requirements,
          system_design_ids: designs,
        },
        remove: {},
      }),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', task.id] }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: ['task-operations'] }),
      ])
      onClose()
    },
  })
  const changed = requirements.length > 0 || designs.length > 0

  return (
    <Dialog label="Attach task context" onClose={() => !mutation.isPending && onClose()}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">Attach context to this task</h2>
        <p className="mt-1 text-sm leading-6 text-muted">
          Choose confirmed documents the next work-order attempt should receive. This does not recover the task.
        </p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <TaskContextPicker
          label="Context"
          hint="Already attached documents stay selected. Add one or more confirmed documents."
          loading={requirementQuery.isLoading || designQuery.isLoading}
          groups={[
            {
              key: 'requirements',
              label: 'Requirements',
              options: (requirementQuery.data ?? [])
                .filter((item) => item.current_version && !initialRequirements.includes(item.requirement.id))
                .map((item) => ({ id: item.requirement.id, title: item.requirement.title })),
              selected: requirements,
              onChange: setRequirements,
            },
            {
              key: 'designs',
              label: 'System Design',
              options: (designQuery.data ?? [])
                .filter((item) => item.current_version && !initialDesigns.includes(item.document.id))
                .map((item) => ({ id: item.document.id, title: item.document.title })),
              selected: designs,
              onChange: setDesigns,
            },
          ]}
        />
        {(requirementQuery.error || designQuery.error) && (
          <p className="text-sm text-failure">Could not load confirmed context.</p>
        )}
        {mutation.error && (
          <p className="text-sm text-failure">{errorMessage(mutation.error, 'Could not attach context.')}</p>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            Cancel
          </Button>
          <Button onClick={() => mutation.mutate()} disabled={!changed || mutation.isPending}>
            {mutation.isPending ? 'Attaching…' : 'Attach selected context'}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
