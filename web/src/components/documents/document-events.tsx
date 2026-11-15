import { useInfiniteQuery } from '@tanstack/react-query'
import { fetchDocumentEvents } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { DocumentEventPage } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

type DocumentEventPageParam = { offset: number; snapshot?: number }

// Document events load only while the History view shows, from the events
// route. The first page sends no snapshot. Every later page passes the first
// page's snapshot_id, so the pages stay one stable list
// (component-web-document-surfaces invariant 8).
export function useDocumentEvents(kind: 'requirement' | 'system_design', id: string, enabled: boolean) {
  const { workspace } = useWorkspaceSelection()
  const query = useInfiniteQuery({
    queryKey: ['document-events', workspace, kind, id],
    initialPageParam: { offset: 0 } as DocumentEventPageParam,
    queryFn: ({ pageParam, signal }) =>
      fetchDocumentEvents(workspace, kind, id, pageParam.offset, pageParam.snapshot, signal),
    getNextPageParam: (page: DocumentEventPage, pages: DocumentEventPage[]): DocumentEventPageParam | undefined =>
      page.offset + page.events.length < page.total
        ? { offset: page.offset + page.events.length, snapshot: pages[0]?.snapshot_id ?? page.snapshot_id }
        : undefined,
    enabled: enabled && Boolean(workspace),
  })
  const pages = query.data?.pages ?? []
  return {
    ...query,
    events: pages.flatMap((page) => page.events),
    // The total is unknown until the first page arrives.
    total: pages[0]?.total,
    loaded: pages.length > 0,
    // An error with no page is a failed first read. An error after a page
    // arrived belongs to an older page, and MoreDocumentEvents states it.
    firstReadError: pages.length === 0 ? query.error : null,
  }
}

export type DocumentEventsState = ReturnType<typeof useDocumentEvents>

// The activity statement while the first page loads or after it fails. It
// renders nothing once the first page has arrived.
export function DocumentEventsStatus({ history }: { history: DocumentEventsState }) {
  if (history.loaded) return null
  if (history.firstReadError)
    return (
      <p role="alert" className="text-sm text-failure">
        Could not load activity. {errorMessage(history.firstReadError, '')}
      </p>
    )
  return (
    <p role="status" className="text-sm text-muted">
      Loading activity…
    </p>
  )
}

export function MoreDocumentEvents({ history }: { history: DocumentEventsState }) {
  if (!history.loaded) return null
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
