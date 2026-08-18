export type DocumentSort = 'name' | 'created' | 'updated'
export type DocumentSortDirection = 'ascending' | 'descending'

interface SortableDocument {
  id: string
  title: string
  created_at: string
  updated_at: string
}

function timestamp(value: string) {
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed
}

export function compareDocumentText(left: string, right: string) {
  return left.localeCompare(right, 'en', { sensitivity: 'base' })
}

export function compareDocumentTimestamp(left: string, right: string) {
  const leftTime = timestamp(left)
  const rightTime = timestamp(right)
  return leftTime === rightTime ? 0 : leftTime < rightTime ? -1 : 1
}

export function compareDocuments(
  left: SortableDocument,
  right: SortableDocument,
  sort: DocumentSort,
  direction: DocumentSortDirection,
) {
  const comparison =
    sort === 'name'
      ? compareDocumentText(left.title, right.title)
      : compareDocumentTimestamp(
          left[sort === 'created' ? 'created_at' : 'updated_at'],
          right[sort === 'created' ? 'created_at' : 'updated_at'],
        )
  const multiplier = direction === 'ascending' ? 1 : -1
  return comparison === 0 ? compareDocumentText(left.id, right.id) : comparison * multiplier
}
