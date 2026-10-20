import type { InboxItem } from '../../lib/types'

// The stable identity of one inbox item, used for row keys, the `selected`
// search parameter and deduplicating the paged read (AC-2.8). A task item is
// its task ID. A decision proposal is `decision:<DEC-n>`. Any other proposal,
// including one of a tier this client does not know, is its literal
// `<tier>:<document-id>:<version>`, or `<tier>:<document-id>` when it carries
// no version.
export function inboxItemKey(item: InboxItem): string {
  const proposal = item.proposal
  if (proposal) {
    if (proposal.tier === 'decision') return `decision:${proposal.id}`
    return proposal.version == null
      ? `${proposal.tier}:${proposal.id}`
      : `${proposal.tier}:${proposal.id}:${proposal.version}`
  }
  return item.task?.id ?? ''
}

// The item to select after `resolved` left the list (AC-2.10): the first item
// after it in the earlier order that is still listed, otherwise the nearest
// one before it that is still listed. Several items can leave at once (a
// confirmation that dismisses older versions, a poll that lands other
// operators' acts), so neighbours are taken from the earlier order and kept
// only when they survive.
export function survivingNeighbour(before: readonly string[], resolved: string, now: readonly string[]) {
  const index = before.indexOf(resolved)
  if (index < 0) return undefined
  const present = new Set(now)
  for (const key of before.slice(index + 1)) if (present.has(key)) return key
  for (const key of before.slice(0, index).reverse()) if (present.has(key)) return key
  return undefined
}

// The next or previous key in the list for keyboard movement, bounded at the
// ends. With nothing selected, either direction starts at the first item.
export function steppedKey(keys: readonly string[], current: string | undefined, step: 1 | -1) {
  if (keys.length === 0) return undefined
  const index = current == null ? -1 : keys.indexOf(current)
  if (index < 0) return keys[0]
  return keys[Math.min(Math.max(index + step, 0), keys.length - 1)]
}
