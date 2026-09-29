import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { RefreshCw } from 'lucide-react'
import { fetchWorkspaceVerificationKits } from '../../lib/api'
import type {
  VerificationKitRepository,
  VerificationKitStatus,
  VerificationKitUnavailableReason,
} from '../../lib/types'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { VerificationDiagnostics, VerificationKitRow } from './verification-kit-row'

// Kits declared in each configured repository's base-branch manifest, and how
// their pins compare with the confirmed documents (component-web-dashboard
// VK-WEB-4, req-verification-kits REQ-11). The query has its own cadence:
// activity, SSE and configuration saves never refetch it.

const COUNT_LABEL: Partial<Record<VerificationKitStatus, string>> = {
  behind: 'behind',
  pending: 'awaiting confirmation',
  unresolved: 'unresolved',
  unpinned: 'without pins',
  invalid: 'invalid',
}

function headerCount(repo: VerificationKitRepository) {
  const total = `${repo.kits.length} ${repo.kits.length === 1 ? 'kit' : 'kits'}`
  const counts = new Map<string, number>()
  for (const kit of repo.kits) {
    const label = COUNT_LABEL[kit.status]
    if (label) counts.set(label, (counts.get(label) ?? 0) + 1)
  }
  return [total, ...[...counts].map(([label, count]) => `${count} ${label}`)].join(' · ')
}

function UnavailableReason({ reason }: { reason?: VerificationKitUnavailableReason }) {
  switch (reason) {
    case 'no_app':
      return (
        <>
          Connect the workspace GitHub App in{' '}
          <Link to="/settings" className="text-primary hover:underline">
            Settings
          </Link>{' '}
          so Conveyor can read the manifest.
        </>
      )
    case 'permission':
      return <>The GitHub App cannot read this repository. Grant it access to the repository and refresh.</>
    case 'unknown_revision':
      return <>The base branch was not found on GitHub. Check the repository's base branch in General.</>
    default:
      return <>The GitHub read failed. Refresh to try again.</>
  }
}

function RepositorySection({ repo }: { repo: VerificationKitRepository }) {
  return (
    <section aria-label={repo.repository} className="rounded-lg border border-border bg-card">
      <header className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 border-b border-border px-4 py-3">
        <h3 className="text-sm font-semibold">{repo.repository}</h3>
        <span className="font-mono text-xs text-faint">{repo.base}</span>
        {repo.commit_sha && (
          <span className="font-mono text-xs text-faint" title={repo.commit_sha}>
            @ {repo.commit_sha.slice(0, 8)}
          </span>
        )}
        {repo.state !== 'unavailable' && repo.state !== 'no_manifest' && (
          <span className="ml-auto text-xs text-muted">{headerCount(repo)}</span>
        )}
      </header>
      {repo.state === 'no_manifest' && (
        <p className="px-4 py-3 text-sm text-muted">
          No kits declared in <span className="font-mono text-xs">.conveyor/kits/manifest.yaml</span>.
        </p>
      )}
      {repo.state === 'unavailable' && (
        <div className="px-4 py-3 text-sm">
          <p className="font-medium">Couldn't read the kit manifest</p>
          <p className="text-muted">
            <UnavailableReason reason={repo.reason} />
          </p>
        </div>
      )}
      {repo.state === 'invalid' && repo.diagnostics.length > 0 && (
        <div className="px-4 pt-3">
          <VerificationDiagnostics diagnostics={repo.diagnostics} />
        </div>
      )}
      {(repo.state === 'ok' || repo.state === 'invalid') && repo.kits.length > 0 && (
        <div className="pt-1">
          {repo.kits.map((kit) => (
            <VerificationKitRow key={kit.id} kit={kit} />
          ))}
        </div>
      )}
      {repo.state === 'ok' && repo.kits.length === 0 && (
        <p className="px-4 py-3 text-sm text-muted">The manifest declares no kits.</p>
      )}
    </section>
  )
}

export function VerificationKits({ workspace }: { workspace: string }) {
  const query = useQuery({
    queryKey: ['verification-kits', workspace],
    queryFn: ({ signal }) => fetchWorkspaceVerificationKits(workspace, signal),
    enabled: Boolean(workspace),
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Verification kits</CardTitle>
        <Button
          size="sm"
          variant="secondary"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
          aria-label="Refresh verification kits"
        >
          <RefreshCw className={query.isFetching ? 'animate-spin' : undefined} />
          Refresh
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-xs text-faint">
          Kits declared at the head of each repository's base branch. Status compares each kit's pinned documents with
          the confirmed versions; verification itself uses the versions a task was filed with.
        </p>
        {query.isPending && <p className="text-sm text-muted">Loading kits…</p>}
        {query.isError && (
          <p className="rounded-md border border-failure/30 bg-failure-soft p-3 text-sm text-failure">
            {query.error.message}
          </p>
        )}
        {query.data?.repositories.length === 0 && (
          <p className="text-sm text-muted">No repositories are configured for this workspace.</p>
        )}
        {query.data?.repositories.map((repo) => (
          <RepositorySection key={repo.repository} repo={repo} />
        ))}
      </CardContent>
    </Card>
  )
}
