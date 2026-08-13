import { useQueries } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { pendingUserRequestChanges, userRunImplementation } from '../../lib/activity'
import { fetchTaskActivity } from '../../lib/api'
import { isBlueprintAnchor } from '../../lib/blueprint'
import type { ActivityItem } from '../../lib/types'
import { relativeTime } from '../../lib/utils'
import { useActivity, useWorkspaceSelection } from '../app-shell'
import { type AttentionItem, AttentionSurface } from '../documents/attention-surface'
import { Button } from '../ui/button'

// Work that came back to the signed-in person, rendered in the attention
// surface every other outstanding signal already uses rather than in a tray of
// its own (REQ-6; the sole-rendering discipline). This changes nothing about
// attention, staleness, bounce, hold, or assignment derivation — it is the
// presentation of a marker the server already keeps.
//
// The marker itself reaches the dashboard only folded into `needs_attention`,
// so the feed narrows the candidates — my assignments that are asking for a
// human — and the task's own detail, which carries the durable events, is what
// decides and supplies the feedback. That keeps the reading exact instead of
// inferred from state, and the fan-out is bounded by how much of my own work is
// waiting on me. Both queries are ones the app already holds: the unfiltered
// feed behind the navigation badge, and the same task-detail cache entry the
// task panel reads.

// Enough of the feedback to recognize which return this is, without turning an
// attention entry into the task. The whole text is on the task itself.
const EXCERPT_LIMIT = 220

function excerpt(feedback: string): string {
  return feedback.length > EXCERPT_LIMIT ? `${feedback.slice(0, EXCERPT_LIMIT).trimEnd()}…` : feedback
}

// The latest implement claimant is the durable discriminator the server uses
// when bouncing work. A task-run claim needs the person to resume it; a worker
// claim returns to the factory even when an operator separately holds the task.
function nextStep(item: ActivityItem): string {
  return userRunImplementation(item.events)
    ? `Nothing is running this — pick the feedback up by running the task again from your machine (conveyor run ${item.task.id}).`
    : 'The factory is already on it — a fresh implementation run is carrying this feedback.'
}

export function ReturnedForChangesAttention({ me }: { me: string }) {
  const { workspace } = useWorkspaceSelection()
  const { data: feed } = useActivity()
  const candidates = (feed ?? [])
    .filter((summary) => summary.task.assignee?.user_id === me)
    .filter((summary) => summary.needs_attention && !isBlueprintAnchor(summary.task))
    .filter((summary) => summary.task.state !== 'merged' && summary.task.state !== 'closed')
    .map((summary) => summary.task.id)

  const details = useQueries({
    queries: candidates.map((taskId) => ({
      queryKey: ['task', workspace, taskId],
      queryFn: () => fetchTaskActivity(taskId),
      enabled: Boolean(workspace),
    })),
  })

  const items: AttentionItem[] = []
  for (const detail of details) {
    const item = detail.data as ActivityItem | undefined
    if (!item) continue
    const returned = pendingUserRequestChanges(item.events)
    if (!returned) continue
    items.push({
      id: `returned-${item.task.id}`,
      title: `${item.task.title || item.task.id} came back to you with feedback`,
      detail: (
        <>
          {returned.feedback && <p className="text-foreground/85">“{excerpt(returned.feedback)}”</p>}
          <p className={returned.feedback ? 'mt-1' : undefined}>
            {nextStep(item)}{' '}
            <span className="text-faint" title={new Date(returned.at).toLocaleString()}>
              Sent {relativeTime(returned.at)}.
            </span>
          </p>
        </>
      ),
      action: (
        <Link to="/tasks" search={{ task: item.task.id }}>
          <Button size="sm" variant="secondary" tabIndex={-1}>
            Open task
          </Button>
        </Link>
      ),
    })
  }

  // Nothing outstanding is the ordinary case on this surface, and the list
  // below already stands for the workspace; an empty all-clear panel above it
  // would be a second thing to read that says nothing.
  if (items.length === 0) return null
  return (
    <div className="mt-6">
      <AttentionSurface items={items} />
    </div>
  )
}
