import { useQuery } from '@tanstack/react-query'
import { useLocation, useNavigate } from '@tanstack/react-router'
import { useEffect, useId } from 'react'
import { fetchEnvironments } from '../../lib/api'
import { useWorkspaceSelection } from '../app-shell'
import { withQuerySignal } from '../task/use-task-detail'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Select } from '../ui/input'
import { Switch } from '../ui/switch'
import { type MonitorSearch, sameSearch, validateMonitorSearch } from './monitor-search'
import { ReportList } from './report-list'

/**
 * The workspace's reports on the Monitor page, newest `last_seen` first, with
 * a filter by environment, a filter for reports that belong to no crate, and
 * a switch to closed reports (component-feedback-reports invariant 26;
 * req-production-feedback AC-6.3). The filters live in the address.
 */
export function MonitorReports() {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  // The address as parsed, before the route's validation.
  const raw = useLocation({ select: (location) => location.search }) as Record<string, unknown>
  const search = validateMonitorSearch(raw)
  // Only the Monitor address is cleaned: while the page is left for another
  // address, the location already names that address.
  const onMonitor = useLocation({ select: (location) => location.pathname.endsWith('/monitor') })
  const clean = !onMonitor || sameSearch(raw, search)
  const id = useId()
  // The address holds exactly what the page shows: a value the page dropped
  // leaves the address in place, with no new history entry. The cleanup reads
  // the address at the moment it runs, so it never undoes a newer choice.
  useEffect(() => {
    if (!clean)
      void navigate({
        to: '/w/$workspaceId/monitor',
        params: { workspaceId: workspace },
        search: (previous: Record<string, unknown>) => validateMonitorSearch(previous),
        replace: true,
      })
  }, [clean, navigate, workspace])
  const setSearch = (change: Partial<Record<keyof MonitorSearch, unknown>>) =>
    void navigate({
      to: '/w/$workspaceId/monitor',
      params: { workspaceId: workspace },
      search: validateMonitorSearch({ ...search, ...change }),
    })
  const environments = useQuery({
    queryKey: ['environments', workspace, 'monitor-report-filter'],
    queryFn: (context) => withQuerySignal(context, (signal) => fetchEnvironments(workspace, false, signal)),
    enabled: Boolean(workspace),
    retry: false,
    refetchOnWindowFocus: false,
  })
  const rows = environments.data ?? []
  const unknownEnvironment =
    search.environment !== undefined &&
    environments.data !== undefined &&
    !rows.some((row) => row.id === search.environment)
  const closed = search.show === 'closed'
  return (
    <Card className="mt-6" data-monitor-reports>
      <CardHeader>
        <CardTitle>{closed ? 'Closed reports' : 'Open reports'}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex flex-wrap items-end gap-x-4 gap-y-2 text-xs">
          <div className="flex min-w-0 flex-col gap-1">
            <label htmlFor={`${id}-environment`} className="font-medium text-muted">
              Environment
            </label>
            <Select
              id={`${id}-environment`}
              value={search.environment ?? ''}
              onChange={(event) => setSearch({ environment: event.target.value || undefined })}
              className="max-w-64 text-xs"
            >
              <option value="">All environments</option>
              {rows.map((row) => (
                <option key={row.id} value={row.id}>
                  {row.name}
                </option>
              ))}
              {search.environment !== undefined && !rows.some((row) => row.id === search.environment) && (
                <option value={search.environment}>An environment that is not in the list</option>
              )}
            </Select>
          </div>
          <label className="inline-flex items-center gap-2 pb-1.5">
            <input
              type="checkbox"
              checked={search.no_crate === true}
              onChange={(event) => setSearch({ no_crate: event.target.checked ? true : undefined })}
            />
            <span>Only reports in no crate</span>
          </label>
          <span className="inline-flex items-center gap-2 pb-1">
            <Switch
              checked={closed}
              aria-label="Show closed reports"
              onChange={(value) => setSearch({ show: value ? 'closed' : undefined })}
            />
            <span aria-hidden>Show closed reports</span>
          </span>
        </div>
        {unknownEnvironment && (
          <p role="status" className="text-xs text-attention">
            The address names an environment that is not in the list of environments. Choose another environment or All
            environments.
          </p>
        )}
        {environments.error != null && (
          <p role="status" className="text-xs text-failure">
            The environments did not load, so the filter shows only All environments.
          </p>
        )}
        <ReportList
          filter={{
            state: closed ? 'closed' : 'open',
            environment_id: search.environment,
            no_crate: search.no_crate === true,
          }}
          label={closed ? 'Closed reports' : 'Open reports'}
          emptyText={
            closed
              ? 'No closed report matches these filters.'
              : search.environment || search.no_crate
                ? 'No open report matches these filters.'
                : 'No open report. Reports appear here when a watcher sends one.'
          }
        />
      </CardContent>
    </Card>
  )
}
