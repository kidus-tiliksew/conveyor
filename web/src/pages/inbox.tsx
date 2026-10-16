import { Link, useSearch } from '@tanstack/react-router'
import { useState } from 'react'
import { AutomaticBand } from '../components/inbox/automatic-band'
import { BlockedRow } from '../components/inbox/blocked-row'
import { DecisionCard } from '../components/inbox/decision-card'
import { InboxBandChips } from '../components/inbox/inbox-band-chips'
import { InboxContextSheet } from '../components/inbox/inbox-context-sheet'
import type { InboxSearch } from '../components/inbox/inbox-search'
import { ProposalCard } from '../components/inbox/proposal-card'
import { useNextActions } from '../components/inbox/use-next-actions'
import { Badge } from '../components/ui/badge'
import { errorMessage } from '../lib/errors'
import type { InboxItem } from '../lib/types'

// The retired pending-proposals queue's narrowing, moved verbatim and applied
// to the decisions band: `task` keeps that task's proposals (and its own
// decision card), `document` with `tier` keeps that document's proposals.
function matchesSearch(item: InboxItem, search: InboxSearch) {
  const proposal = item.proposal
  if (item.kind === 'document_proposal' && proposal) {
    if (proposal.tier !== 'requirement' && proposal.tier !== 'system_design' && proposal.tier !== 'decision')
      return false
    if (search.task && (proposal.origin_type !== 'task' || proposal.origin_id !== search.task)) return false
    if (search.document && (proposal.id !== search.document || proposal.tier !== search.tier)) return false
    return true
  }
  if (search.task && item.task?.id !== search.task) return false
  if (search.document) return false
  return true
}

// The operator inbox (feature-operator-next-action NA-6; REQ-2): decisions,
// blocked work and automatic work, each in the server's order. One read feeds
// every band; the band chips filter it in the browser.
export function InboxPage() {
  const search = useSearch({ from: '/inbox' })
  const inbox = useNextActions()
  const [viewing, setViewing] = useState<{ taskId: string; kind: string } | null>(null)
  const items = inbox.data?.items ?? []
  const allDecisions = items.filter((item) => item.band === 'decisions')
  const decisions = allDecisions.filter((item) => matchesSearch(item, search))
  const blocked = items.filter((item) => item.band === 'blocked')
  const automatic = items.filter((item) => item.band === 'automatic')
  const decisionTaskIds = new Set(allDecisions.flatMap((item) => (item.task ? [item.task.id] : [])))
  const show = (band: InboxSearch['band']) => !search.band || search.band === band
  const counts = { decisions: allDecisions.length, blocked: blocked.length, automatic: automatic.length }

  return (
    <div className="h-full overflow-y-auto">
      {viewing && <InboxContextSheet {...viewing} onClose={() => setViewing(null)} />}
      <div className="mx-auto max-w-5xl px-4 py-6 sm:px-6 sm:py-8">
        <header className="flex flex-wrap items-start justify-between gap-3 border-b border-border pb-5">
          <div className="min-w-0">
            <h1 className="text-xl font-semibold tracking-tight">Inbox</h1>
            <p className="mt-1 text-sm text-muted">
              Only the calls that need your judgment. Below them: tasks held up by other tasks, and what Conveyor
              already handled.
            </p>
          </div>
          <Badge variant={allDecisions.length > 0 ? 'attention' : 'positive'}>
            {allDecisions.length} {allDecisions.length === 1 ? 'decision' : 'decisions'}
          </Badge>
        </header>
        {inbox.data && <InboxBandChips search={search} counts={counts} />}

        {(search.task || search.document) && (
          <div className="mt-4 flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">
            <span className="min-w-0 break-words">
              {search.document && search.tier
                ? `Showing ${search.tier === 'requirement' ? 'Requirement' : 'System Design'} proposals for ${search.document}. Confirming another version dismisses older pending versions.`
                : `Showing decisions from task ${search.task}.`}
            </span>
            <Link to="/inbox" search={{}} className="font-medium text-primary hover:underline">
              Show all
            </Link>
          </div>
        )}
        {inbox.isLoading && <p className="mt-8 text-sm text-muted">Loading the inbox…</p>}
        {inbox.error && (
          <p className="mt-8 rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(inbox.error, 'Could not load the inbox.')}
          </p>
        )}

        {inbox.data && show('decisions') && (
          <section aria-label="Decisions" className="mt-6">
            <h2 className="mb-3 text-sm font-semibold">Decisions</h2>
            {decisions.length === 0 ? (
              <p className="rounded-lg border border-border bg-card p-6 text-sm text-muted">
                No decisions are waiting for you.
              </p>
            ) : (
              <ul className="space-y-3">
                {decisions.map((item) =>
                  item.kind === 'document_proposal' && item.proposal ? (
                    <ProposalCard
                      key={`${item.proposal.tier}:${item.proposal.id}:${item.proposal.version ?? 0}`}
                      proposal={item.proposal}
                      sentence={item.sentence}
                    />
                  ) : item.task ? (
                    <DecisionCard
                      key={item.task.id}
                      item={item}
                      task={item.task}
                      onView={(task) => setViewing({ taskId: task.id, kind: item.next_action?.kind ?? '' })}
                    />
                  ) : null,
                )}
              </ul>
            )}
          </section>
        )}

        {inbox.data && show('blocked') && blocked.length > 0 && (
          <section aria-label="Blocked" className="mt-8">
            <h2 className="mb-3 text-sm font-semibold">Blocked</h2>
            <ul className="divide-y divide-border rounded-lg border border-border bg-card">
              {blocked.map((item) =>
                item.task ? (
                  <BlockedRow key={item.task.id} item={item} task={item.task} decisionTaskIds={decisionTaskIds} />
                ) : null,
              )}
            </ul>
          </section>
        )}

        {inbox.data && show('automatic') && (automatic.length > 0 || inbox.data.acts.length > 0) && (
          <section aria-label="Automatic" className="mt-8">
            <h2 className="mb-3 text-sm font-semibold">Automatic</h2>
            <AutomaticBand items={automatic} acts={inbox.data.acts} />
          </section>
        )}
      </div>
    </div>
  )
}
