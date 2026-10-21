// The requirement insight panel (req-requirement-insights REQ-1;
// feature-requirement-insights RI-7). It renders in the Board map's side slot
// for the requirement the address's scope names, with the tab its insight
// parameter names. Only the shown tab is mounted, so only its read runs: a
// closed panel or an inactive tab issues no insight read (AC-1.5).
import { BookText, ChartLine, Layers, type LucideIcon, Sparkles, X } from 'lucide-react'
import { type KeyboardEvent, useEffect, useId, useRef } from 'react'
import type { RequirementInsightTab } from '../../lib/types'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import { ChartsTab } from './charts-tab'
import { DocumentsTab } from './documents-tab'
import type { RowHover } from './shared'
import { SummaryTab } from './summary-tab'

const tabs: Array<{ id: RequirementInsightTab; label: string; Icon: LucideIcon }> = [
  { id: 'summary', label: 'Summary', Icon: Sparkles },
  { id: 'charts', label: 'Charts', Icon: ChartLine },
  { id: 'documents', label: 'Documents', Icon: Layers },
]

export function InsightPanel({
  workspace,
  requirement,
  tab,
  onTab,
  onClose,
  onHover,
  onActivateTask,
}: {
  workspace: string
  requirement: { id: string; title: string }
  tab: RequirementInsightTab
  onTab: (tab: RequirementInsightTab) => void
  onClose: () => void
  onHover: RowHover
  onActivateTask: (id: string) => void
}) {
  const id = useId()
  const latestHover = useRef(onHover)
  latestHover.current = onHover
  // A tab change or the panel closing clears any row emphasis on the map.
  useEffect(() => () => latestHover.current(undefined), [tab])
  const tabRefs = useRef<Partial<Record<RequirementInsightTab, HTMLButtonElement | null>>>({})
  const onTabKey = (event: KeyboardEvent) => {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
    event.preventDefault()
    const index = tabs.findIndex((entry) => entry.id === tab)
    const next = tabs[(index + (event.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length].id
    onTab(next)
    tabRefs.current[next]?.focus()
  }
  return (
    <aside
      aria-label="Requirement insights"
      className="absolute inset-0 z-20 flex min-w-0 flex-col bg-surface md:static md:z-auto md:w-[360px] md:shrink-0 md:border-l md:border-edge md:shadow-[-12px_0_32px_rgb(0_0_0/0.08)] lg:w-[404px]"
    >
      <div className="shrink-0 px-4 pt-3.5">
        <div className="flex items-center gap-1.5 text-[11px] text-muted">
          <BookText aria-hidden className="size-3.5" />
          Requirement
          <span className="truncate font-mono text-[10.5px] text-faint">{requirement.id}</span>
          <Button
            size="icon"
            variant="ghost"
            aria-label="Close insights"
            title="Close"
            onClick={onClose}
            className="-mr-2 ml-auto size-7"
          >
            <X />
          </Button>
        </div>
        <h2 className="mb-2.5 mt-0.5 text-[15px] font-semibold leading-snug text-foreground">{requirement.title}</h2>
        <div role="tablist" aria-label="Insight views" className="flex gap-4 border-b border-border">
          {tabs.map(({ id: value, label, Icon }) => (
            <button
              key={value}
              ref={(element) => {
                tabRefs.current[value] = element
              }}
              type="button"
              role="tab"
              id={`${id}-${value}`}
              aria-selected={tab === value}
              aria-controls={`${id}-panel`}
              tabIndex={tab === value ? 0 : -1}
              onClick={() => onTab(value)}
              onKeyDown={onTabKey}
              className={cn(
                '-mb-px inline-flex items-center gap-1.5 border-b-2 pb-2 pt-1.5 text-xs font-medium transition-colors focus-visible:outline-2 focus-visible:outline-primary',
                tab === value
                  ? 'border-primary text-foreground'
                  : 'border-transparent text-muted hover:text-foreground',
              )}
            >
              <Icon aria-hidden className="size-3.5" />
              {label}
            </button>
          ))}
        </div>
      </div>
      <div
        role="tabpanel"
        id={`${id}-panel`}
        aria-labelledby={`${id}-${tab}`}
        className="min-h-0 flex-1 overflow-y-auto px-4 pb-5 pt-3.5"
      >
        {tab === 'summary' && (
          <SummaryTab
            workspace={workspace}
            requirementId={requirement.id}
            onHover={onHover}
            onActivateTask={onActivateTask}
          />
        )}
        {tab === 'charts' && <ChartsTab workspace={workspace} requirementId={requirement.id} />}
        {tab === 'documents' && <DocumentsTab workspace={workspace} requirementId={requirement.id} onHover={onHover} />}
      </div>
    </aside>
  )
}
