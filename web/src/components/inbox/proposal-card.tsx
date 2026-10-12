import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, Clock, FileDiff, X } from 'lucide-react'
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
import type { PendingProposal } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { VersionDismissDialog } from '../documents/version-dismiss-dialog'
import { type ReviseTarget, VersionReviseDialog } from '../documents/version-revise-dialog'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { nextActionsQueryKey } from './use-next-actions'

const tierLabels: Record<PendingProposal['tier'], string> = {
  requirement: 'Requirement',
  system_design: 'System Design',
  decision: 'Decision',
}

// A pending document proposal in the Inbox's decisions band (AC-2.6). These
// are the retired pending-proposals queue's rules, unchanged: Confirm reads
// the document's current version first and confirms against it; Requirement
// and System Design Dismiss ask why, Decision Dismiss sends at once; without
// confirm_documents the card is read-only. `sentence` is the server's own
// multi-version note.
export function ProposalCard({ proposal, sentence }: { proposal: PendingProposal; sentence?: string }) {
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

  return (
    <li
      data-inbox-item="document_proposal"
      className="grid gap-3 rounded-lg border border-border bg-card px-4 py-4 md:grid-cols-[minmax(0,1fr)_auto]"
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
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <Badge>{tierLabels[proposal.tier]}</Badge>
          {proposal.version != null && <Badge variant="mono">v{proposal.version}</Badge>}
          <span className="inline-flex items-center gap-1 text-xs text-faint">
            <Clock className="size-3" aria-hidden /> {formatAge(proposal.age_seconds)}
          </span>
        </div>
        <h3 className="mt-2 break-words font-medium leading-6">{proposal.title}</h3>
        <p className="mt-1 text-xs text-muted">Origin: {originLink(proposal)}</p>
        {sentence && <p className="mt-1 text-xs text-muted">{sentence}</p>}
        {resolve.error && !dismissing && (
          <p className="mt-2 text-xs text-failure">{errorMessage(resolve.error, 'Could not resolve this proposal.')}</p>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2 self-center">
        {detailLink(proposal)}
        {canConfirmDocuments && (
          <Button size="sm" disabled={resolve.isPending} onClick={() => resolve.mutate({ action: 'confirm' })}>
            <Check />
            {action === 'confirm' ? 'Confirming…' : 'Confirm'}
          </Button>
        )}
        {canConfirmDocuments && canProposeDocuments && proposal.tier !== 'decision' && proposal.version != null && (
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
        {canConfirmDocuments && (
          <Button
            size="sm"
            variant="destructive"
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
        )}
      </div>
    </li>
  )
}

function detailLink(proposal: PendingProposal) {
  const className = 'inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline'
  if (proposal.tier === 'requirement')
    return (
      <Link
        to="/requirements"
        search={{ requirement: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        <FileDiff className="size-3" /> View details
      </Link>
    )
  if (proposal.tier === 'system_design')
    return (
      <Link
        to="/system-design"
        search={{ document: proposal.id, tab: 'changes', target: proposal.version }}
        className={className}
      >
        <FileDiff className="size-3" /> View details
      </Link>
    )
  return (
    <Link to="/system-design" hash={`decision-${proposal.id.toLowerCase()}`} className={className}>
      <FileDiff className="size-3" /> View details
    </Link>
  )
}

function originLink(proposal: PendingProposal) {
  if (proposal.origin_type === 'task' && proposal.origin_id)
    return (
      <Link to="/tasks/$taskId/full" params={{ taskId: proposal.origin_id }} className="text-primary hover:underline">
        task {proposal.origin_id}
      </Link>
    )
  if (proposal.origin_type === 'session' && proposal.origin_id)
    return (
      <a href={`/planning?session=${encodeURIComponent(proposal.origin_id)}`} className="text-primary hover:underline">
        planning session {proposal.origin_id}
      </a>
    )
  if (proposal.origin_type === 'drift' && proposal.origin_id) return <>change signal {proposal.origin_id}</>
  return <>operator</>
}

function formatAge(seconds: number) {
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m old`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h old`
  return `${Math.floor(seconds / 86_400)}d old`
}
