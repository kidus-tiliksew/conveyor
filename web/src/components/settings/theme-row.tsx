import { SunMoon } from 'lucide-react'
import type { ThemeChoice } from '../../lib/theme'
import { useTheme } from '../theme-provider'
import { SegmentedControl } from '../ui/segmented-control'
import { BrowserRow } from './browser-row'

const choices: { value: ThemeChoice; label: string }[] = [
  { value: 'system', label: 'System' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
]

// The theme choice on Settings. It uses the same theme store as the sidebar
// control, so a change in either one shows in the other at once
// (component-web-dashboard invariant 19).
export function ThemeRow() {
  const { choice, setChoice } = useTheme()
  return (
    <BrowserRow
      icon={SunMoon}
      title="Theme"
      description="System follows the setting of your device."
      control={<SegmentedControl label="Theme" options={choices} value={choice} onChange={setChoice} />}
    />
  )
}
