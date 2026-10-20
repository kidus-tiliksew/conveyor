import { useQuery } from '@tanstack/react-query'
import { fetchDecision, fetchRequirement, fetchSystemDesign } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { Decision, PendingProposal, RequirementView, SystemDesignView } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { DocumentComparison } from '../documents/version-diff'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'

// What a pending document proposal changes, shown in the Inbox detail view
// (req-operator-next-action AC-2.9). A requirement or System Design version is
// compared with the document's current confirmed version through the document
// surfaces' own comparison; with no confirmed version the comparison starts
// from an empty base, so the proposed content shows whole. A decision proposal
// shows its statement, context and rejected alternatives in full.
//
// Each read uses the document pages' workspace-scoped key with the query's
// abort signal and reads again on every mount. Content shows only from a read
// that succeeded since this view mounted, so cached content is never shown for
// a pending or failed read, and the detail view is keyed by workspace and item,
// so a late read never shows under another selection.
export function ProposalContext({ proposal }: { proposal: PendingProposal }) {
  if (proposal.tier === 'decision') return <DecisionText id={proposal.id} />
  if (proposal.version == null) return null
  return <VersionChanges proposal={proposal} version={proposal.version} />
}

function VersionChanges({ proposal, version }: { proposal: PendingProposal; version: number }) {
  const { workspace } = useWorkspaceSelection()
  const requirement = proposal.tier === 'requirement'
  const { data, error, refetch, isFetching, isFetchedAfterMount } = useQuery<RequirementView | SystemDesignView>({
    queryKey: [requirement ? 'requirement' : 'system-design', workspace, proposal.id],
    queryFn: ({ signal }) =>
      requirement
        ? fetchRequirement(proposal.id, workspace, signal)
        : fetchSystemDesign(proposal.id, workspace, signal),
    enabled: Boolean(workspace),
    refetchOnMount: 'always',
  })
  const view = data && error == null && isFetchedAfterMount ? data : undefined
  const target = (view?.pending_versions ?? []).find((candidate) => candidate.version === version)
  const current = view?.current_version
  return (
    <section aria-label="Proposed changes" data-proposal-changes aria-busy={!view && error == null}>
      <h3 className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">Changes</h3>
      {!view && error == null && <Skeleton className="mt-2 h-24" />}
      {error != null && <ReadError error={error} retrying={isFetching} retry={() => void refetch()} />}
      {view && !target && (
        <p className="mt-2 text-sm text-muted">Version {version} is no longer pending on this document.</p>
      )}
      {view && target && (
        <>
          <p className="mt-1 text-xs text-muted" data-comparison-base>
            {current
              ? `Confirmed v${current.version} compared with proposed v${target.version}.`
              : `No version is confirmed yet: proposed v${target.version} is shown against an empty base.`}
          </p>
          <DocumentComparison
            key={`${current?.version ?? 0}:${target.version}`}
            before={current ?? { content: '' }}
            after={target}
          />
        </>
      )}
    </section>
  )
}

function DecisionText({ id }: { id: string }) {
  const { workspace } = useWorkspaceSelection()
  const { data, error, refetch, isFetching, isFetchedAfterMount } = useQuery<Decision>({
    queryKey: ['decision', workspace, id],
    queryFn: ({ signal }) => fetchDecision(id, workspace, signal),
    enabled: Boolean(workspace),
    refetchOnMount: 'always',
  })
  const decision = data && error == null && isFetchedAfterMount ? data : undefined
  return (
    <section aria-label="Proposed decision" data-proposal-decision aria-busy={!decision && error == null}>
      {!decision && error == null && <Skeleton className="h-24" />}
      {error != null && <ReadError error={error} retrying={isFetching} retry={() => void refetch()} />}
      {decision && (
        <dl className="space-y-3 text-sm">
          {(
            [
              ['Statement', decision.statement],
              ['Context', decision.context],
              ['Alternatives rejected', decision.alternatives_rejected],
            ] as const
          ).map(([label, text]) => (
            <div key={label}>
              <dt className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">{label}</dt>
              <dd className="mt-1 whitespace-pre-wrap break-words leading-6">{text || '—'}</dd>
            </div>
          ))}
          {decision.supersedes && (
            <p className="text-xs text-muted">
              Supersedes <span className="font-mono">{decision.supersedes}</span>.
            </p>
          )}
        </dl>
      )}
    </section>
  )
}

function ReadError({ error, retrying, retry }: { error: unknown; retrying: boolean; retry: () => void }) {
  return (
    <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
      <span className="text-failure">{errorMessage(error, 'Could not load this proposal.')}</span>
      <Button size="sm" variant="secondary" disabled={retrying} onClick={retry}>
        Retry
      </Button>
    </div>
  )
}
