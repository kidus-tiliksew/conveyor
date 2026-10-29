import { expect, type Page, type Request, type Route, test } from '@playwright/test'
import type { ActivitySummary, TaskMapItem, TaskMapResponse } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'

// A workspace switch never shows the previous workspace's tasks, and the
// selected workspace's first activity read never waits for the previous one
// (component-web-dashboard, Shell and workspace scoping and Server-state
// discipline; component-web-task-surfaces, Board map Read). Every held read is
// a route response barrier, so no case sleeps or depends on timing.

const alpha = { id: 'alpha', name: 'Alpha' }
const bravo = { id: 'bravo', name: 'Bravo' }

// Recent enough for the Board's default 30-day Updated filter, although the
// mock serves every task whatever the filter says.
const recent = new Date(Date.now() - 60 * 60 * 1000).toISOString()

function summary(workspace: string, id: string, title: string): ActivitySummary {
  return {
    task: {
      id,
      workspace,
      source: 'operator',
      title,
      repo: 'conveyor',
      branch: `conveyor/${id}`,
      state: 'running',
      created_at: recent,
    },
    latest_stage: 'implement',
    last_event_at: recent,
    needs_attention: false,
  } as ActivitySummary
}

function mapTask(id: string, title: string): TaskMapItem {
  return {
    id,
    title,
    repo: 'conveyor',
    state: 'running',
    last_event_at: recent,
    next_action: {
      label: 'Agent working',
      actor: 'agent',
      kind: 'working',
      judgment: false,
      summary: 'Server summary: Agent working.',
      deadline: null,
    },
    depends_on: [],
    blocking_task_ids: [],
    unsatisfiable_task_ids: [],
    requirements: [],
  }
}

const activityByWorkspace: Record<string, ActivitySummary[]> = {
  alpha: [summary('alpha', 'alpha-1', 'Alpha only change'), summary('alpha', 'alpha-2', 'Alpha second change')],
  bravo: [summary('bravo', 'bravo-1', 'Bravo only change')],
}

const mapByWorkspace: Record<string, TaskMapResponse> = {
  alpha: { tasks: [mapTask('alpha-1', 'Alpha only change')], requirements: [] },
  bravo: { tasks: [mapTask('bravo-1', 'Bravo only change')], requirements: [] },
}

// A response the test releases on its own schedule.
class Barrier {
  private open!: () => void
  readonly released = new Promise<void>((resolve) => {
    this.open = resolve
  })
  release() {
    this.open()
  }
}

type Server = {
  memberships: Array<{ id: string; name: string }>
  // Returns a barrier the matching response waits for, or nothing to answer at once.
  holdActivity: (workspace: string, request: Request) => Barrier | undefined
  holdMap: (workspace: string) => Barrier | undefined
}

async function mockServer(page: Page, initial: string): Promise<Server> {
  const server: Server = {
    memberships: [alpha, bravo],
    holdActivity: () => undefined,
    holdMap: () => undefined,
  }
  await page.addInitScript((selected) => {
    localStorage.setItem('conveyor-theme', 'dark')
    if (!localStorage.getItem('conveyor-workspace')) localStorage.setItem('conveyor-workspace', selected)
  }, initial)
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const workspace = url.searchParams.get('workspace_id') ?? request.headers()['x-workspace-id'] ?? ''
    if (url.pathname === '/v1/workspaces') return route.fulfill({ json: server.memberships })
    if (url.pathname === '/v1/me') {
      return route.fulfill({
        json: callerIdentity({ id: 'usr_operator', email: 'operator@example.test', role: 'operator' }, request),
      })
    }
    if (url.pathname === '/v1/workspace') {
      return route.fulfill({ json: { workspace, repos: [{ name: 'conveyor' }] } })
    }
    if (url.pathname === '/v1/pending-proposals') {
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    }
    if (url.pathname === '/v1/activity') {
      const barrier = server.holdActivity(workspace, request)
      if (barrier) await barrier.released
      const items = (activityByWorkspace[workspace] ?? []).filter((item) => {
        const query = url.searchParams.get('q')?.toLocaleLowerCase()
        return !query || item.task.title.toLocaleLowerCase().includes(query)
      })
      // The browser may have aborted the read while it was held.
      return route
        .fulfill({
          headers: {
            'X-Conveyor-Total': String(items.length),
            'X-Conveyor-Limit': url.searchParams.get('limit') ?? '100',
            'X-Conveyor-Offset': url.searchParams.get('offset') ?? '0',
          },
          json: items,
        })
        .catch(() => undefined)
    }
    if (url.pathname === '/v1/task-map') {
      const barrier = server.holdMap(workspace)
      if (barrier) await barrier.released
      return route
        .fulfill({ json: mapByWorkspace[workspace] ?? { tasks: [], requirements: [] } })
        .catch(() => undefined)
    }
    return route.fulfill({ json: [] })
  })
  return server
}

const card = (page: Page, title: string) => page.getByText(title, { exact: true })

