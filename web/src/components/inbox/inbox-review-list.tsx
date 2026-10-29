import { Link } from '@tanstack/react-router'
import type { InboxItem } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { pullRequestLabel } from './decision-card'
import { Kbd } from './inbox-keyboard'
import type { InboxSearch } from './inbox-search'
import { inboxItemKey } from './inbox-selection'
import { knownTier, originWord, proposalAge, tierLabels } from './proposal-card'

// The Inbox's decision list (req-operator-next-action AC-2.8; DEC-82). One
// compact row per item, in the server's order (DEC-82(2)): the kind, the title
// clamped to two lines, a short meta line and the age. A row carries no choice
// control (DEC-82(1)); it is a link that names the item in `selected`, so the
// selection survives reload and browser history. The link covers the whole row
// through a stretched overlay; a task's pull request link sits above it, outside
// the row's link, so it opens the pull request without selecting the row
// (feature-operator-next-action NA-6).
export function InboxReviewList({
  items,
  selectedKey,
  search,
}: {
  items: InboxItem[]
  selectedKey: string | undefined
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
      <ul
        aria-label="Decision list"
        className="divide-y divide-border overflow-hidden rounded-lg border border-border bg-card"
      >
        {items.map((item) => {
          const key = inboxItemKey(item)
          return (
            <li key={key}>
              <InboxRow item={item} rowKey={key} selected={key === selectedKey} search={search} />
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
  search,
}: {
  item: InboxItem
  rowKey: string
  selected: boolean
  search: InboxSearch
}) {
  const { workspace } = useWorkspaceSelection()
  const row = describeRow(item)
  const pullRequest = item.kind === 'document_proposal' ? undefined : item.pull_request
  return (
    <div
      data-inbox-row-item={rowKey}
      className={cn(
        'relative min-w-0 border-l-2 px-3 py-2.5 hover:bg-surface',
        selected ? 'border-l-primary bg-primary-soft/40' : 'border-l-transparent',
      )}
    >
      <Link
        to="/w/$workspaceId/inbox"
        params={{ workspaceId: workspace }}
        search={{ ...search, selected: rowKey }}
        resetScroll={false}
        data-inbox-row={rowKey}
        aria-current={selected ? 'true' : undefined}
        className="block min-w-0 outline-none after:absolute after:inset-0 after:content-[''] focus-visible:after:outline-2 focus-visible:after:outline-offset-[-2px] focus-visible:after:outline-primary"
      >
        <span className="flex min-w-0 items-center gap-2 text-[11px]">
          <span data-row-kind className="min-w-0 truncate font-semibold uppercase tracking-[0.06em] text-attention">
            {row.kind}
          </span>
          {row.blocks > 0 && (
            <span data-row-blocks className="shrink-0 rounded bg-attention-soft px-1.5 font-medium text-attention">
              Blocks {row.blocks}
            </span>
          )}
          {row.age && (
            <time dateTime={row.age.dateTime} title={row.age.absolute} className="ml-auto shrink-0 text-faint">
              {row.age.relative}
            </time>
          )}
        </span>
        <span
          data-row-title
          title={row.title}
          className="mt-0.5 line-clamp-2 break-words text-sm font-medium leading-5"
        >
          {row.title}
        </span>
      </Link>
      {pullRequest ? (
        <span className="mt-0.5 block truncate text-xs">
          <a
            data-row-pull-request
            href={pullRequest.url}
            target="_blank"
            rel="noreferrer"
            className="relative z-10 font-mono text-primary hover:underline"
          >
            {row.meta}
          </a>
        </span>
      ) : (
        row.meta && <span className="mt-0.5 block truncate text-xs text-muted">{row.meta}</span>
      )}
    </div>
  )
}

function describeRow(item: InboxItem) {
  const proposal = item.proposal
  if (item.kind === 'document_proposal' && proposal) {
    const known = knownTier(proposal.tier)
    const kind = known ? `${tierLabels[proposal.tier]} proposal` : 'Document proposal'
    const version = proposal.version == null ? '' : `v${proposal.version} · `
    return {
      kind,
      title: proposal.title,
      meta: `${version}${originWord(proposal)}`,
      blocks: 0,
      age: proposalAge(proposal),
    }
  }
  const meta = item.pull_request ? pullRequestLabel(item.pull_request) : 'No PR yet'
  return {
    kind: item.next_action?.label ?? 'Decision',
    title: item.task?.title ?? '',
    meta,
    blocks: item.next_action?.blocks?.length ?? 0,
    age: item.last_event_at
      ? { relative: relativeTime(item.last_event_at), dateTime: item.last_event_at, absolute: undefined }
      : null,
  }
}
