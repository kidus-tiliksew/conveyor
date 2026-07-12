import { useMemo } from 'react'
import { Link } from '@tanstack/react-router'
import { ArrowRight } from 'lucide-react'
import { groupForSummary, parseProvenance } from '../lib/activity'
import { stageGroups } from '../lib/contracts'
import { relativeTime } from '../lib/utils'
import { useActivity, useWorkspace } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'

// Overview: the factory's health at a glance — stage distribution (spec
// §13.3), what needs a human, and what the workspace is running.
export function HomePage() {
  const { data: activity, isLoading } = useActivity()
  const { data: workspace } = useWorkspace()

  const counts = useMemo(() => {
    const byGroup = new Map<string, number>()
    for (const item of activity ?? []) {
      const key = groupForSummary(item)
      byGroup.set(key, (byGroup.get(key) ?? 0) + 1)
    }
    return byGroup
  }, [activity])

  const attention = (activity ?? [])
    .filter((item) => item.needs_attention)
    .sort((a, b) => new Date(b.last_event_at || b.task.created_at).getTime() - new Date(a.last_event_at || a.task.created_at).getTime())

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl px-6 py-8">
        <h1 className="text-xl font-semibold tracking-tight">
          {workspace ? `${workspace.workspace} workspace` : 'Conveyor'}
        </h1>
        <p className="mt-1 text-sm text-muted">The factory at a glance.</p>

        <div className="mt-6 grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
          {stageGroups
            .filter(({ key }) => key !== 'done')
            .map(({ key, label }) => (
              <Link
                key={key}
                to="/activity"
                className="rounded-xl border border-border px-4 py-3 transition-colors hover:bg-surface"
              >
                <p className="text-[11px] font-medium uppercase tracking-wider text-faint">{label}</p>
                <p className="mt-1 text-2xl font-semibold tabular-nums">
                  {isLoading ? '–' : counts.get(key) ?? 0}
                </p>
              </Link>
            ))}
        </div>

        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Needs attention</CardTitle>
            {attention.length > 0 && <Badge variant="attention">{attention.length}</Badge>}
          </CardHeader>
          <CardContent className="p-2">
            {isLoading && <Skeleton className="m-2 h-12" />}
            {!isLoading && attention.length === 0 && (
              <p className="px-3 py-4 text-sm text-muted">Nothing is waiting on a human. The line is moving.</p>
            )}
            {attention.map((item) => (
              <Link
                key={item.task.id}
                to="/tasks/$taskId"
                params={{ taskId: item.task.id }}
                className="flex items-center gap-3 rounded-lg px-3 py-2.5 transition-colors hover:bg-surface"
              >
                <span className="font-mono text-xs text-faint">{item.task.id}</span>
                <span className="min-w-0 flex-1 truncate text-sm font-medium">{item.task.title}</span>
                <Badge variant="accent" className="max-w-40 truncate">{parseProvenance(item.task.source).label}</Badge>
                <span className="text-xs text-faint">{relativeTime(item.last_event_at || item.task.created_at)}</span>
                <ArrowRight className="size-3.5 text-faint" />
              </Link>
            ))}
          </CardContent>
        </Card>

        {workspace && (
          <Card className="mt-4">
            <CardHeader>
              <CardTitle>Workspace</CardTitle>
              <Link to="/workspace" className="text-xs text-primary hover:underline">
                Configuration
              </Link>
            </CardHeader>
            <CardContent className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm md:grid-cols-4">
              <div>
                <p className="text-[11px] uppercase tracking-wider text-faint">Repos</p>
                <p className="mt-0.5 font-medium">{workspace.repos?.length ?? 0}</p>
              </div>
              <div>
                <p className="text-[11px] uppercase tracking-wider text-faint">Credentials</p>
                <p className="mt-0.5 font-medium">{workspace.credentials?.length ?? 0}</p>
              </div>
              <div>
                <p className="text-[11px] uppercase tracking-wider text-faint">Base image</p>
                <p className="mt-0.5 truncate font-mono text-xs">{workspace.image}</p>
              </div>
              <div>
                <p className="text-[11px] uppercase tracking-wider text-faint">Database</p>
                <p className="mt-0.5 font-medium">{workspace.database}</p>
              </div>
            </CardContent>
          </Card>
        )}
      </div>
    </div>
  )
}
