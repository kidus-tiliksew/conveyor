import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearch } from '@tanstack/react-router'
import { Check, Clock, FileDiff, X } from 'lucide-react'
import {
  useDashboardSession,
  usePendingProposals,
  useWorkspaceCapability,
  useWorkspaceSelection,
} from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import {
  confirmRequirementVersion,
  confirmSystemDesignVersion,
  fetchRequirement,
  fetchSystemDesign,
  resolveDecision,
  resolveTaskContextProposal,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type { PendingProposal } from '../lib/types'

const tierLabels: Record<PendingProposal['tier'], string> = {
  requirement: 'Requirement',
  system_design: 'System Design',
  decision: 'Decision',
  task_context: 'Suggested context',
}

export function PendingProposalsPage() {
  const token = useDashboardSession()
  const canConfirmDocuments = useWorkspaceCapability('confirm_documents')
  const canOperateGates = useWorkspaceCapability('operate_gates')
  const { workspace } = useWorkspaceSelection()
  const search = useSearch({ from: '/pending-proposals' })
  const client = useQueryClient()
  const proposals = usePendingProposals()
  const resolve = useMutation({
    mutationFn: async ({ proposal, action }: { proposal: PendingProposal; action: 'confirm' | 'dismiss' }) => {
      if (proposal.tier === 'task_context') {
        if (proposal.origin_type !== 'task' || !proposal.origin_id || !proposal.target_kind) {
          throw new Error('This context suggestion is missing its task or document kind.')
        }
        return resolveTaskContextProposal(proposal.origin_id, proposal.target_kind, proposal.id, action)
      }
      if (proposal.tier === 'decision') return resolveDecision(proposal.id, action)
      if (action === 'dismiss') throw new Error('This document tier is resolved by choosing the version to confirm.')
      if (proposal.version == null) throw new Error('The proposal did not include a version.')
      if (proposal.tier === 'requirement') {
        const view = await fetchRequirement(proposal.id)
        return confirmRequirementVersion(proposal.id, proposal.version, view.requirement.current_version ?? 0)
      }
      const view = await fetchSystemDesign(proposal.id)
      return confirmSystemDesignVersion(proposal.id, proposal.version, view.document.current_version ?? 0)
    },
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
        client.invalidateQueries({ queryKey: ['activity', workspace] }),
        client.invalidateQueries({ queryKey: ['task', workspace] }),
        client.invalidateQueries({ queryKey: ['requirements', workspace] }),
        client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
        client.invalidateQueries({ queryKey: ['decisions', workspace] }),
      ])
    },
  })
  const items = (proposals.data?.items ?? []).filter((proposal) => {
    if (search.task && (proposal.origin_type !== 'task' || proposal.origin_id !== search.task)) return false
    if (search.document && (proposal.id !== search.document || proposal.tier !== search.tier)) return false
    return true
  })

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <header className="flex flex-wrap items-start justify-between gap-3 border-b border-border pb-5">
          <div>
            <h1 className="text-xl font-semibold tracking-tight">Pending proposals</h1>
            <p className="mt-1 text-sm text-muted">
              Document updates and suggested context waiting for an operator across this workspace.
            </p>
          </div>
          <Badge variant={items.length > 0 ? 'attention' : 'positive'}>
            {items.length} {items.length === 1 ? 'proposal' : 'proposals'}
          </Badge>
        </header>

        {search.task && (
          <div className="mt-4 flex items-center justify-between rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">
            <span>Showing proposals from task {search.task}.</span>
            <Link to="/pending-proposals" search={{}} className="font-medium text-primary hover:underline">
              Show all
            </Link>
          </div>
        )}
        {search.document && search.tier && (
          <div className="mt-4 flex items-center justify-between rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">
            <span>
              Showing {search.tier === 'requirement' ? 'Requirement' : 'System Design'} proposals for {search.document}.
              Confirming another version dismisses older pending versions.
            </span>
            <Link to="/pending-proposals" search={{}} className="font-medium text-primary hover:underline">
              Show all
            </Link>
          </div>
        )}
        {proposals.isLoading && <p className="mt-8 text-sm text-muted">Loading proposals…</p>}
        {proposals.error && (
          <p className="mt-8 rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(proposals.error, 'Could not load pending proposals.')}
          </p>
        )}
        {!proposals.isLoading && !proposals.error && items.length === 0 && (
          <p className="mt-8 rounded-lg border border-border bg-card p-6 text-sm text-muted">
            No document or context decisions are waiting for you.
          </p>
        )}
        {items.length > 0 && (
          <ul className="mt-5 divide-y divide-border rounded-lg border border-border bg-card">
            {items.map((proposal) => {
              const key = proposalKey(proposal)
              const active = resolve.variables != null && proposalKey(resolve.variables.proposal) === key
              const sameDocumentCount = (proposals.data?.items ?? []).filter(
                (item) => item.tier === proposal.tier && item.id === proposal.id,
              ).length
              const taskContextActionable =
                proposal.tier === 'task_context' &&
                proposal.origin_type === 'task' &&
                Boolean(proposal.origin_id && proposal.target_kind)
              const canConfirm =
                proposal.tier === 'task_context' ? canOperateGates && taskContextActionable : canConfirmDocuments
              const canDismiss =
                proposal.tier === 'task_context'
                  ? canOperateGates && taskContextActionable
                  : canConfirmDocuments && proposal.tier === 'decision'
              return (
                <li key={key} className="grid gap-4 px-4 py-4 md:grid-cols-[minmax(0,1fr)_auto]">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge>{tierLabels[proposal.tier]}</Badge>
                      {proposal.version != null && <Badge variant="mono">v{proposal.version}</Badge>}
                      <span className="inline-flex items-center gap-1 text-xs text-faint">
                        <Clock className="size-3" aria-hidden /> {formatAge(proposal.age_seconds)}
                      </span>
                    </div>
                    <h2 className="mt-2 font-medium leading-6">{proposal.title}</h2>
                    <p className="mt-1 text-xs text-muted">Origin: {originLink(proposal)}</p>
                    {proposal.tier === 'task_context' && proposal.target_kind && (
                      <p className="mt-1 text-xs text-muted">
                        {proposal.target_kind === 'requirement' ? 'Outcome' : 'Guidance'} suggested for this task.
                      </p>
                    )}
                    {proposal.justification && (
                      <p className="mt-2 text-sm leading-5 text-foreground">{proposal.justification}</p>
                    )}
                    {proposal.tier !== 'task_context' && sameDocumentCount > 1 && proposal.version != null && (
                      <p className="mt-1 text-xs text-muted">
                        Confirming a later version also dismisses earlier pending versions.
                      </p>
                    )}
                    {resolve.error && active && (
                      <p className="mt-2 text-xs text-failure">
                        {errorMessage(resolve.error, 'Could not resolve this proposal.')}
                      </p>
                    )}
                  </div>
                  <div className="flex flex-wrap items-center gap-2 self-center">
                    {detailLink(proposal)}
                    {canConfirm && (
                      <Button
                        size="sm"
                        disabled={!token || resolve.isPending}
                        onClick={() => resolve.mutate({ proposal, action: 'confirm' })}
                      >
                        <Check />
                        {active && resolve.isPending && resolve.variables?.action === 'confirm'
                          ? 'Confirming…'
                          : 'Confirm'}
                      </Button>
                    )}
                    {canDismiss && (
                      <Button
                        size="sm"
                        variant="destructive"
                        disabled={!token || resolve.isPending}
                        onClick={() => resolve.mutate({ proposal, action: 'dismiss' })}
                      >
                        <X />
                        {active && resolve.isPending && resolve.variables?.action === 'dismiss'
                          ? 'Dismissing…'
                          : 'Dismiss'}
                      </Button>
                    )}
                  </div>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}

function proposalKey(proposal: PendingProposal) {
  return `${proposal.tier}:${proposal.origin_id ?? ''}:${proposal.id}:${proposal.version ?? 0}`
}

function detailLink(proposal: PendingProposal) {
  const className = 'inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline'
  if (proposal.tier === 'task_context' && proposal.target_kind === 'requirement')
    return (
      <Link to="/requirements" search={{ requirement: proposal.id }} className={className}>
        <FileDiff className="size-3" /> View document
      </Link>
    )
  if (proposal.tier === 'task_context' && proposal.target_kind === 'system_design')
    return (
      <Link to="/system-design" search={{ document: proposal.id }} className={className}>
        <FileDiff className="size-3" /> View document
      </Link>
    )
  if (proposal.tier === 'task_context') return null
  if (proposal.tier === 'requirement')
    return (
      <Link to="/requirements" search={{ requirement: proposal.id }} className={className}>
        <FileDiff className="size-3" /> View details
      </Link>
    )
  if (proposal.tier === 'system_design')
    return (
      <Link to="/system-design" search={{ document: proposal.id }} className={className}>
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
