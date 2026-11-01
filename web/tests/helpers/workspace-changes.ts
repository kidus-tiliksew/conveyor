import type { Page } from '@playwright/test'

// A controlled workspace change stream (component-web-dashboard, Workspace
// change stream). The page's `EventSource` is replaced by a fake whose
// connections stay connecting until the spec sends `ready`, and which never
// retries on its own, so the dashboard's backoff and Playwright's clock decide
// every reconnect. React StrictMode's discarded first effect leaves a closed
// connection behind it; the controls act on the newest open connection.
export type WorkspaceChangeControl = {
  // Every connection the page opened, in order, with its address and state.
  connections(): Promise<Array<{ url: string; closed: boolean }>>
  // Connections the page has not closed.
  open(): Promise<number>
  // The server subscribed: the newest open connection receives `ready`.
  ready(): Promise<void>
  // One `activity` frame naming a task, or naming none for a folded notice.
  activity(taskId?: string): Promise<void>
  // The connection drops; a browser EventSource fires `error`.
  error(): Promise<void>
}

type PageControl = {
  connections(): Array<{ url: string; closed: boolean }>
  ready(): void
  activity(taskId?: string): void
  error(): void
}

export async function controlWorkspaceChanges(page: Page): Promise<WorkspaceChangeControl> {
  await page.addInitScript(() => {
    class FakeEventSource extends EventTarget {
      static readonly CONNECTING = 0
      static readonly OPEN = 1
      static readonly CLOSED = 2
      readonly CONNECTING = 0
      readonly OPEN = 1
      readonly CLOSED = 2
      readonly url: string
      readonly withCredentials = false
      readyState = 0
      onopen: ((event: Event) => void) | null = null
      onmessage: ((event: MessageEvent) => void) | null = null
      onerror: ((event: Event) => void) | null = null
      constructor(url: string | URL) {
        super()
        this.url = new URL(String(url), window.location.href).pathname
        sources.push(this)
      }
      close() {
        this.readyState = 2
      }
    }
    const sources: FakeEventSource[] = []
    const live = () => {
      const source = sources.findLast((candidate) => candidate.readyState !== 2)
      if (!source) throw new Error('no open workspace change stream')
      return source
    }
    const control: PageControl = {
      connections: () => sources.map((source) => ({ url: source.url, closed: source.readyState === 2 })),
      ready: () => {
        const source = live()
        source.readyState = 1
        source.dispatchEvent(new Event('open'))
        source.dispatchEvent(new MessageEvent('ready', { data: '{}' }))
      },
      activity: (taskId) => {
        live().dispatchEvent(
          new MessageEvent('activity', { data: JSON.stringify(taskId === undefined ? {} : { task_id: taskId }) }),
        )
      },
      error: () => {
        const source = live()
        source.readyState = 0
        source.dispatchEvent(new Event('error'))
      },
    }
    const target = window as unknown as Record<string, unknown>
    target.EventSource = FakeEventSource
    target.__workspaceChanges = control
  })
  type ControlWindow = { __workspaceChanges: PageControl }
  const connections = () => page.evaluate(() => (window as unknown as ControlWindow).__workspaceChanges.connections())
  return {
    connections,
    open: async () => (await connections()).filter((connection) => !connection.closed).length,
    ready: () => page.evaluate(() => (window as unknown as ControlWindow).__workspaceChanges.ready()),
    activity: (taskId) =>
      page.evaluate(
        (id) => (window as unknown as ControlWindow).__workspaceChanges.activity(id ?? undefined),
        taskId ?? null,
      ),
    error: () => page.evaluate(() => (window as unknown as ControlWindow).__workspaceChanges.error()),
  }
}
