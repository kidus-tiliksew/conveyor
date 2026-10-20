import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useEffect, useRef } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { AutomaticBand } from '../components/inbox/automatic-band'
import { BlockedRow } from '../components/inbox/blocked-row'
import { InboxBandChips } from '../components/inbox/inbox-band-chips'
import { useInboxKeyboard } from '../components/inbox/inbox-keyboard'
import { InboxReviewDetail } from '../components/inbox/inbox-review-detail'
import { InboxReviewList } from '../components/inbox/inbox-review-list'
import type { InboxSearch } from '../components/inbox/inbox-search'
import { inboxItemKey, steppedKey, survivingNeighbour } from '../components/inbox/inbox-selection'
import { useNextActions } from '../components/inbox/use-next-actions'
import { Badge } from '../components/ui/badge'
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
  const search = useSearch({ from: '/inbox' })
  const navigate = useNavigate({ from: '/inbox' })
  const { workspace } = useWorkspaceSelection()
  const inbox = useNextActions()
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

  return (
    <div ref={root} className="h-full overflow-y-auto">
      <div className="mx-auto max-w-6xl px-4 py-6 sm:px-6 sm:py-8">
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
            {decisions.length === 0 ? (
              <>
                <h2 className="mb-3 text-sm font-semibold">Decisions</h2>
                <p data-no-decisions className="rounded-lg border border-border bg-card p-6 text-sm text-muted">
                  No decisions are waiting for you.
                </p>
              </>
            ) : (
              <div className="grid min-w-0 items-start gap-4 md:grid-cols-[minmax(0,18rem)_minmax(0,1fr)] lg:grid-cols-[minmax(0,20rem)_minmax(0,1fr)]">
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
