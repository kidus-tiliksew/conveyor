import { Link } from '@tanstack/react-router'
import { Flame } from 'lucide-react'
import type { InboxItem } from '../../lib/types'
import { cn, fullTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { pullRequestLabel } from './decision-card'
import { Kbd } from './inbox-keyboard'
import { compactAge, DueChip, inboxKind, itemKind, KindIcon, toneBar, UnblocksMark, useMinuteClock } from './inbox-kind'
import type { InboxSearch } from './inbox-search'
import { inboxItemKey } from './inbox-selection'
import { knownTier, proposalAge, tierLabels } from './proposal-card'

// The Inbox's decision list (req-operator-next-action AC-2.8; DEC-82). One
// compact row per item, in the server's order (DEC-82(2)): the kind's icon in
// its tone and the kind, a proposal's version, the age, the title clamped to two
// lines, and a meta line of urgency marks and the pull request only when there
// is one to show (NA-6). A row the header counts as urgent carries one urgent
// mark, so the count can be traced to its rows. A row carries no choice
// control (DEC-82(1)); it is a link that names the item in `selected`, so the
// selection survives reload and browser history. The link covers the whole row
// through a stretched overlay; a task's pull request link sits above it, outside
// the row's link, so it opens the pull request without selecting the row
// (feature-operator-next-action NA-6).
export function InboxReviewList({
  items,
  selectedKey,
  urgentKeys,
  search,
}: {
  items: InboxItem[]
  selectedKey: string | undefined
  urgentKeys: ReadonlySet<string>
  search: InboxSearch
}) {
  return (
    <div className="min-w-0">
      <div className="mb-2 flex items-center gap-2">
        <h2 className="text-sm font-semibold">Decisions</h2>
        <span className="text-xs text-muted">{items.length}</span>
        <span className="ml-auto hidden items-center gap-1 text-[11px] text-faint md:flex" data-move-hint>
          <Kbd>j</Kbd>
          <Kbd>k</Kbd>
          move
        </span>
      </div>
      <ul aria-label="Decision list" className="space-y-0.5">
        {items.map((item) => {
          const key = inboxItemKey(item)
          return (
            <li key={key}>
              <InboxRow
                item={item}
                rowKey={key}
                selected={key === selectedKey}
                urgent={urgentKeys.has(key)}
                search={search}
              />
            </li>
          )
        })}
      </ul>
    </div>
  )
}

function InboxRow({
  item,
  rowKey,
  selected,
  urgent,
  search,
}: {
  item: InboxItem
  rowKey: string
  selected: boolean
  urgent: boolean
  search: InboxSearch
}) {
  const { workspace } = useWorkspaceSelection()
  const now = useMinuteClock()
  const row = describeRow(item, now)
  const kind = itemKind(item)
  const { tone } = inboxKind(kind)
  const pullRequest = item.kind === 'document_proposal' ? undefined : item.pull_request
  return (
    <div
      data-inbox-row-item={rowKey}
      className={cn(
        'relative min-w-0 overflow-hidden rounded-lg border px-3 py-2 transition-colors',
        selected ? 'border-edge bg-card shadow-sm' : 'border-transparent hover:bg-card/60',
      )}
    >
      <span
        aria-hidden
        data-row-accent={selected ? tone : undefined}
        className={cn('absolute inset-y-2 left-0 w-[3px] rounded-r', selected ? toneBar[tone] : 'bg-transparent')}
      />
      <Link
        to="/w/$workspaceId/inbox"
        params={{ workspaceId: workspace }}
        search={{ ...search, selected: rowKey }}
        resetScroll={false}
        data-inbox-row={rowKey}
        aria-current={selected ? 'true' : undefined}
        className="block min-w-0 outline-none after:absolute after:inset-0 after:rounded-lg after:content-[''] focus-visible:after:outline-2 focus-visible:after:outline-offset-[-2px] focus-visible:after:outline-primary"
      >
        <span className="flex min-w-0 items-center gap-1.5 text-xs">
          <KindIcon kind={kind} />
          <span data-row-kind className="min-w-0 truncate font-medium text-muted">
            {row.kind}
          </span>
          {row.version && <span className="shrink-0 font-mono text-faint">{row.version}</span>}
          {row.age && (
            <time
              data-row-age
              dateTime={row.age.dateTime}
              title={row.age.absolute}
              className="ml-auto shrink-0 text-[11px] tabular-nums text-faint"
            >
              {row.age.compact}
            </time>
          )}
        </span>
        <span
          data-row-title
          title={row.title}
          className={cn(
            'mt-0.5 line-clamp-2 break-words text-sm leading-5',
            selected ? 'font-semibold' : 'font-medium',
          )}
        >
          {row.title}
        </span>
      </Link>
      {(urgent || row.blocks > 0 || row.deadline || pullRequest) && (
        <span className="mt-1 flex min-w-0 flex-wrap items-center gap-1.5 text-xs">
          {urgent && <UrgentMark />}
          <UnblocksMark count={row.blocks} />
          <DueChip deadline={row.deadline} />
          {pullRequest && (
            <a
              data-row-pull-request
              href={pullRequest.url}
              target="_blank"
              rel="noreferrer"
              title={pullRequestLabel(pullRequest)}
              className="relative z-10 truncate font-mono text-primary hover:underline"
            >
              {pullRequest.number != null ? `PR #${pullRequest.number}` : 'PR'}
            </a>
          )}
        </span>
      )}
    </div>
  )
}

// The one mark that traces a row to the header's urgent count: the decision
// unblocks other tasks or is due within three hours.
function UrgentMark() {
  return (
    <span
      role="note"
      aria-label="Urgent"
      title="Unblocks other tasks or is due within three hours"
      data-row-urgent
      className="inline-flex shrink-0 items-center gap-1 rounded bg-attention-soft px-1.5 font-medium text-attention"
    >
      <Flame aria-hidden className="size-3" />
      Urgent
    </span>
  )
}

function describeRow(item: InboxItem, now: number) {
  const proposal = item.proposal
  if (item.kind === 'document_proposal' && proposal) {
    const known = knownTier(proposal.tier)
    const kind = known ? `${tierLabels[proposal.tier]} proposal` : 'Document proposal'
    const age = proposalAge(proposal)
    return {
      kind,
      title: proposal.title,
      version: proposal.version == null ? '' : `v${proposal.version}`,
      blocks: 0,
      deadline: item.deadline,
      age: age ? { compact: compactAge(age.since, now), dateTime: age.dateTime, absolute: age.absolute } : null,
    }
  }
  const compact = compactAge(item.last_event_at, now)
  return {
    kind: item.next_action?.label ?? 'Decision',
    title: item.task?.title ?? '',
    version: '',
    blocks: item.next_action?.blocks?.length ?? 0,
    deadline: item.deadline ?? item.next_action?.deadline,
    age:
      item.last_event_at && compact
        ? { compact, dateTime: item.last_event_at, absolute: fullTime(item.last_event_at) }
        : null,
  }
}
