import { ExternalLink, Info } from 'lucide-react'
import { stageLabels } from '../lib/contracts'
import { usd } from '../lib/utils'
import { useWorkspace } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'

// Workspace management: repos and their sandbox environments, harness/model
// routing per stage, and the credential pool — the conveyor.yaml surface,
// read-only by design (spec §2.1: config is an operator-owned file).
export function WorkspacePage() {
  const { data: workspace, isLoading, error } = useWorkspace()

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl px-6 py-8">
        <h1 className="text-xl font-semibold tracking-tight">Workspace</h1>
        <p className="mt-1 text-sm text-muted">
          Repositories, sandbox environments, harness routing, and credentials.
        </p>

        {isLoading && <Skeleton className="mt-6 h-64" />}
        {error != null && (
          <p className="mt-6 rounded-lg bg-failure-soft p-3 text-sm text-failure">
            Workspace snapshot unavailable: {String(error)}
          </p>
        )}
        {workspace && (
          <div className="mt-6 space-y-4">
            <div className="flex items-start gap-2 rounded-lg border border-border bg-surface px-3 py-2.5 text-xs leading-5 text-muted">
              <Info className="mt-0.5 size-3.5 shrink-0" />
              <span>
                This is a read-only snapshot of <code className="font-mono">conveyor.yaml</code>. Edit the file and
                restart <code className="font-mono">conveyord</code> to change it — configuration is versioned with the
                deployment, not mutated from the dashboard (spec §2.1).
              </span>
            </div>

            <Card>
              <CardHeader>
                <CardTitle>Overview</CardTitle>
              </CardHeader>
              <CardContent className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm md:grid-cols-4">
                <Fact label="Workspace" value={workspace.workspace} />
                <Fact label="Base image" value={<code className="font-mono text-xs">{workspace.image}</code>} />
                <Fact label="Database" value={workspace.database} />
                <Fact label="Max bounces" value={String(workspace.max_bounces)} />
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Repositories & environments</CardTitle>
                <span className="text-xs text-faint">{workspace.repos?.length ?? 0}</span>
              </CardHeader>
              <CardContent className="space-y-3">
                {(workspace.repos ?? []).map((repo) => (
                  <div key={repo.name} className="rounded-lg border border-border p-3">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-sm font-semibold">{repo.name}</span>
                      <Badge variant="mono">base: {repo.base}</Badge>
                      {repo.github ? (
                        <a
                          href={`https://github.com/${repo.github}`}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                        >
                          {repo.github}
                          <ExternalLink className="size-3" />
                        </a>
                      ) : (
                        <Badge>issue polling off</Badge>
                      )}
                    </div>
                    <dl className="mt-2.5 grid grid-cols-2 gap-x-6 gap-y-1.5 text-xs md:grid-cols-3">
                      <Fact label="Sandbox image" value={<code className="font-mono">{repo.image}</code>} />
                      <Fact label="Secrets delivered" value={`${repo.secret_ref_count} ref${repo.secret_ref_count === 1 ? '' : 's'}`} />
                      <Fact
                        label="Tool policy"
                        value={
                          repo.allowed_commands?.length || repo.denied_commands?.length
                            ? `${repo.allowed_commands?.length ?? 0} allowed · ${repo.denied_commands?.length ?? 0} denied`
                            : 'unrestricted'
                        }
                      />
                    </dl>
                    {(repo.allowed_commands?.length ?? 0) > 0 && (
                      <div className="mt-2 flex flex-wrap gap-1.5">
                        {repo.allowed_commands!.map((command) => (
                          <Badge key={command.join(' ')} variant="positive" className="font-mono">
                            {command.join(' ')}
                          </Badge>
                        ))}
                        {repo.denied_commands?.map((command) => (
                          <Badge key={command.join(' ')} variant="failure" className="font-mono">
                            {command.join(' ')}
                          </Badge>
                        ))}
                      </div>
                    )}
                  </div>
                ))}
                {(workspace.repos?.length ?? 0) === 0 && <p className="text-sm text-muted">No repositories configured.</p>}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Stage routing — harness & model</CardTitle>
              </CardHeader>
              <CardContent className="p-0">
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b border-border text-left text-[11px] uppercase tracking-wider text-faint">
                        <th className="px-4 py-2 font-medium">Stage</th>
                        <th className="px-4 py-2 font-medium">Harnesses</th>
                        <th className="px-4 py-2 font-medium">Model tier</th>
                        <th className="px-4 py-2 font-medium">Budget</th>
                        <th className="px-4 py-2 font-medium">Timeout</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(workspace.routing ?? []).map((route) => (
                        <tr key={route.stage} className="border-b border-border last:border-0">
                          <td className="px-4 py-2.5 font-medium">{stageLabels[route.stage] ?? route.stage}</td>
                          <td className="px-4 py-2.5">
                            <span className="flex flex-wrap gap-1.5">
                              {route.harnesses.map((harness, order) => (
                                <Badge key={harness} variant={order === 0 ? 'accent' : 'default'} className="font-mono">
                                  {harness}
                                </Badge>
                              ))}
                            </span>
                          </td>
                          <td className="px-4 py-2.5 text-muted">{route.model_tier || '—'}</td>
                          <td className="px-4 py-2.5 font-mono text-xs tabular-nums">{usd(route.budget_usd)}</td>
                          <td className="px-4 py-2.5 font-mono text-xs">{route.timeout}</td>
                        </tr>
                      ))}
                      {(workspace.routing?.length ?? 0) === 0 && (
                        <tr>
                          <td colSpan={5} className="px-4 py-4 text-sm text-muted">
                            No routing overrides — every stage uses the default credential and a {`2h`} timeout.
                          </td>
                        </tr>
                      )}
                    </tbody>
                  </table>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Credential pool</CardTitle>
                <span className="text-xs text-faint">{workspace.credentials?.length ?? 0}</span>
              </CardHeader>
              <CardContent className="space-y-2">
                {(workspace.credentials ?? []).map((credential) => (
                  <div key={credential.id} className="flex flex-wrap items-center gap-2 rounded-lg border border-border px-3 py-2">
                    <span className="font-mono text-xs">{credential.id}</span>
                    <Badge variant="accent" className="font-mono">{credential.harness}</Badge>
                    <Badge className="font-mono">{credential.vendor}</Badge>
                    <Badge variant="mono">{credential.kind}</Badge>
                    <span className="ml-auto text-xs text-faint">
                      {credential.owner_kind} · {credential.owner_id}
                    </span>
                  </div>
                ))}
                {(workspace.credentials?.length ?? 0) === 0 && (
                  <p className="text-sm leading-6 text-muted">
                    No credentials in the pool — the local runner falls back to the operator's Codex login. Add
                    entries under <code className="font-mono">credentials:</code> in conveyor.yaml (spec §5.2).
                  </p>
                )}
              </CardContent>
            </Card>
          </div>
        )}
      </div>
    </div>
  )
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <dt className="text-[11px] uppercase tracking-wider text-faint">{label}</dt>
      <dd className="mt-0.5 min-w-0 truncate">{value}</dd>
    </div>
  )
}
