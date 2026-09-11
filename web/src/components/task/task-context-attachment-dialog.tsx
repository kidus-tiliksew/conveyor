import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchRequirements, fetchSystemDesigns, updateTaskContext } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { Task } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { SuccessorLinks } from '../documents/successor-links'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { TaskContextPicker } from './task-context-picker'

export function TaskContextAttachmentDialog({ task, onClose }: { task: Task; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const canOperate = useWorkspaceCapability('operate_gates')
  const canEdit = canOperate && task.state !== 'merged' && task.state !== 'closed'
  const initialRequirements = task.context?.requirements?.map((item) => item.id) ?? []
  const initialDesigns = task.context?.designs?.map((item) => item.id) ?? []
  const [requirements, setRequirements] = useState<string[]>([])
  const [designs, setDesigns] = useState<string[]>([])
  const [removedRequirements, setRemovedRequirements] = useState<string[]>([])
  const [removedDesigns, setRemovedDesigns] = useState<string[]>([])
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
  // Normalize against refreshed task context as well as local picker transitions.
  const addedRequirements = requirements.filter((id) => !initialRequirements.includes(id))
  const addedDesigns = designs.filter((id) => !initialDesigns.includes(id))
  const removingRequirements = removedRequirements.filter((id) => initialRequirements.includes(id))
  const removingDesigns = removedDesigns.filter((id) => initialDesigns.includes(id))
  const mutation = useMutation({
    mutationFn: () =>
      updateTaskContext(task.id, {
        add: {
          requirement_ids: addedRequirements,
          system_design_ids: addedDesigns,
        },
        remove: { requirement_ids: removingRequirements, system_design_ids: removingDesigns },
      }),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', workspace, task.id] }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: ['task-operations'] }),
      ])
      onClose()
    },
  })
  const hasRemovals = removingRequirements.length > 0 || removingDesigns.length > 0
  const changed = addedRequirements.length > 0 || addedDesigns.length > 0 || hasRemovals

  return (
    <Dialog label="Manage task context" onClose={() => !mutation.isPending && onClose()}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">Manage task context</h2>
        <p className="mt-1 text-sm leading-6 text-muted">
          Choose confirmed documents the next work-order attempt should receive. This does not recover the task.
        </p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <section aria-label="Attached documents" className="space-y-2">
          <h3 className="text-sm font-medium">Attached documents</h3>
          {[
            {
              key: 'requirements',
              items: task.context?.requirements ?? [],
              removed: removedRequirements,
              setRemoved: setRemovedRequirements,
            },
            {
              key: 'designs',
              items: task.context?.designs ?? [],
              removed: removedDesigns,
              setRemoved: setRemovedDesigns,
            },
          ].map((group) =>
            group.items.map((item) => (
              <div key={`${group.key}:${item.id}`} className="flex items-center gap-2 text-sm">
                <span className="min-w-0 flex-1">
                  {item.title} · v{item.version}
                </span>
                {item.archived && (
                  <span className="flex flex-wrap items-center gap-1">
                    <Badge variant="outline">Archived</Badge>
                    {!!item.superseded_by?.length && <SuccessorLinks ids={item.superseded_by} compact />}
                  </span>
                )}
                {canEdit && (
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={mutation.isPending}
                    aria-label={`${group.removed.includes(item.id) ? 'Keep' : 'Remove'} context ${item.title}`}
                    onClick={() =>
                      group.setRemoved((ids) =>
                        ids.includes(item.id) ? ids.filter((id) => id !== item.id) : [...ids, item.id],
                      )
                    }
                  >
                    {group.removed.includes(item.id) ? 'Keep attached' : 'Remove'}
                  </Button>
                )}
              </div>
            )),
          )}
          {initialRequirements.length === 0 && initialDesigns.length === 0 && (
            <p className="text-sm text-muted">No documents attached.</p>
          )}
        </section>
        {canEdit && (
          <fieldset disabled={mutation.isPending}>
            <TaskContextPicker
              label="Context"
              hint="Add confirmed documents, or reselect a document to keep it attached."
              loading={requirementQuery.isLoading || designQuery.isLoading}
              groups={[
                {
                  key: 'requirements',
                  label: 'Requirements',
                  options: (requirementQuery.data ?? [])
                    .filter(
                      (item) =>
                        item.current_version &&
                        !item.requirement.archived &&
                        (!initialRequirements.includes(item.requirement.id) ||
                          removedRequirements.includes(item.requirement.id)),
                    )
                    .map((item) => ({ id: item.requirement.id, title: item.requirement.title })),
                  selected: addedRequirements,
                  onChange: (ids) => {
                    setRequirements(ids.filter((id) => !initialRequirements.includes(id)))
                    setRemovedRequirements((removed) => removed.filter((id) => !ids.includes(id)))
                  },
                },
                {
                  key: 'designs',
                  label: 'System Design',
                  options: (designQuery.data ?? [])
                    .filter(
                      (item) =>
                        item.current_version &&
                        !item.document.archived &&
                        (!initialDesigns.includes(item.document.id) || removedDesigns.includes(item.document.id)),
                    )
                    .map((item) => ({ id: item.document.id, title: item.document.title })),
                  selected: addedDesigns,
                  onChange: (ids) => {
                    setDesigns(ids.filter((id) => !initialDesigns.includes(id)))
                    setRemovedDesigns((removed) => removed.filter((id) => !ids.includes(id)))
                  },
                },
              ]}
            />
          </fieldset>
        )}
        {(requirementQuery.error || designQuery.error) && (
          <p className="text-sm text-failure">Could not load confirmed context.</p>
        )}
        {mutation.error && (
          <p className="text-sm text-failure">{errorMessage(mutation.error, 'Could not save context.')}</p>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            Cancel
          </Button>
          <Button onClick={() => mutation.mutate()} disabled={!canEdit || !changed || mutation.isPending}>
            {mutation.isPending ? 'Saving…' : hasRemovals ? 'Save context' : 'Attach selected context'}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
