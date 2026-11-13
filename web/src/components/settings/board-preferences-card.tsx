import { Columns3 } from 'lucide-react'
import { setShowStageProgress, useShowStageProgress } from '../../lib/board-preferences'
import { Switch } from '../ui/switch'
import { BrowserRow } from './browser-row'

// The Board's per-browser display choice (component-web-dashboard, Board
// preferences). Stage progress is off by default: a card's column already
// says which stage it is in (req-operator-next-action AC-3.3).
export function BoardPreferencesCard() {
  const showStageProgress = useShowStageProgress()
  return (
    <BrowserRow
      icon={Columns3}
      title="Show stage progress on cards"
      description="Draw each task's path through its stages and approvals on its Board card."
      control={
        <Switch aria-label="Show stage progress on cards" checked={showStageProgress} onChange={setShowStageProgress} />
      }
    />
  )
}
