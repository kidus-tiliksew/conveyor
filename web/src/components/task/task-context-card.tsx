import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, X } from 'lucide-react'
import { useState } from 'react'
import { resolveTaskContextProposal, updateTaskContext } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { TaskContext, TaskContextProposal, TaskState } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { SuccessorLinks } from '../documents/successor-links'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'

// The task's pinned authority: the confirmed outcomes and technical guidance
// the factory serves to this task's sessions. Rows, not prose — the link is
// the point and the version pin is the caveat.
export function TaskContextCard({
  taskId,
  taskState,
  context,
}: {
  taskId: string
  taskState: TaskState
  context?: TaskContext
}) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const canRemove = canOperate && taskState !== 'merged' && taskState !== 'closed'
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [selected, setSelected] = useState<{
    id: string
    title: string
    version: number
    kind: 'requirement_ids' | 'system_design_ids'
  } | null>(null)
  const removal = useMutation({
    mutationFn: (document: NonNullable<typeof selected>) =>
      updateTaskContext(taskId, { add: {}, remove: { [document.kind]: [document.id] } }),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', workspace, taskId] }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: ['task-operations'] }),
      ])
      setSelected(null)
    },
  })
  const removeControl = (
    item: { id: string; title: string; version: number },
    kind: 'requirement_ids' | 'system_design_ids',
  ) =>
    canRemove && (
      <Button
        size="sm"
        variant="ghost"
        aria-label={`Remove context ${item.title}`}
        disabled={removal.isPending}
        onClick={() => {
          removal.reset()
          setSelected({ ...item, kind })
        }}
      >
        <X /> Remove
      </Button>
    )
  const requirements = context?.requirements ?? []
  const designs = context?.designs ?? []
  const proposals = context?.proposals ?? []
  if (requirements.length === 0 && designs.length === 0 && proposals.length === 0) return null
  return (
    <div className="space-y-3">
      {(requirements.length > 0 || designs.length > 0) && (
        <section aria-label="Attached context" className="rounded-md border border-border bg-card">
          <h3 className="border-b border-border px-4 py-2.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            Attached context
          </h3>
          <ul className="px-4 py-2">
            {requirements.map((item) => (
              <ContextRow
                key={item.id}
                kind="Outcome"
                meta={`${item.id} · v${item.version}`}
                action={removeControl(item, 'requirement_ids')}
              >
                <Link
                  to="/requirements"
                  search={{ requirement: item.id }}
                  className="truncate text-primary hover:underline"
                >
                  {item.title}
                </Link>
                {item.archived && (
                  <span className="group relative">
                    <Badge variant="outline">Archived</Badge>
                    {item.superseded_by?.length && (
                      <span className="invisible absolute left-0 top-full z-20 mt-1 w-max max-w-80 rounded-md border border-border bg-background px-3 py-2 opacity-0 shadow-lg group-hover:visible group-hover:opacity-100">
                        <SuccessorLinks ids={item.superseded_by} compact />
                      </span>
                    )}
                  </span>
                )}
              </ContextRow>
            ))}
            {designs.map((item) => (
              <ContextRow
                key={item.id}
                kind="Guidance"
                meta={`${item.id} · v${item.version}`}
                action={removeControl(item, 'system_design_ids')}
              >
                <Link
                  to="/system-design"
                  search={{ document: item.id }}
                  className="truncate text-primary hover:underline"
                >
                  {item.title}
                </Link>
                {item.archived && (
                  <span className="group relative">
                    <Badge variant="outline">Archived</Badge>
                    {item.superseded_by?.length && (
                      <span className="invisible absolute left-0 top-full z-20 mt-1 w-max max-w-80 rounded-md border border-border bg-background px-3 py-2 opacity-0 shadow-lg group-hover:visible group-hover:opacity-100">
                        <SuccessorLinks ids={item.superseded_by} compact />
                      </span>
                    )}
                  </span>
                )}
              </ContextRow>
            ))}
          </ul>
        </section>
      )}
      {selected && canRemove && (
        <Dialog label="Remove task context" onClose={() => !removal.isPending && setSelected(null)}>
          <div className="border-b border-border px-5 py-4">
            <h2 className="font-semibold">Remove this context document?</h2>
            <p className="mt-1 text-sm leading-6 text-muted">
              {selected.title} · v{selected.version}
            </p>
            <p className="mt-1 text-sm leading-6 text-muted">
              This removes the attachment from this task. The document and task history remain readable.
            </p>
          </div>
          <div className="space-y-4 px-5 py-4">
            {removal.error && (
              <p className="text-sm text-failure">{errorMessage(removal.error, 'Could not remove context.')}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" disabled={removal.isPending} onClick={() => setSelected(null)}>
                Cancel
              </Button>
              <Button variant="destructive" disabled={removal.isPending} onClick={() => removal.mutate(selected)}>
                {removal.isPending ? 'Removing…' : 'Remove context'}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
      {proposals.length > 0 && <SuggestedContextCard taskId={taskId} proposals={proposals} />}
    </div>
  )
}

function SuggestedContextCard({ taskId, proposals }: { taskId: string; proposals: TaskContextProposal[] }) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const resolve = useMutation({
    mutationFn: ({ proposal, action }: { proposal: TaskContextProposal; action: 'confirm' | 'dismiss' }) =>
      resolveTaskContextProposal(taskId, proposal.target_kind, proposal.target_id, action),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', workspace, taskId] }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
      ])
    },
  })

  return (
    <section aria-label="Suggested context" className="rounded-md border border-border bg-card">
      <div className="border-b border-border px-4 py-2.5">
        <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Suggested context</h3>
        <p className="mt-1 text-xs text-faint">Review documents suggested for this task.</p>
      </div>
      <ul className="divide-y divide-border">
        {proposals.map((proposal) => {
          const key = `${proposal.target_kind}:${proposal.target_id}`
          const active =
            resolve.variables != null &&
            `${resolve.variables.proposal.target_kind}:${resolve.variables.proposal.target_id}` === key
          return (
            <li key={key} className="px-4 py-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <Badge>{proposal.target_kind === 'requirement' ? 'Outcome' : 'Guidance'}</Badge>
                <Badge variant="mono">From {sourceLabel(proposal.source)}</Badge>
              </div>
              <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
                {proposal.target_kind === 'requirement' ? (
                  <Link
                    to="/requirements"
                    search={{ requirement: proposal.target_id }}
                    className="font-medium text-primary hover:underline"
                  >
                    {proposal.target_title}
                  </Link>
                ) : (
                  <Link
                    to="/system-design"
                    search={{ document: proposal.target_id }}
                    className="font-medium text-primary hover:underline"
                  >
                    {proposal.target_title}
                  </Link>
                )}
                <span className="font-mono text-[11px] text-faint">{proposal.target_id}</span>
              </div>
              <p className="mt-2 text-xs leading-5 text-muted">{proposal.justification}</p>
              {resolve.error && active && (
                <p className="mt-2 text-xs text-failure">
                  {errorMessage(resolve.error, 'Could not resolve this suggestion.')}
                </p>
              )}
              {canOperate && (
                <div className="mt-3 flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    disabled={resolve.isPending}
                    onClick={() => resolve.mutate({ proposal, action: 'confirm' })}
                  >
                    <Check />
                    {active && resolve.isPending && resolve.variables?.action === 'confirm'
                      ? 'Confirming…'
                      : 'Attach context'}
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    disabled={resolve.isPending}
                    onClick={() => resolve.mutate({ proposal, action: 'dismiss' })}
                  >
                    <X />
                    {active && resolve.isPending && resolve.variables?.action === 'dismiss' ? 'Dismissing…' : 'Dismiss'}
                  </Button>
                </div>
              )}
            </li>
          )
        })}
      </ul>
    </section>
  )
}

function sourceLabel(source: TaskContextProposal['source']) {
  if (source === 'triage') return 'task intake'
  if (source === 'planning') return 'planning'
  return 'an operator'
}

function ContextRow({
  kind,
  meta,
  action,
  children,
}: {
  kind: string
  meta: string
  action: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <li className="flex items-baseline gap-3 py-1.5 text-sm">
      <span className="w-16 shrink-0 text-[10px] font-medium uppercase tracking-wider text-faint">{kind}</span>
      {children}
      <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">{meta}</span>
      {action}
    </li>
  )
}