test('a rail switch shows no previous-workspace card while the selected workspace loads', async ({ page }) => {
  const server = await mockServer(page, 'alpha')
  await page.goto('/')
  await expect(card(page, 'Alpha only change')).toBeVisible()

  const bravoRead = new Barrier()
  server.holdActivity = (workspace) => (workspace === 'bravo' ? bravoRead : undefined)
  const bravoRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === '/v1/activity' &&
      new URL(request.url()).searchParams.get('workspace_id') === 'bravo',
  )
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Bravo' }).click()
  await bravoRequest

  await expect(
    page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Bravo' }),
  ).toHaveAttribute('aria-current', 'true')
  await expect(card(page, 'Alpha only change')).toHaveCount(0)
  await expect(card(page, 'Alpha second change')).toHaveCount(0)
  await expect(card(page, 'Bravo only change')).toHaveCount(0)

  bravoRead.release()
  await expect(card(page, 'Bravo only change')).toBeVisible()
  await expect(card(page, 'Alpha only change')).toHaveCount(0)
})

test('an activity read in flight at the switch is aborted and does not delay the selected workspace', async ({
  page,
}) => {
  await installQueryClientProbe(page)
  const server = await mockServer(page, 'alpha')
  await page.goto('/')
  await expect(card(page, 'Alpha only change')).toBeVisible()

  // Hold alpha's next read, the one a Board refresh sends, and never release it
  // before bravo's read has been sent.
  const alphaRead = new Barrier()
  server.holdActivity = (workspace) => (workspace === 'alpha' ? alphaRead : undefined)
  const alphaRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === '/v1/activity' &&
      new URL(request.url()).searchParams.get('workspace_id') === 'alpha',
  )
  const alphaAborted = page.waitForEvent(
    'requestfailed',
    (request) =>
      new URL(request.url()).pathname === '/v1/activity' &&
      new URL(request.url()).searchParams.get('workspace_id') === 'alpha',
  )
  void withQueryClient(
    page,
    (client) =>
      void (client as unknown as { refetchQueries(filter: { queryKey: unknown[] }): Promise<void> }).refetchQueries({
        queryKey: ['activity', 'alpha'],
      }),
    undefined,
  )
  await alphaRequest

  const bravoRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === '/v1/activity' &&
      new URL(request.url()).searchParams.get('workspace_id') === 'bravo',
  )
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Bravo' }).click()

  // Alpha's response is still held, so bravo's read proves it did not queue
  // behind it, and the browser aborted alpha's read.
  await bravoRequest
  await alphaAborted
  await expect(card(page, 'Bravo only change')).toBeVisible()
  await expect(card(page, 'Alpha only change')).toHaveCount(0)
  alphaRead.release()
  await expect(card(page, 'Alpha only change')).toHaveCount(0)
})

test('a filter change in one workspace keeps the previous page while the new read loads', async ({ page }) => {
  const server = await mockServer(page, 'alpha')
  await page.goto('/')
  await expect(card(page, 'Alpha second change')).toBeVisible()

  const filtered = new Barrier()
  server.holdActivity = (workspace, request) =>
    workspace === 'alpha' && new URL(request.url()).searchParams.get('q') ? filtered : undefined
  const filteredRequest = page.waitForRequest(
    (request) => new URL(request.url()).pathname === '/v1/activity' && !!new URL(request.url()).searchParams.get('q'),
  )
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('only')
  await filteredRequest

  // The previous page stays on the Board while the filtered read is held.
  await expect(card(page, 'Alpha only change')).toBeVisible()
  await expect(card(page, 'Alpha second change')).toBeVisible()

  filtered.release()
  await expect(card(page, 'Alpha second change')).toHaveCount(0)
  await expect(card(page, 'Alpha only change')).toBeVisible()
})

test('a Board map whose workspace changes under it shows no previous-workspace task', async ({ page }) => {
  await installQueryClientProbe(page)
  const server = await mockServer(page, 'alpha')
  await page.goto('/?view=map')
  await expect(page.getByTestId('rf__node-alpha-1')).toBeVisible()

  // A membership refresh that leaves one workspace selects it in place, with
  // the map still mounted (component-web-dashboard, Shell and workspace
  // scoping).
  const bravoMap = new Barrier()
  server.holdMap = (workspace) => (workspace === 'bravo' ? bravoMap : undefined)
  server.memberships = [bravo]
  const bravoRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === '/v1/task-map' &&
      new URL(request.url()).searchParams.get('workspace_id') === 'bravo',
  )
  await withQueryClient(
    page,
    (client) =>
      (client as unknown as { refetchQueries(filter: { queryKey: unknown[] }): Promise<void> }).refetchQueries({
        queryKey: ['workspaces'],
      }),
    undefined,
  )
  await bravoRequest

  await expect(page).toHaveURL(/view=map/)
  await expect(page.getByTestId('rf__node-alpha-1')).toHaveCount(0)
  await expect(page.getByTestId('rf__node-bravo-1')).toHaveCount(0)

  bravoMap.release()
  await expect(page.getByTestId('rf__node-bravo-1')).toBeVisible()
  await expect(page.getByTestId('rf__node-alpha-1')).toHaveCount(0)
})
