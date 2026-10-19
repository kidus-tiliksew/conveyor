import { assigneeName } from '../../lib/activity'
import type { TaskAssignee } from '../../lib/types'
import { Disclosure } from '../ui/disclosure'

// Two letters is enough to recognise a colleague across a dense list and short
// enough to hold one width in every row. A single-word name gives its first two
// rather than leaving the disc half empty.
function initials(name: string): string {
  const words = name.split(/[\s@._-]+/).filter(Boolean)
  if (words.length === 0) return '?'
  return (words.length === 1 ? words[0].slice(0, 2) : words[0][0] + words[1][0]).toUpperCase()
}

/**
 * Assignment rendered as presence: an assigned task carries its person and an
 * unassigned one carries nothing at all — an empty "Unassigned" label would
 * spend a row's width saying that nothing is true (REQ-4).
 *
 * One chip serves the Tasks rows and the Board cards so assignment cannot read
 * as two different things on two surfaces. The name is what the surface shows;
 * the account identifiers behind it belong in the tooltip.
 */
export function AssigneeChip({
  assignee,
  className = '',
  compact = false,
  tone = 'primary',
}: {
  assignee?: TaskAssignee
  className?: string
  // A Board card's single row holds the initials alone; the name stays in the
  // chip's details and its accessible name.
  compact?: boolean
  // The Board list keeps every row neutral apart from its operator and
  // progress marks, so its initials sit on the raised neutral instead of the
  // primary tint the cards use (component-web-task-surfaces, Board and Tasks
  // list).
  tone?: 'primary' | 'neutral'
}) {
  if (!assignee) return null
  const name = assigneeName(assignee)
  const detail = [assignee.email, assignee.user_id].filter((value) => value && value !== name).join(' · ')
  return (
    <Disclosure
      className={className}
      triggerClassName="gap-1.5"
      content={detail ? `Assigned to ${name} — ${detail}` : `Assigned to ${name}`}
      title={detail ? `Assigned to ${name} — ${detail}` : `Assigned to ${name}`}
    >
      <span
        aria-hidden="true"
        className={`flex size-4 shrink-0 items-center justify-center rounded-full text-[9px] font-medium leading-none ${
          tone === 'neutral' ? 'bg-raised text-muted ring-1 ring-border' : 'bg-primary/15 text-primary'
        }`}
      >
        {initials(name)}
      </span>
      <span className={compact ? 'sr-only' : 'truncate'}>{name}</span>
    </Disclosure>
  )
}
