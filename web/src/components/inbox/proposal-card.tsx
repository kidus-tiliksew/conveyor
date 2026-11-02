import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, X } from 'lucide-react'
import { useState } from 'react'
import {
  confirmRequirementVersion,
  confirmSystemDesignVersion,
  dismissRequirementVersion,
  dismissSystemDesignVersion,
  fetchRequirement,
  fetchSystemDesign,
  resolveDecision,
} from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { MutationWorkspace } from '../../lib/query-keys'
import type { InboxItem, PendingProposal } from '../../lib/types'
import { relativeTime } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { VersionDismissDialog } from '../documents/version-dismiss-dialog'
import { type ReviseTarget, VersionReviseDialog } from '../documents/version-revise-dialog'
import { NextActionIDs } from '../task/next-action-parts'
import { Button } from '../ui/button'
import { Kbd } from './inbox-keyboard'
import { KindPill } from './inbox-kind'
import { ProposalContext } from './proposal-context'
import { nextActionsQueryKey } from './use-next-actions'

export const tierLabels: Record<PendingProposal['tier'], string> = {
  requirement: 'Requirement',
  system_design: 'System Design',
  decision: 'Decision',
}

export function knownTier(tier: string): tier is PendingProposal['tier'] {
  return tier === 'requirement' || tier === 'system_design' || tier === 'decision'
}

