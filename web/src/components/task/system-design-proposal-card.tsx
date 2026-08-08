import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, FileText } from 'lucide-react'
import { confirmSystemDesignVersion, fetchSystemDesigns, SystemDesignConflictError } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { SystemDesignVersion, SystemDesignView } from '../../lib/types'
import { useOperatorToken, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

interface Proposal {
  document: SystemDesignView['document']
  version: SystemDesignVersion
  /** The confirmed version the document is on, for the confirm route's If-Match. */
  expected: number
}

// Document plus version, not the object reference: a refetch rebuilds these
// records, and the in-flight and failed states have to keep pointing at the
// same proposal across it.
function identity(proposal: Proposal) {
  return `${proposal.document.id}:${proposal.version.version}`
}

// A task's own unresolved proposals, and nothing else. Scoping by origin is
// what keeps §21.62 a carve-out rather than a second attention surface: this
// task renders what it raised, never another task's pending versions.
//
// The collection is shared with the task and board filters, which read only
// each document's identity, so neither the pending list nor its resolution
// flags are assumed present — a partial payload leaves a task with no card,
// never a detail surface that fails to render.
function selfOriginated(taskId: string, designs: SystemDesignView[]): Proposal[] {
  return designs.flatMap((item) =>
    (item.pending_versions ?? [])
      .filter((version) => version.origin_task_id === taskId && !version.confirmed && !version.dismissed)
      .map((version) => ({ document: item.document, version, expected: item.document.current_version ?? 0 })),
  )
}

/**
 * The proposals this task raised, over the existing System Design read API.
 * The timeline asks before it renders a tail slot, so a task with nothing
 * pending grows no empty row; the query key is the one the document surfaces
 * and the task filters already use, so this is normally a cache read.
 */
export function useSystemDesignProposals(taskId: string): Proposal[] {
  const { workspace } = useWorkspaceSelection()
  const designs = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
  })
  return selfOriginated(taskId, designs.data ?? [])
}

/**
 * The origin-task proposal card (spec §21.62). When an implement session
 * proposes a System Design revision, the confirm affordance used to live only
 * on the per-document attention surface — a different page from the task the
 * pipeline was waiting on. This renders the same decision where it was raised.
 *
 * Presentation only: the read is the existing `/v1/system-designs` collection
 * filtered client-side, and Confirm posts to the same operator-credentialed
 * route the attention surface uses. Agents still never confirm. A resolved
 * version stops matching the filter, so the card clears itself — no new state,
 * and the timeline already records what happened.
 *
 * §21.61 change 1 is otherwise untouched: the document canvas keeps its
 * attention surface as the only document-side rendering, and drift and
 * staleness are not rendered here at all.
 */
export function SystemDesignProposalCard({ taskId }: { taskId: string }) {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const proposals = useSystemDesignProposals(taskId)
  const confirm = useMutation({
    mutationFn: (proposal: Proposal) =>
      confirmSystemDesignVersion(token, proposal.document.id, proposal.version.version, proposal.expected),
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
      aria-label="System Design proposals from this task"
      className="space-y-3 rounded-lg border border-attention/40 bg-attention-soft px-3 py-3"
    >
      {proposals.map((proposal) => {
        const active = confirm.variables != null && identity(confirm.variables) === identity(proposal)
        return (
          <div key={identity(proposal)} className="flex items-start gap-2">
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
              <Button size="sm" disabled={!token || confirm.isPending} onClick={() => confirm.mutate(proposal)}>
                <Check />
                {active && confirm.isPending ? 'Confirming…' : `Confirm version ${proposal.version.version}`}
              </Button>
              {confirm.error != null && active && (
                <p className="text-failure">{errorMessage(confirm.error, 'Could not confirm this version.')}</p>
              )}
            </div>
          </div>
        )
      })}
    </section>
  )
}
