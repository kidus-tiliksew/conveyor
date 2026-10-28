import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { slowReadQueryDefaults, workspacesQueryDefaults } from './components/app-shell'
import { router } from './router'
import '@fontsource/ibm-plex-sans/400.css'
import '@fontsource/ibm-plex-sans/500.css'
import '@fontsource/ibm-plex-sans/600.css'
import '@fontsource/ibm-plex-sans/700.css'
import './styles.css'

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
