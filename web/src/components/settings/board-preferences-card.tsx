import { Columns3 } from 'lucide-react'
import { setShowStageProgress, useShowStageProgress } from '../../lib/board-preferences'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Switch } from '../ui/switch'

// The Board's per-browser display choices (component-web-dashboard, Board
// preferences). Stage progress is off by default: a card's column already
// says which stage it is in (req-operator-next-action AC-3.3).
export function BoardPreferencesCard() {
  const showStageProgress = useShowStageProgress()
  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Board</CardTitle>
        <Columns3 className="size-4 text-primary" />
      </CardHeader>
      <CardContent>
        <div className="flex items-start justify-between gap-4 text-sm">
          <div className="min-w-0 space-y-1">
            <p className="font-medium text-foreground">Show stage progress on cards</p>
            <p className="leading-6 text-muted">
              Draw each task's path through its stages and approvals on its Board card. Saved in this browser.
            </p>
          </div>
          <Switch
            aria-label="Show stage progress on cards"
            checked={showStageProgress}
            onChange={setShowStageProgress}
          />
        </div>
      </CardContent>
    </Card>
  )
}
