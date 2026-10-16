import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { FileText, Target, X } from 'lucide-react'
import { useState } from 'react'
import { updateTaskContext } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { TaskContext, TaskContextProposal, TaskState } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { SuccessorLinks } from '../documents/successor-links'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { Disclosure } from '../ui/disclosure'
import { KindLabel, SectionHeading } from './section-heading'

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
        {/* Its aria-label names the document; below sm the icon alone leaves
            the row's ID room on the line beside it. */}
        <X /> <span className="max-sm:sr-only">Remove</span>
      </Button>
    )
  const requirements = context?.requirements ?? []
  const designs = context?.designs ?? []
  const proposals = context?.proposals ?? []
  if (requirements.length === 0 && designs.length === 0 && proposals.length === 0) return null
  return (
    <div className="space-y-3">
      {(requirements.length > 0 || designs.length > 0) && (
        <section aria-label="Attached context" className="rounded-lg border border-border bg-card">
          <SectionHeading className="border-b border-border px-4 py-3" count={requirements.length + designs.length}>
            Context
          </SectionHeading>
          <ul className="px-2 py-1.5">
            {requirements.map((item) => (
              <ContextRow
                key={item.id}
                kind="Outcome"
                version={item.version}
                action={removeControl(item, 'requirement_ids')}
              >
                <Link
                  to="/requirements"
                  search={{ requirement: item.id }}
                  aria-label={`${item.title} (${item.id})`}
                  className="truncate text-foreground hover:text-primary hover:underline focus-visible:text-primary"
                >
                  {item.title}
                </Link>
                {item.archived && (
                  <Disclosure
                    content={item.superseded_by?.length ? <SuccessorLinks ids={item.superseded_by} compact /> : null}
                    contentClassName="mt-1 max-w-80 text-inherit"
                  >
                    <Badge variant="outline">Archived</Badge>
                  </Disclosure>
                )}
              </ContextRow>
            ))}
            {designs.map((item) => (
              <ContextRow
                key={item.id}
                kind="Guidance"
                version={item.version}
                action={removeControl(item, 'system_design_ids')}
              >
                <Link
                  to="/system-design"
                  search={{ document: item.id }}
                  aria-label={`${item.title} (${item.id})`}
                  className="truncate text-foreground hover:text-primary hover:underline focus-visible:text-primary"
                >
                  {item.title}
                </Link>
                {item.archived && (
                  <Disclosure
                    content={item.superseded_by?.length ? <SuccessorLinks ids={item.superseded_by} compact /> : null}
                    contentClassName="mt-1 max-w-80 text-inherit"
                  >
                    <Badge variant="outline">Archived</Badge>
                  </Disclosure>
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
      {proposals.length > 0 && <SuggestedContextCard proposals={proposals} />}
    </div>
  )
}

// Triage's (or an operator's) suggestions, informational only: the
// next-action card's server choices are the one place each suggestion is
// attached or dismissed (component-web-task-surfaces, Next-action card).
function SuggestedContextCard({ proposals }: { proposals: TaskContextProposal[] }) {
  return (
    <section aria-label="Suggested context" className="rounded-lg border border-border bg-card">
      <div className="border-b border-border px-4 py-3">
        <SectionHeading count={proposals.length}>Suggested context</SectionHeading>
        <p className="mt-0.5 text-xs text-muted">Attach or dismiss each suggestion from the next-action card.</p>
      </div>
      <ul className="divide-y divide-border">
        {proposals.map((proposal) => {
          const requirement = proposal.target_kind === 'requirement'
          const Icon = requirement ? Target : FileText
          return (
            <li key={`${proposal.target_kind}:${proposal.target_id}`} className="px-4 py-3 text-sm">
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <Icon aria-hidden className={requirement ? 'size-3.5 text-primary' : 'size-3.5 text-faint'} />
                <Link
                  to={requirement ? '/requirements' : '/system-design'}
                  search={requirement ? { requirement: proposal.target_id } : { document: proposal.target_id }}
                  aria-label={`${proposal.target_title} (${proposal.target_id})`}
                  className="min-w-0 font-medium text-foreground [overflow-wrap:anywhere] hover:text-primary hover:underline"
                >
                  {proposal.target_title}
                </Link>
                <KindLabel>{requirement ? 'Outcome' : 'Guidance'}</KindLabel>
                <KindLabel>· From {sourceLabel(proposal.source)}</KindLabel>
              </div>
              <p className="mt-1 pl-5.5 text-[13px] leading-5 text-muted">{proposal.justification}</p>
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
  version,
  action,
  children,
}: {
  kind: 'Outcome' | 'Guidance'
  version: number
  action: React.ReactNode
  children: React.ReactNode
}) {
  const Icon = kind === 'Outcome' ? Target : FileText
  return (
    // One row: kind icon and label, the title, then the pinned version at the
    // end. Below sm the kind label drops and the row keeps one line.
    <li className="flex min-w-0 items-center gap-2.5 rounded-md px-2 py-1.5 text-sm hover:bg-surface">
      <Icon
        aria-hidden
        className={kind === 'Outcome' ? 'size-3.5 shrink-0 text-primary' : 'size-3.5 shrink-0 text-faint'}
      />
      <KindLabel className="w-14 max-sm:hidden">{kind}</KindLabel>
      <div className="flex min-w-0 flex-1 items-center gap-2">{children}</div>
      <span className="shrink-0 font-mono text-[11px] text-faint">v{version}</span>
      {action}
    </li>
  )
}
