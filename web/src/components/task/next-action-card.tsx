import { useNextAction } from '../inbox/use-next-actions'
import { Badge } from '../ui/badge'
import { Card, CardContent } from '../ui/card'
import { Skeleton } from '../ui/skeleton'
import { GateChoices } from './merge-at-gate'
import { NextActionDeadline, NextActionIDs } from './next-action-parts'

// The server's next action, leading task detail on the Board sheet and the full
// task route, directly above the timeline (feature-operator-next-action NA-6;
// req-operator-next-action AC-7.1). Every line is server text; the card
// derives nothing. While the human gate is open on the same page it carries
// the choices beside the evidence, so the card does not repeat them
// (`gateOpen`).
export function NextActionCard({ taskId, gateOpen }: { taskId: string; gateOpen: boolean }) {
  const { data: action, isLoading } = useNextAction(taskId)
  if (isLoading) return <Skeleton className="h-20" />
  if (!action || action.kind === 'terminal') return null
  const operator = action.actor === 'operator'
  return (
    <section aria-label="Next action">
      <Card className={operator ? 'border-attention/40' : undefined}>
        <CardContent className="space-y-2.5 py-3">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={operator ? 'attention' : action.kind === 'merging' ? 'positive' : 'mono'}>
              {action.label}
            </Badge>
            <Badge variant="outline" data-actor={action.actor}>
              {action.actor}
            </Badge>
          </div>
          <p className="text-sm font-medium leading-6">{action.summary}</p>
          {action.question && <p className="text-sm text-muted">{action.question}</p>}
          {!gateOpen && <GateChoices taskId={taskId} action={action} />}
          {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
          <NextActionDeadline deadline={action.deadline} />
          <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />
        </CardContent>
      </Card>
    </section>
  )
}
