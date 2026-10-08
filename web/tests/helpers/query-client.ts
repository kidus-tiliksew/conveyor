import type { Page } from '@playwright/test'

// A test-only window onto the application's QueryClient. The dev build of
// React reports every committed fiber root to a DevTools global hook; this
// installs a minimal hook before the app loads and finds the client in the
// QueryClientProvider's props. No product code exposes the client, so specs
// can assert exact cache-key effects (for example which task-detail entry a
// mutation invalidated) without relying on navigation, SSE, polling, or time.
export async function installQueryClientProbe(page: Page) {
  await page.addInitScript(() => {
    const target = window as unknown as Record<string, unknown>
    if (target.__REACT_DEVTOOLS_GLOBAL_HOOK__) return
    const renderers = new Map<number, unknown>()
    target.__REACT_DEVTOOLS_GLOBAL_HOOK__ = {
      renderers,
      supportsFiber: true,
      isDisabled: false,
      inject(renderer: unknown) {
        const id = renderers.size + 1
        renderers.set(id, renderer)
        return id
      },
      onCommitFiberRoot(_id: number, root: unknown) {
        target.__conveyorFiberRoot = root
      },
      onCommitFiberUnmount() {},
      onPostCommitFiberRoot() {},
      onScheduleFiberRoot() {},
      checkDCE() {},
    }
  })
}

// Runs `body` in the page with the application's QueryClient as its argument.
export async function withQueryClient<T, A>(
  page: Page,
  body: (client: QueryClientLike, arg: A) => T,
  arg: A,
): Promise<Awaited<T>> {
  return page.evaluate(
    ([source, value]) => {
      type Fiber = { child?: Fiber; sibling?: Fiber; memoizedProps?: { client?: unknown } }
      const root = (window as unknown as { __conveyorFiberRoot?: { current: Fiber } }).__conveyorFiberRoot
      const stack: Fiber[] = root ? [root.current] : []
      let client: unknown
      while (stack.length > 0 && !client) {
        const fiber = stack.pop() as Fiber
        const candidate = fiber.memoizedProps?.client as { getQueryCache?: unknown } | undefined
        if (candidate && typeof candidate.getQueryCache === 'function') client = candidate
        if (fiber.sibling) stack.push(fiber.sibling)
        if (fiber.child) stack.push(fiber.child)
      }
      if (!client) throw new Error('QueryClient not found in the committed React tree')
      // Indirect eval of the test's own function source, evaluated in the page.
      const fn = (0, eval)(`(${source})`) as (client: unknown, arg: unknown) => unknown
      return fn(client, value)
    },
    [body.toString(), arg] as const,
  ) as Promise<Awaited<T>>
}

export interface QueryClientLike {
  setQueryData(key: readonly unknown[], data: unknown): unknown
  getQueryState(key: readonly unknown[]): { isInvalidated: boolean; dataUpdatedAt: number } | undefined
}
