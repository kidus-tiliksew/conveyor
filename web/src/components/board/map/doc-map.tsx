// The requirements with open work. Each row is a title, a thin delivery bar,
// and a dot when something needs a person; selecting a row swaps the canvas
// beside it.
import { cn } from '../../../lib/utils'
import { type DocIndex, mapClusters, UNATTACHED } from './docs'
import { DONE, type GraphStatus, type GraphTask, statusMeta, type TaskGraphModel } from './graph-model'

const GATES = new Set<GraphStatus>(['plan_gate', 'merge_gate', 'needs_you'])
// Delivered first, then what is moving, then what needs a person, then what
// closed undelivered: every treatment statusOf yields, so the bar fills.
const barOrder: GraphStatus[] = [
  'merged',
  'running',
  'ready',
  'parked',
  'plan_gate',
  'merge_gate',
  'needs_you',
  'blocked',
  'closed',
]

export interface DocCard {
  id: string
  title: string
  tasks: GraphTask[]
}

// One card per cluster the map draws: a requirement is a card only when it
// is the home of an open task, plus one for open tasks that serve none
// (req-board-map AC-1.2).
export function docCards(index: DocIndex, model: TaskGraphModel, visible: Set<string>): DocCard[] {
  const cards: DocCard[] = []
  for (const cluster of mapClusters(visible, model, index).clusters) {
    const tasks = cluster.taskIds
      .map((taskId) => model.tasks.get(taskId))
      .filter((task): task is GraphTask => Boolean(task))
    cards.push({
      id: cluster.id,
      title: cluster.id === UNATTACHED ? `No ${index.kind}` : (index.docs.get(cluster.id)?.title ?? cluster.id),
      tasks,
    })
  }
  return cards.sort(byUrgency)
}

// Most blocked first, then most waiting on a decision, then most work
// (AC-1.3): each count outranks every later one, however large.
export function byUrgency(a: { tasks: GraphTask[] }, b: { tasks: GraphTask[] }) {
  const blocked = (card: { tasks: GraphTask[] }) => card.tasks.filter((task) => task.status === 'blocked').length
  const gates = (card: { tasks: GraphTask[] }) => card.tasks.filter((task) => GATES.has(task.status)).length
  return blocked(b) - blocked(a) || gates(b) - gates(a) || b.tasks.length - a.tasks.length
}

export function DocList({
  cards,
  selected,
  onSelect,
}: {
  cards: DocCard[]
  selected?: string
  onSelect: (id: string) => void
}) {
  return (
    <nav aria-label="Documents" className="min-h-0 flex-1 overflow-y-auto py-2">
      {cards.map((card) => {
        const total = card.tasks.length
        const counts = new Map<GraphStatus, number>()
        for (const task of card.tasks) counts.set(task.status, (counts.get(task.status) ?? 0) + 1)
        const open = card.tasks.filter((task) => !DONE.has(task.status)).length
        const blocked = counts.get('blocked') ?? 0
        const gates = card.tasks.filter((task) => GATES.has(task.status)).length
        const active = card.id === selected
        return (
          <button
            key={card.id}
            type="button"
            aria-current={active ? 'true' : undefined}
            onClick={() => onSelect(card.id)}
            className={cn(
              'group flex w-full flex-col gap-1.5 border-l-2 px-4 py-2.5 text-left',
              active ? 'border-primary bg-raised' : 'border-transparent hover:bg-surface',
            )}
          >
            <span className="flex items-center gap-2">
              <span
                className={cn(
                  'min-w-0 flex-1 truncate text-[13px]',
                  active ? 'font-semibold text-foreground' : 'font-medium text-muted group-hover:text-foreground',
                )}
              >
                {card.title}
              </span>
              {(blocked > 0 || gates > 0) && (
                <span
                  className={cn('size-1.5 shrink-0 rounded-full', blocked ? 'bg-failure' : 'bg-attention-dot')}
                  title={[blocked && `${blocked} blocked`, gates && `${gates} waiting on a decision`]
                    .filter(Boolean)
                    .join(', ')}
                />
              )}
              <span className="shrink-0 font-mono text-[11px] text-faint">{open}</span>
            </span>
            <span className="flex h-[3px] gap-px overflow-hidden rounded-full bg-raised">
              {barOrder
                .filter((status) => counts.get(status))
                .map((status) => (
                  <span
                    key={status}
                    className={statusMeta[status].dot}
                    style={{ width: `${((counts.get(status) ?? 0) / total) * 100}%` }}
                  />
                ))}
            </span>
          </button>
        )
      })}
    </nav>
  )
}