// The selected pending document proposal in the Inbox's detail view
// (feature-operator-next-action NA-6; req-operator-next-action AC-1.8,
// AC-2.6, AC-2.8, AC-2.9). It reads like a task decision: the server's label,
// question and if-ignored text, the proposed change in place, a footer band of
// choices with their keys, and raw identifiers only in the IDs disclosure.
// `onActed` runs after a confirm or dismiss succeeds and when the Revise
// dialog closes; the page moves the selection only if the item then leaves
// (AC-2.10). The acts keep the retired pending-proposals queue's
// rules: Confirm reads the document's current version first and confirms
// against it; Requirement and System Design Dismiss ask why, Decision Dismiss
// sends at once; without confirm_documents, or for a tier this client does not
// know, the card is read-only. `sentence` is the server's multi-version note.
export function ProposalDetail({
  item,
  proposal,
  onActed,
}: {
  item: InboxItem
  proposal: PendingProposal
  onActed: () => void
}) {
  const canConfirmDocuments = useWorkspaceCapability('confirm_documents')
  const canProposeDocuments = useWorkspaceCapability('propose_documents')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [dismissing, setDismissing] = useState(false)
  const [reviseTarget, setReviseTarget] = useState<ReviseTarget | null>(null)
  const resolve = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: async ({ action, note }: { action: 'confirm' | 'dismiss'; note?: string }) => {
      if (proposal.tier === 'decision') return resolveDecision(workspace, proposal.id, action)
      if (proposal.version == null) throw new Error('The proposal did not include a version.')
      if (proposal.tier === 'requirement') {
        if (action === 'dismiss') return dismissRequirementVersion(workspace, proposal.id, proposal.version, note)
        const view = await fetchRequirement(proposal.id, workspace)
        return confirmRequirementVersion(
          workspace,
          proposal.id,
          proposal.version,
          view.requirement.current_version ?? 0,
        )
      }
      if (action === 'dismiss') return dismissSystemDesignVersion(workspace, proposal.id, proposal.version, note)
      const view = await fetchSystemDesign(proposal.id, workspace)
      return confirmSystemDesignVersion(workspace, proposal.id, proposal.version, view.document.current_version ?? 0)
    },
    onSuccess: (_, variables) => {
      if (variables.action === 'dismiss') setDismissing(false)
      onActed()
    },
    onSettled: async (_data, _error, _variables, context) => {
      const acted = context?.workspace ?? workspace
      await Promise.all([
        client.invalidateQueries({ queryKey: nextActionsQueryKey(acted) }),
        client.invalidateQueries({ queryKey: ['pending-proposals', acted] }),
        client.invalidateQueries({ queryKey: ['activity', acted] }),
        client.invalidateQueries({ queryKey: ['task', acted] }),
        client.invalidateQueries({ queryKey: ['requirements', acted] }),
        client.invalidateQueries({ queryKey: ['system-designs', acted] }),
        client.invalidateQueries({ queryKey: ['decisions', acted] }),
      ])
    },
  })
  const action = resolve.isPending ? resolve.variables?.action : undefined
  const known = knownTier(proposal.tier)
  const canAct = canConfirmDocuments && known
  const canRevise = canAct && canProposeDocuments && proposal.tier !== 'decision' && proposal.version != null
  const age = proposalAge(proposal)
  const label = item.label || (known ? `${tierLabels[proposal.tier]} proposal` : 'Document proposal')

  return (
    <article
      data-inbox-item="document_proposal"
      aria-label={proposal.title}
      className="flex min-h-0 min-w-0 flex-1 flex-col rounded-xl border border-border bg-card md:overflow-hidden"
    >
      {reviseTarget && (
        <VersionReviseDialog
          target={reviseTarget}
          onClose={() => {
            setReviseTarget(null)
            onActed()
          }}
        />
      )}
      {dismissing && proposal.tier !== 'decision' && proposal.version != null && (
        <VersionDismissDialog
          documentTitle={proposal.title}
          version={proposal.version}
          pending={resolve.isPending}
          error={resolve.error ? errorMessage(resolve.error, 'Could not dismiss this version.') : undefined}
          onCancel={() => setDismissing(false)}
          onConfirm={(note) => resolve.mutate({ action: 'dismiss', note })}
        />
      )}
      <div className="min-h-0 min-w-0 flex-1 space-y-3 overflow-y-auto px-5 pb-5 pt-5">
        <div className="flex flex-wrap items-center gap-2">
          <KindPill kind="document_proposal" label={label} />
          {proposal.version != null && (
            <span data-proposal-version className="font-mono text-[11px] text-muted">
              v{proposal.version}
            </span>
          )}
          {age && (
            <time data-proposal-age dateTime={age.dateTime} title={age.absolute} className="ml-auto text-xs text-faint">
              {age.relative}
            </time>
          )}
        </div>
        <h2 data-detail-title className="break-words text-base font-semibold leading-6">
          {proposal.title}
        </h2>
        <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
          <span>Origin: {originLink(proposal, workspace)}</span>
          {known && contextLink(proposal, workspace)}
        </div>
        {item.question && <p className="text-sm leading-6">{item.question}</p>}
        {known && (
          <div className="border-t border-border pt-3">
            <ProposalContext proposal={proposal} />
          </div>
        )}
      </div>
      <div
        data-decision-footer
        className="z-10 shrink-0 space-y-2 rounded-b-xl border-t border-border bg-surface px-5 py-3 max-md:sticky max-md:bottom-0"
      >
        {resolve.error && !dismissing && (
          <p className="text-xs text-failure">{errorMessage(resolve.error, 'Could not resolve this proposal.')}</p>
        )}
        {canAct && (
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              data-shortcut="c"
              disabled={resolve.isPending}
              onClick={() => resolve.mutate({ action: 'confirm' })}
            >
              <Check />
              {action === 'confirm' ? 'Confirming…' : 'Confirm'}
            </Button>
            <Kbd>c</Kbd>
            {canRevise && (
              <Button
                size="sm"
                variant="secondary"
                data-shortcut="r"
                disabled={resolve.isPending}
                onClick={() =>
                  setReviseTarget({
                    id: proposal.id,
                    title: proposal.title,
                    tier: proposal.tier as ReviseTarget['tier'],
                    version: proposal.version as number,
                  })
                }
              >
                Revise
              </Button>
            )}
            {canRevise && <Kbd>r</Kbd>}
            <Button
              size="sm"
              variant="ghost"
              data-shortcut="d"
              disabled={resolve.isPending}
              onClick={() => {
                resolve.reset()
                if (proposal.tier === 'decision') resolve.mutate({ action: 'dismiss' })
                else setDismissing(true)
              }}
            >
              <X />
              {action === 'dismiss' ? 'Dismissing…' : 'Dismiss'}
            </Button>
            <Kbd>d</Kbd>
          </div>
        )}
        <div className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-1 text-xs text-muted">
          {item.if_ignored && (
            <p className="min-w-0 break-words">
              If you do nothing: <span className="font-medium text-foreground">{item.if_ignored}</span>
            </p>
          )}
          {item.sentence && <p className="min-w-0 break-words">{item.sentence}</p>}
          <div className="ml-auto">
            <NextActionIDs
              value={{
                document: proposal.id,
                tier: proposal.tier,
                ...(proposal.version == null ? {} : { version: proposal.version }),
                origin: proposal.origin_type,
                ...(proposal.origin_id ? { origin_id: proposal.origin_id } : {}),
              }}
            />
          </div>
        </div>
      </div>
    </article>
  )
}

