import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { Check } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { AutomaticBand } from '../components/inbox/automatic-band'
import { BlockedRow } from '../components/inbox/blocked-row'
import { InboxBandChips } from '../components/inbox/inbox-band-chips'
import { useInboxKeyboard } from '../components/inbox/inbox-keyboard'
import { isUrgent, useMinuteClock } from '../components/inbox/inbox-kind'
import { InboxReviewDetail, ReportActWatcher } from '../components/inbox/inbox-review-detail'
import { InboxReviewList } from '../components/inbox/inbox-review-list'
import type { InboxSearch } from '../components/inbox/inbox-search'
import { inboxItemKey, steppedKey, survivingNeighbour } from '../components/inbox/inbox-selection'
import { useNextActions } from '../components/inbox/use-next-actions'
import { ErrorState } from '../components/ui/error-state'
import { PageHeader } from '../components/ui/page-header'
import { Skeleton } from '../components/ui/skeleton'
import type { InboxItem } from '../lib/types'
import { cn } from '../lib/utils'

// The retired pending-proposals queue's narrowing, applied to the decisions
// band: `task` keeps that task's proposals (and its own decision), `document`
// with `tier` keeps that document's proposals. A proposal of a tier this
// client does not know stays listed read-only (component-next-action, State and
// failure). A report from production belongs to no task, so the task
// narrowing keeps it, as the server's task filter does; the document
// narrowing lists only proposals (req-operator-next-action AC-2.12).
function matchesSearch(item: InboxItem, search: InboxSearch) {
  if (item.kind === 'feedback_report') return !search.document
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

// The operator inbox (component-next-action invariants 21, 22; REQ-2): decisions,
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
  // Urgency is read once, over the decisions the list shows, so the header's
  // count and the rows' urgent marks name the same items.
  const urgentKeys = new Set(decisions.filter((item) => isUrgent(item, now)).map(inboxItemKey))
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
  // The selection after an act on a report (AC-2.10; component-next-action
  // invariant 37) follows the rule stated at `ReportHold`: the mark of the
  // task and proposal path below, plus the order and the card kept from the
  // click, because a report can leave the list before its act answers.
  const [holds, setHolds] = useState<Record<string, ReportHold>>({})
  const view = searchView(search)
  const shownHold = Object.values(holds).find(
    (hold) => hold.workspace === workspace && hold.address === search.selected && hold.view === view,
  )
  const explicit = search.selected && (keys.includes(search.selected) || shownHold) ? search.selected : undefined
  const selectedKey = shownHold ? shownHold.key : (explicit ?? keys[0])
  const selectedItem = shownHold
    ? (decisions[keys.indexOf(shownHold.key)] ?? shownHold.item)
    : selectedKey
      ? decisions[keys.indexOf(selectedKey)]
      : undefined

  // Applies the rule to each hold whenever the list, the address, the band,
  // the filter, or the workspace changes.
  const keyList = keys.join('\n')
  useEffect(() => {
    const entries = Object.entries(holds)
    if (entries.length === 0) return
    const next = { ...holds }
    let changed = false
    let choose: { selected: string | undefined } | undefined
    for (const [id, hold] of entries) {
      // A selection the person made, or a change of band, filter, or
      // workspace, ends the rule; the page's own behaviour applies.
      if (hold.workspace !== workspace || hold.view !== view || hold.address !== search.selected) {
        delete next[id]
        changed = true
        continue
      }
      // Until a 200, and while the list still lists the report, nothing moves.
      if (!hold.recorded || !inbox.data || keys.includes(hold.key)) continue
      // The list no longer lists the report: the nearest item after its place
      // in the order at the click that the list still lists, else the nearest
      // before it; none at all is the empty state.
      delete next[id]
      changed = true
      choose = { selected: survivingNeighbour(hold.order, hold.key, keys) }
    }
    if (changed) setHolds(next)
    if (choose) {
      const selected = choose.selected
      void navigate({ search: (current) => ({ ...current, selected }), replace: true, resetScroll: false })
    }
  }, [holds, inbox.data, keyList, workspace, view, search.selected, navigate])

  // Acts recorded from this page, by workspace and item. When the selected
  // item leaves after one of them, the selection moves to its surviving
  // neighbour in the earlier order (AC-2.10); an item that leaves without an
  // act from here is a stale selection and falls back as above.
  const acted = useRef(new Set<string>())
  const previous = useRef<{ workspace: string; keys: string[]; selected?: string } | null>(null)
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
  // What the page shows now, for the report acts that start and settle
  // between renders.
  const shown = useRef({ workspace, keys, selectedKey, selectedItem, address: search.selected, view })
  shown.current = { workspace, keys, selectedKey, selectedItem, address: search.selected, view }
  const holdKey = (id: string) => `${workspace}\n${id}`
  // An act starts on the report the detail shows, which is the selected
  // one; the hold keeps the order the list showed, the card, and the address.
  const reportActStarted = (id: string) => {
    const now = shown.current
    if (now.workspace !== workspace || now.selectedKey !== id || !now.selectedItem) return
    const hold: ReportHold = {
      workspace,
      key: id,
      item: now.selectedItem,
      order: now.keys,
      address: now.address,
      view: now.view,
      recorded: false,
    }
    setHolds((current) => ({ ...current, [holdKey(id)]: hold }))
  }
  // A 200 marks the hold as recorded; the choice waits for a list that no
  // longer lists the report. A refusal, a retryable or unknown answer, or a
  // failed request ends the rule, and an item that left falls back as any
  // stale selection does.
  const reportActSettled = (id: string, recorded: boolean) => {
    setHolds((current) => {
      const hold = current[holdKey(id)]
      if (!hold) return current
      const next = { ...current }
      if (recorded) next[holdKey(id)] = { ...hold, recorded: true }
      else delete next[holdKey(id)]
      return next
    })
  }
  const heldIds = Object.values(holds).flatMap((hold) => (hold.workspace === workspace ? [hold.key] : []))
  const reportIds = [
    ...new Set([
      ...allDecisions.flatMap((item) => (item.kind === 'feedback_report' && item.report ? [item.report.id] : [])),
      ...heldIds,
    ]),
  ]
  const showBlocked = show('blocked') && blocked.length > 0
  const showAutomatic = show('automatic') && (automatic.length > 0 || (inbox.data?.acts.length ?? 0) > 0)

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* On a phone the bands take their own line, flush with the page's left edge. */}
      <PageHeader
        title="Inbox"
        className="max-sm:[&>div:last-child]:ml-0 max-sm:[&>div:last-child]:w-full"
        description={
          inbox.data ? (
            <InboxSummary decisions={allDecisions.length} urgent={urgentKeys.size} />
          ) : (
            inbox.isLoading && (
              <span
                aria-hidden
                data-inbox-summary-skeleton
                className="inline-block h-3.5 w-20 animate-pulse rounded bg-raised/60 align-middle"
              />
            )
          )
        }
      >
        {(inbox.data || inbox.isLoading) && <InboxBandChips search={search} counts={inbox.data && counts} />}
      </PageHeader>
      {reportIds.map((id) => (
        <ReportActWatcher
          key={`${workspace}:${id}`}
          workspace={workspace}
          reportId={id}
          onStarted={() => reportActStarted(id)}
          onSettled={(recorded) => reportActSettled(id, recorded)}
        />
      ))}
      <div ref={root} className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-6xl px-4 py-5 sm:px-6">
          {(search.task || search.document) && (
            <div className="mb-4 flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">
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
          {inbox.isLoading && show('decisions') && <InboxSkeleton />}
          {inbox.error && (
            <ErrorState
              title="Could not load the inbox"
              error={inbox.error}
              onRetry={() => void inbox.refetch()}
              retrying={inbox.isFetching}
            />
          )}

          {inbox.data && show('decisions') && (
            <section aria-label="Decisions">
              {decisions.length === 0 && !shownHold ? (
                <EmptyDecisions />
              ) : (
                <div className="grid min-w-0 items-start gap-4 md:grid-cols-[minmax(0,19rem)_minmax(0,1fr)] lg:grid-cols-[minmax(0,21rem)_minmax(0,1fr)]">
                  <div className={cn('min-w-0', explicit ? 'hidden md:block' : 'block')}>
                    <InboxReviewList
                      items={decisions}
                      selectedKey={selectedKey}
                      urgentKeys={urgentKeys}
                      search={search}
                    />
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
            <div
              className={cn(
                'grid min-w-0 items-start gap-6',
                show('decisions') && 'mt-10',
                showBlocked && showAutomatic && 'lg:grid-cols-2',
              )}
            >
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
                    Automatic work <span className="text-xs font-normal text-faint">{automatic.length}</span>
                  </h2>
                  <AutomaticBand items={automatic} acts={inbox.data.acts} />
                </section>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

// The header's one-line summary (component-next-action). The band
// chips beside it carry the counts, so it adds only what they do not: urgent
// counts the listed decisions that unblock other tasks or are due within three
// hours, and each one it counts carries an urgent mark in its row.
function InboxSummary({ decisions, urgent }: { decisions: number; urgent: number }) {
  if (decisions === 0)
    return (
      <span data-inbox-summary className="text-sm text-faint">
        Nothing needs you right now.
      </span>
    )
  return (
    <span data-inbox-summary className="text-sm text-faint">
      {urgent > 0 ? (
        <span data-inbox-urgent className="font-medium text-attention">
          {urgent} urgent
        </span>
      ) : (
        'Nothing urgent'
      )}
    </span>
  )
}

// The first load's placeholder, in the decisions band's own grid: the list's
// heading line and compact rows on the left and the detail card on the right,
// with the classes of the content that replaces it, so nothing moves when the
// read answers.
function InboxSkeleton() {
  return (
    <div data-inbox-skeleton aria-busy="true">
      <span className="sr-only">Loading the inbox…</span>
      <div className="grid min-w-0 items-start gap-4 md:grid-cols-[minmax(0,19rem)_minmax(0,1fr)] lg:grid-cols-[minmax(0,21rem)_minmax(0,1fr)]">
        <div className="min-w-0" data-inbox-skeleton-list>
          <div className="mb-2 flex h-5 items-center gap-2">
            <Skeleton className="h-3.5 w-16" />
          </div>
          <ul className="space-y-0.5">
            {[0, 1, 2, 3].map((row) => (
              <li key={row} data-inbox-skeleton-row className="rounded-lg border border-transparent px-3 py-2">
                <Skeleton className="h-4 w-32" />
                <Skeleton className="mt-0.5 h-5 w-full" />
              </li>
            ))}
          </ul>
        </div>
        <div className="hidden min-w-0 md:block" data-inbox-skeleton-detail>
          <Skeleton className="h-72 rounded-xl" />
        </div>
      </div>
    </div>
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

// The rule for the selection after an act on a report (AC-2.10;
// component-next-action invariant 37). It is the task and proposal rule (a
// mark set by a local act that stays until the item leaves, then the
// surviving neighbour in the earlier order), with two additions: the order
// and the card are kept from the click, because the report can leave the
// list before its act answers, and the choice is made from a list in which
// the report is gone.
// 1. When the person sends an act on the selected report, the page keeps the
//    order of the list as it showed it then and the report's place in it,
//    and keeps the report's card in view.
// 2. Nothing is chosen when the act answers. After a 200, the first list the
//    page shows that no longer lists the report decides: it is the answer of
//    an existing refresh (the read the 200 starts, a workspace stream frame,
//    or window focus), or the list already shown at the 200 when a refresh
//    dropped the report before the answer. Until then the report stays
//    selected and in view, through any number of reads that still list it.
// 3. From that list: the nearest item after the report's place in the order
//    at the click that the list still lists; if none, the nearest item before
//    it; if the list holds no decision, the empty state.
// 4. A selection the person makes before that list stays, and ends the rule.
// 5. A change of band, filter, or workspace before that list ends the rule;
//    the page's behaviour for that change applies.
// 6. A read that fails changes no list: the page shows the failure as it
//    does for any read, and the next list decides.
// 7. A refused, retryable, unknown (500), or failed act, and an item that
//    leaves through someone else's act, end or never start the rule: the
//    stale-selection fallback applies.
interface ReportHold {
  workspace: string
  key: string
  item: InboxItem
  order: string[]
  address: string | undefined
  view: string
  // Set by a 200.
  recorded: boolean
}

// The band and filters of the address: a change of them ends the rule.
function searchView(search: InboxSearch) {
  return JSON.stringify([search.band ?? '', search.task ?? '', search.document ?? '', search.tier ?? ''])
}
