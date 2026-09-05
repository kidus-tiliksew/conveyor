import { useInfiniteQuery } from '@tanstack/react-query'
import { fetchDocumentEvents } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { TaskEvent } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

export function useDocumentEvents(
  kind: 'requirement' | 'system_design',
  id: string,
  item: {
    lineage: TaskEvent[]
    lineage_total: number
    lineage_snapshot_id: number
  },
) {
  const { workspace } = useWorkspaceSelection()
  const total = item.lineage_total ?? item.lineage.length
  const snapshot = item.lineage_snapshot_id ?? 0
  const query = useInfiniteQuery({
    queryKey: ['document-events', workspace, kind, id, snapshot, total],
    initialPageParam: 0,
    initialData: {
      pages: [{ events: item.lineage, total, limit: 50, offset: 0, snapshot_id: snapshot }],
      pageParams: [0],
    },
    queryFn: ({ pageParam }) => fetchDocumentEvents(kind, id, pageParam, snapshot),
    getNextPageParam: (page) =>
      page.offset + page.events.length < page.total ? page.offset + page.events.length : undefined,
    staleTime: Number.POSITIVE_INFINITY,
  })
  return { ...query, events: query.data.pages.flatMap((page) => page.events), total }
}

export function MoreDocumentEvents({ history }: { history: ReturnType<typeof useDocumentEvents> }) {
  return (
    <>
      {history.hasNextPage && (
        <Button
          variant="ghost"
          size="sm"
          disabled={history.isFetchingNextPage}
          onClick={() => void history.fetchNextPage()}
        >
          {history.isFetchingNextPage ? 'Loading activity…' : 'Load older activity'}
        </Button>
      )}
      {history.error && (
        <p className="text-xs text-failure">{errorMessage(history.error, 'Could not load older activity.')}</p>
      )}
    </>
  )
}