// The proposal's age: the server's age_seconds when it is a finite
// non-negative number, otherwise the relative time of a valid proposed_at,
// and nothing when neither is usable, so the card never prints NaN.
export function proposalAge(proposal: Pick<PendingProposal, 'proposed_at' | 'age_seconds'>) {
  const at = proposal.proposed_at ? new Date(proposal.proposed_at) : undefined
  const validAt = at && Number.isFinite(at.getTime()) && at.getTime() > 0 ? at : undefined
  const seconds = proposal.age_seconds
  const since =
    typeof seconds === 'number' && Number.isFinite(seconds) && seconds >= 0
      ? new Date(Date.now() - seconds * 1000)
      : validAt
  if (!since) return null
  return {
    relative: relativeTime(since.toISOString()),
    since: since.toISOString(),
    dateTime: (validAt ?? since).toISOString(),
    absolute: validAt?.toLocaleString(),
  }
}

function contextLink(proposal: PendingProposal, workspace: string) {
  const className = 'font-medium text-primary hover:underline'
  if (proposal.tier === 'requirement')
    return (
      <Link
        to="/w/$workspaceId/requirements"
        params={{ workspaceId: workspace }}
        search={{ requirement: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        View changes →
      </Link>
    )
  if (proposal.tier === 'system_design')
    return (
      <Link
        to="/w/$workspaceId/system-design"
        params={{ workspaceId: workspace }}
        search={{ document: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        View changes →
      </Link>
    )
  return (
    <Link
      to="/w/$workspaceId/decisions/$decisionId"
      params={{ workspaceId: workspace, decisionId: proposal.id }}
      className={className}
    >
      View decision →
    </Link>
  )
}

// The origin in plain words for a list row, without its link.
export function originWord(proposal: Pick<PendingProposal, 'origin_type' | 'origin_id'>) {
  if (proposal.origin_type === 'task' && proposal.origin_id) return 'Task'
  if (proposal.origin_type === 'session' && proposal.origin_id) return 'Planning session'
  if (proposal.origin_type === 'drift') return 'Change signal'
  return 'Operator'
}

// The origin in plain words; its identifier stays in the IDs disclosure.
function originLink(proposal: PendingProposal, workspace: string) {
  if (proposal.origin_type === 'task' && proposal.origin_id)
    return (
      <Link
        to="/w/$workspaceId/tasks/$taskId/full"
        params={{ workspaceId: workspace, taskId: proposal.origin_id }}
        className="text-primary hover:underline"
      >
        Task
      </Link>
    )
  if (proposal.origin_type === 'session' && proposal.origin_id)
    return (
      <Link
        to="/w/$workspaceId/planning"
        params={{ workspaceId: workspace }}
        search={{ session: proposal.origin_id }}
        className="text-primary hover:underline"
      >
        Planning session
      </Link>
    )
  if (proposal.origin_type === 'drift') return <>Change signal</>
  return <>Operator</>
}
