import { Link } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import type { InboxItem } from '../../lib/types'
import { cn } from '../../lib/utils'
import { DecisionDetail } from './decision-card'
import type { InboxSearch } from './inbox-search'
import { ProposalDetail } from './proposal-card'

// The detail view beside the decision list (req-operator-next-action AC-2.8).
// From md up it is always shown and pinned while the page scrolls, with its
// body scrolling inside and its choices kept in view. Below md it replaces
// the list only while `selected` names an item, with a Back link that clears
// `selected` and keeps every other filter. The item's view is keyed by
// workspace and item, so its reads, composer and mutations start fresh on
// every selection and a late result never shows under another one.
export function InboxReviewDetail({
  item,
  itemKey,
  workspace,
  explicit,
  search,
  onActed,
}: {
  item: InboxItem | undefined
  itemKey: string | undefined
  workspace: string
  explicit: boolean
  search: InboxSearch
  onActed: () => void
}) {
  return (
    <div
      data-inbox-detail={itemKey}
      className={cn(
        'min-w-0 flex-col md:sticky md:top-4 md:flex md:max-h-[calc(100dvh-12rem)]',
        explicit ? 'flex' : 'hidden',
      )}
    >
      <Link
        to="/inbox"
        search={{ ...search, selected: undefined }}
        resetScroll={false}
        data-inbox-back
        className="mb-2 inline-flex items-center gap-1 self-start text-xs font-medium text-primary hover:underline md:hidden"
      >
        <ArrowLeft className="size-3.5" />
        Back to decisions
      </Link>
      {item?.kind === 'document_proposal' && item.proposal ? (
        <ProposalDetail key={`${workspace}:${itemKey}`} item={item} proposal={item.proposal} onActed={onActed} />
      ) : item?.task ? (
        <DecisionDetail key={`${workspace}:${itemKey}`} item={item} task={item.task} onActed={onActed} />
      ) : null}
    </div>
  )
}
