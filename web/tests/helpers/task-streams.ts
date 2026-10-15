import type { Page } from '@playwright/test'

// A controlled task stream (component-web-task-surfaces, Live updates). The
// page's own `fetch` is replaced for the task stream only, so each connection
// stays pending until the test opens, fails, or answers it, and an opened body
// stays open until the test closes or errors it. Playwright's clock holds the
// retry waits and the 250 ms refresh delay, so no assertion races wall time.
export type TaskStreamControl = {
  urls(): string[]
  aborted(): boolean[]
  open(): void
  respond(status: number, body: string | null): void
  fail(): void
  send(text: string): void
  close(): void
  error(): void
}

export async function controlTaskStreams(page: Page) {
  await page.addInitScript(() => {
    type Connection = {
      url: string
      aborted: boolean
      settled: boolean
      resolve: (response: Response) => void
      reject: (reason: unknown) => void
      body?: ReadableStreamDefaultController<Uint8Array>
    }
    const connections: Connection[] = []
    const nativeFetch = window.fetch.bind(window)
    window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      if (!url.includes('/events/stream')) return nativeFetch(input, init)
      return new Promise<Response>((resolve, reject) => {
        const connection: Connection = { url, aborted: false, settled: false, resolve, reject }
        connections.push(connection)
        init?.signal?.addEventListener('abort', () => {
          connection.aborted = true
          const reason = new DOMException('The operation was aborted.', 'AbortError')
          reject(reason)
          try {
            connection.body?.error(reason)
          } catch {
            // The body already ended.
          }
        })
      })
    }
    // The newest connection the page has not aborted; StrictMode's discarded
    // first effect leaves an aborted connection behind it.
    const live = () => {
      const connection = connections.findLast((candidate) => !candidate.aborted)
      if (!connection) throw new Error('no live task stream connection')
      return connection
    }
    const settle = (response: Response | Error) => {
      const connection = live()
      if (connection.settled) throw new Error('the live task stream connection was already answered')
      connection.settled = true
      if (response instanceof Error) connection.reject(response)
      else connection.resolve(response)
      return connection
    }
    const control: TaskStreamControl = {
      urls: () => connections.map((connection) => connection.url),
      aborted: () => connections.map((connection) => connection.aborted),
      open: () => {
        let body: ReadableStreamDefaultController<Uint8Array> | undefined
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            body = controller
          },
        })
        settle(new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })).body = body
      },
      respond: (status, body) => {
        settle(new Response(body, { status }))
      },
      fail: () => {
        settle(new TypeError('Failed to fetch'))
      },
      send: (text) => live().body?.enqueue(new TextEncoder().encode(text)),
      close: () => live().body?.close(),
      error: () => live().body?.error(new TypeError('network error')),
    }
    ;(window as unknown as { __taskStreams: TaskStreamControl }).__taskStreams = control
  })
  const call = <K extends keyof TaskStreamControl>(name: K, ...args: Parameters<TaskStreamControl[K]>) =>
    page.evaluate(
      ([method, values]) =>
        // biome-ignore lint/suspicious/noExplicitAny: forwards a typed call into the page.
        (window as unknown as { __taskStreams: Record<string, (...input: any[]) => unknown> }).__taskStreams[method](
          ...values,
        ) as ReturnType<TaskStreamControl[K]>,
      [name, args] as const,
    )
  return {
    urls: () => call('urls'),
    count: async () => (await call('urls')).length,
    countFor: async (taskId: string) =>
      (await call('urls')).filter((url) => url.includes(`/v1/tasks/${taskId}/events/stream`)).length,
    aborted: () => call('aborted'),
    open: () => call('open'),
    respond: (status: number, body: string | null) => call('respond', status, body),
    fail: () => call('fail'),
    send: (text: string) => call('send', text),
    close: () => call('close'),
    error: () => call('error'),
  }
}

// The notify stream's frames (component-http-api, Live task event stream).
export const readyFrame = 'event: ready\ndata: {}\n\n'
export const activityFrame = 'event: activity\ndata: {}\n\n'
