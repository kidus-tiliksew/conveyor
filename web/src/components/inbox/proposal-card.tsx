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
import { nextActionsQueryKey } from './use-next-actions'

const tierLabels: Record<PendingProposal['tier'], string> = {
  requirement: 'Requirement',
  system_design: 'System Design',
  decision: 'Decision',
}

function knownTier(tier: string): tier is PendingProposal['tier'] {
  return tier === 'requirement' || tier === 'system_design' || tier === 'decision'
}

// A pending document proposal in the Inbox's decisions band
// (feature-operator-next-action NA-6; req-operator-next-action AC-1.8,
// AC-2.6). It reads like a task decision card: the server's label, question
// and if-ignored text, a footer band of choices, and raw identifiers only in
// the IDs disclosure. The acts keep the retired pending-proposals queue's
// rules: Confirm reads the document's current version first and confirms
// against it; Requirement and System Design Dismiss ask why, Decision Dismiss
// sends at once; without confirm_documents, or for a tier this client does not
// know, the card is read-only. `sentence` is the server's multi-version note.
export function ProposalCard({ item, proposal }: { item: InboxItem; proposal: PendingProposal }) {
  const canConfirmDocuments = useWorkspaceCapability('confirm_documents')
  const canProposeDocuments = useWorkspaceCapability('propose_documents')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [dismissing, setDismissing] = useState(false)
  const [reviseTarget, setReviseTarget] = useState<ReviseTarget | null>(null)
  const resolve = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: async ({ action, note }: { action: 'confirm' | 'dismiss'; note?: string }) => {
      if (proposal.tier === 'decision') return resolveDecision(proposal.id, action)
      if (proposal.version == null) throw new Error('The proposal did not include a version.')
      if (proposal.tier === 'requirement') {
        if (action === 'dismiss') return dismissRequirementVersion(proposal.id, proposal.version, note)
        const view = await fetchRequirement(proposal.id)
        return confirmRequirementVersion(proposal.id, proposal.version, view.requirement.current_version ?? 0)
      }
      if (action === 'dismiss') return dismissSystemDesignVersion(proposal.id, proposal.version, note)
      const view = await fetchSystemDesign(proposal.id)
      return confirmSystemDesignVersion(proposal.id, proposal.version, view.document.current_version ?? 0)
    },
    onSuccess: (_, variables) => {
      if (variables.action === 'dismiss') setDismissing(false)
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
    <li
      data-inbox-item="document_proposal"
      className="min-w-0 overflow-hidden rounded-lg border border-attention/40 bg-card"
    >
      {reviseTarget && <VersionReviseDialog target={reviseTarget} onClose={() => setReviseTarget(null)} />}
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
      <div className="min-w-0 space-y-2.5 px-4 pb-3 pt-4">
        <div className="flex flex-wrap items-center gap-2">
          <span data-decision-label className="text-[11px] font-semibold uppercase tracking-[0.08em] text-attention">
            {label}
          </span>
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
        <h3 title={proposal.title} className="line-clamp-2 break-words font-medium leading-6">
          {proposal.title}
        </h3>
        <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
          <span>Origin: {originLink(proposal)}</span>
          {known && contextLink(proposal)}
        </div>
        {item.question && <p className="text-sm leading-6">{item.question}</p>}
        {resolve.error && !dismissing && (
          <p className="text-xs text-failure">{errorMessage(resolve.error, 'Could not resolve this proposal.')}</p>
        )}
      </div>
      <div data-decision-footer className="space-y-2 border-t border-border bg-surface/60 px-4 py-3">
        {canAct && (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={resolve.isPending} onClick={() => resolve.mutate({ action: 'confirm' })}>
              <Check />
              {action === 'confirm' ? 'Confirming…' : 'Confirm'}
            </Button>
            {canRevise && (
              <Button
                size="sm"
                variant="secondary"
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
            <Button
              size="sm"
              variant="ghost"
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
    </li>
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
    dateTime: (validAt ?? since).toISOString(),
    absolute: validAt?.toLocaleString(),
  }
}

function contextLink(proposal: PendingProposal) {
  const className = 'font-medium text-primary hover:underline'
  if (proposal.tier === 'requirement')
    return (
      <Link
        to="/requirements"
        search={{ requirement: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        View changes →
      </Link>
    )
  if (proposal.tier === 'system_design')
    return (
      <Link
        to="/system-design"
        search={{ document: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        View changes →
      </Link>
    )
  return (
    <Link to="/system-design" hash={`decision-${proposal.id.toLowerCase()}`} className={className}>
      View decision →
    </Link>
  )
}

// The origin in plain words; its identifier stays in the IDs disclosure.
function originLink(proposal: PendingProposal) {
  if (proposal.origin_type === 'task' && proposal.origin_id)
    return (
      <Link to="/tasks/$taskId/full" params={{ taskId: proposal.origin_id }} className="text-primary hover:underline">
        Task
      </Link>
    )
  if (proposal.origin_type === 'session' && proposal.origin_id)
    return (
      <a href={`/planning?session=${encodeURIComponent(proposal.origin_id)}`} className="text-primary hover:underline">
        Planning session
      </a>
    )
  if (proposal.origin_type === 'drift') return <>Change signal</>
  return <>Operator</>
}
