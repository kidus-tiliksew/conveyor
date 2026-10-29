import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { dismissDecisionSupersessionSweep } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { decisionsQueryKey } from '../../lib/query-keys'
import type { Decision, DecisionSupersessionSweepEntry } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Disclosure } from '../ui/disclosure'
import { formatDecisionDate } from './decision-model'

const statusLabels: Record<DecisionSupersessionSweepEntry['status'], string> = {
  open: 'Open',
  dismissed: 'Dismissed',
  auto_cleared: 'Cleared',
}

/**
 * The documents flagged for still citing the decision this one superseded
 * (req-document-operating-surfaces AC-8.6). Open flags list first, each with
 * its state and, for a caller with `confirm_documents`, Dismiss. Cleared and
 * dismissed flags collapse below them. The flags manage operator attention
 * only; they never block delivery, review, or merge.
 */
export function DecisionSweep({ decision }: { decision: Decision & { supersedes: string } }) {
  const { workspace } = useWorkspaceSelection()
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const client = useQueryClient()
  const dismiss = useMutation({
    mutationFn: (entry: DecisionSupersessionSweepEntry) =>
      dismissDecisionSupersessionSweep(workspace, decision.id, entry.document_tier, entry.document_id),
    onSettled: () => void client.invalidateQueries({ queryKey: decisionsQueryKey(workspace) }),
  })
  const entries = decision.sweep?.entries ?? []
  const open = entries.filter((entry) => entry.status === 'open')
  const cleared = entries.filter((entry) => entry.status !== 'open')
  const row = (entry: DecisionSupersessionSweepEntry) => {
    const pending =
      dismiss.isPending &&
      dismiss.variables?.document_tier === entry.document_tier &&
      dismiss.variables.document_id === entry.document_id
    const failed =
      dismiss.error &&
      dismiss.variables?.document_tier === entry.document_tier &&
      dismiss.variables.document_id === entry.document_id
    return (
      <li key={`${entry.document_tier}:${entry.document_id}`} className="py-2 text-xs">
        <div className="flex flex-wrap items-center gap-2">
          <SweepDocumentLink entry={entry} />
          <Badge>{sweepTierLabel(entry.document_tier)}</Badge>
          <Badge variant={entry.status === 'open' ? 'attention' : 'default'}>{statusLabels[entry.status]}</Badge>
          <Disclosure triggerClassName="text-faint" content={new Date(entry.detected_at).toLocaleString()}>
            <time dateTime={entry.detected_at}>Detected {formatDecisionDate(entry.detected_at)}</time>
          </Disclosure>
          {entry.status === 'open' && canConfirm && (
            <Button
              className="ml-auto"
              size="sm"
              variant="secondary"
              disabled={dismiss.isPending}
              onClick={() => dismiss.mutate(entry)}
            >
              {pending ? 'Dismissing…' : 'Dismiss'}
            </Button>
          )}
        </div>
        {failed && <p className="mt-1 text-failure">{errorMessage(dismiss.error, 'Could not dismiss this flag.')}</p>}
      </li>
    )
  }
  return (
    <section aria-label={`Cleanup after replacing ${decision.supersedes}`} className="mt-8">
      <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">
        Cleanup after replacing {decision.supersedes}
      </h3>
      <p className="mt-1 text-xs text-muted">
        Documents flagged for still citing {decision.supersedes}. These flags manage operator attention. They do not
        block delivery, review, or merge.
      </p>
      {entries.length === 0 ? (
        <p className="mt-3 text-sm text-muted">No document was flagged.</p>
      ) : (
        <>
          {open.length > 0 ? (
            <ul className="mt-2 divide-y divide-border" aria-label={`Documents still citing ${decision.supersedes}`}>
              {open.map(row)}
            </ul>
          ) : (
            <p className="mt-3 text-sm text-muted">Every flag is cleared.</p>
          )}
          {cleared.length > 0 && (
            <details className="mt-3 rounded-md border border-border px-3 py-2">
              <summary className="cursor-pointer text-xs font-medium text-muted">{cleared.length} cleared</summary>
              <ul className="mt-1 divide-y divide-border" aria-label={`Cleared flags for ${decision.supersedes}`}>
                {cleared.map(row)}
              </ul>
            </details>
          )}
        </>
      )}
    </section>
  )
}

function SweepDocumentLink({ entry }: { entry: DecisionSupersessionSweepEntry }) {
  const { workspace } = useWorkspaceSelection()
  const className = 'font-medium text-primary hover:underline'
  if (entry.document_tier === 'system_design')
    return (
      <Link
        to="/w/$workspaceId/system-design"
        params={{ workspaceId: workspace }}
        search={{ document: entry.document_id }}
        className={className}
      >
        {entry.document_id}
      </Link>
    )
  if (entry.document_tier === 'requirement')
    return (
      <Link
        to="/w/$workspaceId/requirements"
        params={{ workspaceId: workspace }}
        search={{ requirement: entry.document_id }}
        className={className}
      >
        {entry.document_id}
      </Link>
    )
  return (
    <Link
      to="/w/$workspaceId/requirements"
      params={{ workspaceId: workspace }}
      hash={`reference-${entry.document_id}-v0`}
      className={className}
    >
      {entry.document_id}
    </Link>
  )
}

function sweepTierLabel(tier: DecisionSupersessionSweepEntry['document_tier']) {
  if (tier === 'system_design') return 'System Design'
  if (tier === 'reference_document') return 'Reference document'
  return 'Requirement'
}
