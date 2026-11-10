interface SearchableDocument {
  id: string
  title: string
}

/**
 * Tree search matches a document entry's title or ID, ignoring letter case
 * (component-web-document-surfaces, "The tree"). The query is trimmed, so an
 * empty or whitespace-only query matches every entry.
 */
export function documentMatchesQuery(document: SearchableDocument, query: string) {
  const needle = query.trim().toLocaleLowerCase()
  if (!needle) return true
  return document.title.toLocaleLowerCase().includes(needle) || document.id.toLocaleLowerCase().includes(needle)
}
