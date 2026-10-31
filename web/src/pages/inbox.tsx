import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { Check } from 'lucide-react'
import { useEffect, useRef } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { AutomaticBand } from '../components/inbox/automatic-band'
import { BlockedRow } from '../components/inbox/blocked-row'
import { InboxBandChips } from '../components/inbox/inbox-band-chips'
import { useInboxKeyboard } from '../components/inbox/inbox-keyboard'
import { isUrgent, useMinuteClock } from '../components/inbox/inbox-kind'
import { InboxReviewDetail } from '../components/inbox/inbox-review-detail'
import { InboxReviewList } from '../components/inbox/inbox-review-list'
import type { InboxSearch } from '../components/inbox/inbox-search'
import { inboxItemKey, steppedKey, survivingNeighbour } from '../components/inbox/inbox-selection'
import { useNextActions } from '../components/inbox/use-next-actions'
import { errorMessage } from '../lib/errors'
import type { InboxItem } from '../lib/types'
import { cn } from '../lib/utils'

// The retired pending-proposals queue's narrowing, applied to the decisions
// band: `task` keeps that task's proposals (and its own decision), `document`
// with `tier` keeps that document's proposals. A proposal of a tier this
// client does not know stays listed read-only (NA-6).
function matchesSearch(item: InboxItem, search: InboxSearch) {
  const proposal = item.proposal
  if (item.kind === 'document_proposal' && proposal) {
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
// every band; the band chips filter it in the browser. Decisions are a
// list-and-detail review surface (AC-2.8 to AC-2.11; DEC-82): the list holds
// one compact row per item, and the selected item's question, context, change
// and choices render only in the detail view beside it.
export function InboxPage() {
  const search = useSearch({ from: '/w/$workspaceId/inbox' })
  const navigate = useNavigate({ from: '/w/$workspaceId/inbox' })
  const { workspace } = useWorkspaceSelection()
  const inbox = useNextActions()
  const now = useMinuteClock()
  const root = useRef<HTMLDivElement>(null)
  const items = inbox.data?.items ?? []
  const allDecisions = items.filter((item) => item.band === 'decisions')
  const decisions = allDecisions.filter((item) => matchesSearch(item, search))
  const blocked = items.filter((item) => item.band === 'blocked')
  const automatic = items.filter((item) => item.band === 'automatic')
  const decisionTaskIds = new Set(allDecisions.flatMap((item) => (item.task ? [item.task.id] : [])))
  const show = (band: InboxSearch['band']) => !search.band || search.band === band
  const counts = { decisions: allDecisions.length, blocked: blocked.length, automatic: automatic.length }

  // `selected` names an item in the narrowed list. A missing or stale value
  // selects the first item in the wide layout and shows the list alone below
  // md; the page never writes that fallback into the URL, so Back on a phone
  // stays on the list (AC-2.8).
  const keys = decisions.map(inboxItemKey)
  const explicit = search.selected && keys.includes(search.selected) ? search.selected : undefined
  const selectedKey = explicit ?? keys[0]
  const selectedItem = selectedKey ? decisions[keys.indexOf(selectedKey)] : undefined

  // Acts recorded from this page, by workspace and item. When the selected
  // item leaves after one of them, the selection moves to its surviving
  // neighbour in the earlier order (AC-2.10); an item that leaves without an
  // act from here is a stale selection and falls back as above.
  const acted = useRef(new Set<string>())
  const previous = useRef<{ workspace: string; keys: string[]; selected?: string } | null>(null)
  const keyList = keys.join('\n')
  useEffect(() => {
    if (!inbox.data) return
    const before = previous.current
    previous.current = { workspace, keys, selected: selectedKey }
    if (!before || before.workspace !== workspace || !before.selected || keys.includes(before.selected)) return
    const mark = `${workspace}\n${before.selected}`
    if (!acted.current.delete(mark)) return
    void navigate({
      search: (current) => ({ ...current, selected: survivingNeighbour(before.keys, before.selected ?? '', keys) }),
      replace: true,
      resetScroll: false,
    })
  }, [inbox.data, keyList, workspace, selectedKey, navigate])

  useInboxKeyboard({
    root,
    move: (step) => {
      if (!inbox.data || !show('decisions')) return
      const next = steppedKey(keys, selectedKey, step)
      if (!next || next === explicit) return
      void navigate({ search: (current) => ({ ...current, selected: next }), replace: true, resetScroll: false })
      requestAnimationFrame(() => {
        const row = root.current?.querySelector(`[data-inbox-row="${CSS.escape(next)}"]`)
        if (row instanceof HTMLElement && row.getClientRects().length > 0) row.focus({ preventScroll: false })
      })
    },
  })

  const markActed = (key: string) => () => acted.current.add(`${workspace}\n${key}`)
  const showBlocked = show('blocked') && blocked.length > 0
  const showAutomatic = show('automatic') && (automatic.length > 0 || (inbox.data?.acts.length ?? 0) > 0)

  return (
    <div ref={root} className="h-full overflow-y-auto">
      <div className="mx-auto max-w-6xl px-4 py-6 sm:px-6 sm:py-8">
        <header className="flex flex-wrap items-end justify-between gap-x-6 gap-y-3">
          <div className="min-w-0">
            <h1 className="text-2xl font-semibold tracking-tight">Inbox</h1>
            {inbox.data && (
              <InboxSummary
                decisions={allDecisions.length}
                urgent={allDecisions.filter((item) => isUrgent(item, now)).length}
                blocked={blocked.length}
                automatic={automatic.length}
              />
            )}
          </div>
          {inbox.data && <InboxBandChips search={search} counts={counts} />}
        </header>

        {(search.task || search.document) && (
          <div className="mt-4 flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">
            <span className="min-w-0 break-words">
              {search.document && search.tier
                ? `Showing ${search.tier === 'requirement' ? 'Requirement' : 'System Design'} proposals for ${search.document}. Confirming another version dismisses older pending versions.`
                : `Showing decisions from task ${search.task}.`}
            </span>
            <Link
              to="/w/$workspaceId/inbox"
              params={{ workspaceId: workspace }}
              search={{}}
              className="font-medium text-primary hover:underline"
            >
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
            {decisions.length === 0 ? (
              <EmptyDecisions />
            ) : (
              <div className="grid min-w-0 items-start gap-4 md:grid-cols-[minmax(0,19rem)_minmax(0,1fr)] lg:grid-cols-[minmax(0,21rem)_minmax(0,1fr)]">
                <div className={cn('min-w-0', explicit ? 'hidden md:block' : 'block')}>
                  <InboxReviewList items={decisions} selectedKey={selectedKey} search={search} />
                </div>
                <InboxReviewDetail
                  item={selectedItem}
                  itemKey={selectedKey}
                  workspace={workspace}
                  explicit={explicit != null}
                  search={search}
                  onActed={markActed(selectedKey ?? '')}
                />
              </div>
            )}
          </section>
        )}

        {inbox.data && (showBlocked || showAutomatic) && (
          <div className={cn('mt-10 grid min-w-0 items-start gap-6', showBlocked && showAutomatic && 'lg:grid-cols-2')}>
            {showBlocked && (
              <section aria-label="Blocked" className="min-w-0">
                <h2 className="mb-2 flex items-baseline gap-2 text-sm font-semibold">
                  Waiting on other tasks <span className="text-xs font-normal text-faint">{blocked.length}</span>
                </h2>
                <ul className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-card">
                  {blocked.map((item) =>
                    item.task ? (
                      <BlockedRow key={item.task.id} item={item} task={item.task} decisionTaskIds={decisionTaskIds} />
                    ) : null,
                  )}
                </ul>
              </section>
            )}
            {showAutomatic && (
              <section aria-label="Automatic" className="min-w-0">
                <h2 className="mb-2 flex items-baseline gap-2 text-sm font-semibold">
                  Handled automatically <span className="text-xs font-normal text-faint">{automatic.length}</span>
                </h2>
                <AutomaticBand items={automatic} acts={inbox.data.acts} />
              </section>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

// The header's one-line summary (feature-operator-next-action NA-6). Urgent
// counts the decisions that unblock other tasks or are due within three hours.
function InboxSummary({
  decisions,
  urgent,
  blocked,
  automatic,
}: {
  decisions: number
  urgent: number
  blocked: number
  automatic: number
}) {
  if (decisions === 0)
    return (
      <p data-inbox-summary className="mt-1 text-sm text-muted">
        Nothing needs you right now.
      </p>
    )
  return (
    <p data-inbox-summary className="mt-1 text-sm text-muted">
      <span className="font-medium text-foreground">
        {decisions} {decisions === 1 ? 'call needs' : 'calls need'} you
      </span>
      {urgent > 0 && (
        <>
          {' · '}
          <span data-inbox-urgent className="font-medium text-attention">
            {urgent} urgent
          </span>
        </>
      )}
      {` · ${blocked} waiting on other tasks · ${automatic} handled automatically`}
    </p>
  )
}

function EmptyDecisions() {
  return (
    <div
      data-no-decisions
      className="flex flex-col items-center rounded-xl border border-dashed border-border px-6 py-14 text-center"
    >
      <span className="flex size-10 items-center justify-center rounded-full bg-positive-soft text-positive">
        <Check className="size-5" aria-hidden />
      </span>
      <p className="mt-3 font-medium">You're clear</p>
      <p className="mt-1 text-sm text-muted">No decisions are waiting. Conveyor will put the next one here.</p>
    </div>
  )
}
