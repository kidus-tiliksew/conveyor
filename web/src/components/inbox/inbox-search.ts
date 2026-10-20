import type { InboxBand } from '../../lib/types'

// The Inbox's address: the band the page shows, the task, document and tier
// the decisions band narrows to on arrival (the retired pending-proposals
// page's parameters, so its links keep working), and the selected item's key
// (inboxItemKey; req-operator-next-action AC-2.8).
export interface InboxSearch {
  band?: InboxBand
  task?: string
  document?: string
  tier?: 'requirement' | 'system_design'
  selected?: string
}

const bands: ReadonlySet<string> = new Set<InboxBand>(['decisions', 'blocked', 'automatic'])
const text = (value: unknown) => (typeof value === 'string' && value ? value : undefined)

export function validateInboxSearch(search: Record<string, unknown>): InboxSearch {
  return {
    band: typeof search.band === 'string' && bands.has(search.band) ? (search.band as InboxBand) : undefined,
    task: text(search.task),
    document: text(search.document),
    tier: search.tier === 'requirement' || search.tier === 'system_design' ? search.tier : undefined,
    selected: text(search.selected),
  }
}
