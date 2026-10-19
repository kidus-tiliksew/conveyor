import { expect, test } from '@playwright/test'
import { fetchTaskOperations } from '../src/lib/api'
import type { TaskOperationsGroupCounts } from '../src/lib/types'

// The contract function runs in the Playwright Node process against a stubbed
// fetch and stored workspace (component-web-dashboard, Contract layer).
type Captured = { url: URL }

const groupHeaders = {
  'X-Conveyor-Group-Operator-Total': '2',
  'X-Conveyor-Group-Agent-Total': '0',
  'X-Conveyor-Group-Waiting-Total': '13',
  'X-Conveyor-Group-Terminal-Total': '697',
}

const row = {
  task: { id: 'task-1' },
  last_event_at: '2026-10-19T08:00:00Z',
  needs_attention: true,
  plan: { status: 'none' },
}

const globals = globalThis as unknown as { fetch: typeof fetch; localStorage?: Pick<Storage, 'getItem'> }
const originalFetch = globals.fetch
const originalStorage = globals.localStorage

function stub(status: number, body: string, headers: Record<string, string>): Captured[] {
  const captured: Captured[] = []
  globals.localStorage = { getItem: (key: string) => (key === 'conveyor-workspace' ? 'demo' : null) }
  globals.fetch = (async (input: RequestInfo | URL) => {
    captured.push({ url: new URL(String(input), 'http://conveyor.test') })
    return new Response(body, { status, headers: { 'Content-Type': 'application/json', ...headers } })
  }) as typeof fetch
  return captured
}

test.afterEach(() => {
  globals.fetch = originalFetch
  globals.localStorage = originalStorage
})

test('an ungrouped read sends no group and returns no group counts', async () => {
  const captured = stub(200, JSON.stringify([row]), {
    'X-Conveyor-Total': '9',
    'X-Conveyor-Limit': '25',
    'X-Conveyor-Offset': '0',
    ...groupHeaders,
  })
  const page = await fetchTaskOperations({
    limit: 25,
    offset: 0,
    filter: { state: ['queued', 'running'], q: 'map', group: 'operator' },
  })
  expect(captured).toHaveLength(1)
  const params = captured[0].url.searchParams
  expect(captured[0].url.pathname).toBe('/v1/task-operations')
  expect(params.getAll('group')).toEqual([])
  expect(params.getAll('state')).toEqual(['queued', 'running'])
  expect(params.get('q')).toBe('map')
  expect(params.get('workspace_id')).toBe('demo')
  expect(page).toEqual({ items: [row], total: 9, limit: 25, offset: 0 })
  expect('groupCounts' in page).toBe(false)
})

test('a grouped read sends the typed selector once and parses every count', async () => {
  const captured = stub(200, JSON.stringify([row]), {
    'X-Conveyor-Total': '13',
    'X-Conveyor-Limit': '25',
    'X-Conveyor-Offset': '25',
    ...groupHeaders,
  })
  const page = await fetchTaskOperations({
    limit: 25,
    offset: 25,
    filter: { group: 'agent', repository: ['web'] },
    group: 'waiting',
  })
  const params = captured[0].url.searchParams
  expect(params.getAll('group')).toEqual(['waiting'])
  expect(params.getAll('repository')).toEqual(['web'])
  expect(params.get('limit')).toBe('25')
  expect(params.get('offset')).toBe('25')
  const counts: TaskOperationsGroupCounts = { operator: 2, agent: 0, waiting: 13, terminal: 697 }
  expect(page).toEqual({ items: [row], total: 13, limit: 25, offset: 25, groupCounts: counts })
})

for (const [name, value] of [
  ['missing', undefined],
  ['empty', ''],
  ['negative', '-1'],
  ['fractional', '1.5'],
  ['nondecimal', '0x10'],
  ['unsafe', '9007199254740993'],
] as const) {
  test(`a ${name} group count throws`, async () => {
    const headers: Record<string, string> = { ...groupHeaders }
    if (value === undefined) delete headers['X-Conveyor-Group-Waiting-Total']
    else headers['X-Conveyor-Group-Waiting-Total'] = value
    stub(200, '[]', headers)
    await expect(fetchTaskOperations({ limit: 25, offset: 0, group: 'operator' })).rejects.toThrow(
      'Invalid task operations group counts',
    )
  })
}

test('a refused grouped read throws the decoded error', async () => {
  stub(400, 'group must be supplied once as operator, agent, waiting, or terminal', { 'Content-Type': 'text/plain' })
  await expect(fetchTaskOperations({ limit: 25, offset: 0, group: 'terminal' })).rejects.toThrow(
    'group must be supplied once as operator, agent, waiting, or terminal',
  )
})
