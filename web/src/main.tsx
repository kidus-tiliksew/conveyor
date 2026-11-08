import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { slowReadQueryDefaults, workspacesQueryDefaults } from './components/app-shell'
import { router } from './router'
import '@fontsource-variable/inter'
import '@fontsource-variable/jetbrains-mono'
import './styles.css'

// crypto.randomUUID exists only in secure contexts, and a self-hosted factory
// may serve the dashboard over plain HTTP on a private network.
if (typeof crypto.randomUUID !== 'function') {
  crypto.randomUUID = () =>
    '10000000-1000-4000-8000-100000000000'.replace(/[018]/g, (digit) =>
      (Number(digit) ^ (crypto.getRandomValues(new Uint8Array(1))[0] & (15 >> (Number(digit) / 4)))).toString(16),
    ) as ReturnType<typeof crypto.randomUUID>
}

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 5_000, retry: 1, refetchOnWindowFocus: true } },
})
queryClient.setQueryDefaults(['workspaces'], workspacesQueryDefaults)
// Slow-changing families share one five-minute freshness, including readers
// outside the shell that set no stale time (component-web-dashboard).
for (const family of ['caller-identity', 'workspace-members', 'blueprints']) {
  queryClient.setQueryDefaults([family], slowReadQueryDefaults)
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
