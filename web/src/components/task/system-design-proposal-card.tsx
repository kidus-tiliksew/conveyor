import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { AlertTriangle, Check, FileText } from 'lucide-react'
import { confirmSystemDesignVersion, fetchSystemDesigns, SystemDesignConflictError } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { SystemDesignSummary, SystemDesignVersionSummary, Task } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

export interface Proposal {
  document: SystemDesignSummary['document']
  version: SystemDesignVersionSummary
  /** The confirmed version the document is on, for the confirm route's If-Match. */
  expected: number
}

// Document plus version, not the object reference: a refetch rebuilds these
// records, and the in-flight and failed states have to keep pointing at the
// same proposal across it.
export function proposalIdentity(proposal: Proposal) {
  return `${proposal.document.id}:${proposal.version.version}`
}

// A task's own unresolved proposals on its own attached documents, and nothing
// else. Both halves of that scope are load-bearing. Origin is what keeps §21.62
// a carve-out rather than a second attention surface — this task renders what it
// raised, never another task's pending versions. Attachment is what keeps the
// carve-out inside the task's declared context: the read is the workspace-wide
// collection, so a proposal this task raised against a document it does not
// carry stays on the document's own attention surface, where it belongs.
//
// The collection is shared with the task and board filters, which read only
// each document's identity, so neither the pending list nor its resolution
// flags are assumed present — a partial payload leaves a task with no card,
// never a detail surface that fails to render.
function selfOriginated(task: Task, designs: SystemDesignSummary[]): Proposal[] {
  const attached = new Set((task.context?.designs ?? []).map((design) => design.id))
  return designs
    .filter((item) => attached.has(item.document.id))
    .flatMap((item) =>
      (item.pending_versions ?? [])
        .filter((version) => version.origin_task_id === task.id && !version.confirmed && !version.dismissed)
        .map((version) => ({ document: item.document, version, expected: item.document.current_version ?? 0 })),
    )
}

/**
 * The proposals this task raised on the documents it carries, over the existing
 * System Design read API. The timeline asks before it renders a tail slot, so a
 * task with nothing pending grows no empty row; the query key is the one the
 * document surfaces and the task filters already use, so this is normally a
 * cache read.
 */
export function useSystemDesignProposals(task: Task): Proposal[] {
  const { workspace } = useWorkspaceSelection()
  const designs = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  return selfOriginated(task, designs.data ?? [])
}

/**
 * The origin-task proposal card. When an implement session
 * proposes a System Design revision, the confirm affordance used to live only
 * on the per-document attention surface — a different page from the task the
 * pipeline was waiting on. This renders the same decision where it was raised.
 *
 * Presentation only: the read is the existing `/v1/system-designs` collection
 * narrowed client-side to this task's attached documents and its own pending
 * versions, and Confirm posts to the same operator-credentialed
 * route the attention surface uses. The action follows the same
 * `confirm_documents` capability as that route; agents still never confirm. A resolved
 * version stops matching the filter, so the card clears itself — no new state,
 * and the timeline already records what happened.
 *
 * §21.61 change 1 is otherwise untouched: the document canvas keeps its
 * attention surface as the only document-side rendering, and drift and
 * staleness are not rendered here at all.
 */
export function SystemDesignProposalCard({
  task,
  proposals,
  reviewWaiting = false,
}: {
  task: Task
  proposals: Proposal[]
  reviewWaiting?: boolean
}) {
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const confirm = useMutation({
    mutationFn: (proposal: Proposal) =>
      confirmSystemDesignVersion(proposal.document.id, proposal.version.version, proposal.expected),
    // Refetching the collection is what removes the card: the confirmed
    // version is no longer pending, so it stops matching. A conflict means
    // the document moved under us, and the refreshed list is the answer to
    // that too.
    onSettled: (_data, error) => {
      if (error == null || error instanceof SystemDesignConflictError)
        void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
    },
  })

  if (proposals.length === 0) return null

  return (
    <section
      aria-label={reviewWaiting ? 'Review is waiting on a document decision' : 'System Design proposals from this task'}
      className="space-y-3 rounded-lg border border-attention/40 bg-attention-soft px-3 py-3"
    >
      {reviewWaiting && (
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
          <div className="text-xs leading-5 text-muted">
            <p className="font-medium text-attention">Review is waiting on a System Design decision</p>
            <p>This review cannot be claimed until you confirm or dismiss the task&apos;s pending proposal.</p>
          </div>
        </div>
      )}
      {proposals.map((proposal) => {
        const active = confirm.variables != null && proposalIdentity(confirm.variables) === proposalIdentity(proposal)
        return (
          <div key={proposalIdentity(proposal)} className="flex items-start gap-2">
            <FileText className="mt-0.5 size-4 shrink-0 text-attention" />
            <div className="min-w-0 flex-1 space-y-2 text-xs leading-5 text-muted">
              <div>
                <p className="font-medium text-attention">System Design update proposed</p>
                <p>
                  Version {proposal.version.version} proposed by this task for{' '}
                  <Link
                    to="/system-design"
                    search={{ document: proposal.document.id }}
                    className="text-primary hover:underline"
                  >
                    {proposal.document.title}
                  </Link>
                  .
                </p>
              </div>
              <Button size="sm" disabled={!canConfirm || confirm.isPending} onClick={() => confirm.mutate(proposal)}>
                <Check />
                {active && confirm.isPending ? 'Confirming…' : `Confirm version ${proposal.version.version}`}
              </Button>
              {!canConfirm && <p>Document confirmation capability is required.</p>}
              {confirm.error != null && active && (
                <p className="text-failure">{errorMessage(confirm.error, 'Could not confirm this version.')}</p>
              )}
            </div>
          </div>
        )
      })}
      {reviewWaiting && (
        <Link
          to="/pending-proposals"
          search={{ task: task.id }}
          className="inline-block text-xs font-medium text-primary hover:underline"
        >
          Confirm or dismiss the proposal
        </Link>
      )}
    </section>
  )
}
