import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Archive, ArchiveRestore, KeyRound, Pencil, Plus, X } from 'lucide-react'
import { type ReactNode, useState } from 'react'
import {
  fetchEnvironments,
  fetchEnvironmentSuggestions,
  ignoreEnvironmentSuggestion,
  updateEnvironment,
} from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { environmentSuggestionsQueryKey, environmentsQueryKey } from '../../lib/query-keys'
import type { EnvironmentKey, EnvironmentRecord, EnvironmentRow, EnvironmentSuggestion } from '../../lib/types'
import { absoluteTime, relativeTime } from '../../lib/utils'
import { liveStatusInterval } from '../feedback/live-status'
import { WatcherState } from '../feedback/watcher-state'
import { withQuerySignal } from '../task/use-task-detail'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Card, CardContent } from '../ui/card'
import { EnvironmentFormDialog, type EnvironmentFormSeed } from './environment-form'
import { IssueKeyDialog, RevokeKeyDialog } from './environment-key-dialog'

// The most keys an environment holds that are not revoked
// (component-environments invariant 12).
const MAX_KEYS = 5

type FormState =
  | { mode: 'create'; seed?: EnvironmentFormSeed }
  | { mode: 'edit'; environment: EnvironmentRecord }
  | null
type KeyState =
  | { kind: 'issue'; environment: EnvironmentRecord }
  | { kind: 'revoke'; environment: EnvironmentRecord; key: EnvironmentKey }
  | null

/**
 * The Environments tab of the Workspace page (req-environments AC-1.6, AC-1.8,
 * AC-2.1; req-production-feedback AC-2.3; component-environments invariant 18;
 * component-production-feedback invariant 21). Every member who may view the
 * workspace reads the unarchived environments, and the archived ones on
 * request. The controls render only with manage_workspace, and the server
 * checks it again. The caller keys this panel by workspace, so a workspace
 * change unmounts its dialogs and drops their state. The active list reads
 * again every 60 seconds while the tab is visible and the list shows an
 * environment (component-web-dashboard invariant 15). The archived list and the
 * suggestions read on no timer.
 */
export function EnvironmentsPanel({
  workspace,
  canManage,
  repositories,
}: {
  workspace: string
  canManage: boolean
  repositories: string[]
}) {
  const queryClient = useQueryClient()
  const [showArchived, setShowArchived] = useState(false)
  const [form, setForm] = useState<FormState>(null)
  const [keyDialog, setKeyDialog] = useState<KeyState>(null)
  const active = useQuery({
    queryKey: environmentsQueryKey(workspace, false),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchEnvironments(workspace, false, signal)),
    enabled: Boolean(workspace),
    retry: false,
    refetchInterval: (query) => liveStatusInterval(query.state.data?.length),
  })
  const archived = useQuery({
    queryKey: environmentsQueryKey(workspace, true),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchEnvironments(workspace, true, signal)),
    enabled: Boolean(workspace) && showArchived,
    retry: false,
  })
  const suggestions = useQuery({
    queryKey: environmentSuggestionsQueryKey(workspace),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchEnvironmentSuggestions(workspace, signal)),
    enabled: Boolean(workspace) && canManage,
    retry: false,
  })
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['environments', workspace] })
  const setArchived = useMutation({
    mutationFn: ({ environment, archived }: { environment: EnvironmentRecord; archived: boolean }) =>
      updateEnvironment(workspace, environment.id, { archived }),
    onSuccess: refresh,
  })
  const ignore = useMutation({
    mutationFn: (suggestion: EnvironmentSuggestion) =>
      ignoreEnvironmentSuggestion(workspace, suggestion.github_environment),
    onSuccess: refresh,
  })
  const archiveError = (environment: EnvironmentRecord) =>
    setArchived.error && setArchived.variables?.environment.id === environment.id ? setArchived.error : null

  return (
    <div className="space-y-4" data-environments-panel>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 max-w-2xl">
          <h2 className="text-sm font-semibold">Environments</h2>
          <p className="mt-1 text-sm text-muted">
            An environment is a place where your software runs, such as staging or production. Conveyor uses it to show
            where each merged task is live.
          </p>
        </div>
        {canManage && (
          <Button size="sm" onClick={() => setForm({ mode: 'create' })}>
            <Plus />
            Add environment
          </Button>
        )}
      </div>

      {active.isPending && <p className="text-sm text-muted">Loading environments…</p>}
      {active.isError && (
        <div role="alert" className="flex flex-wrap items-center gap-3 rounded-md bg-failure-soft px-3 py-2 text-sm">
          <span className="text-failure">Conveyor could not read the environments. {errorMessage(active.error)}</span>
          <Button size="sm" variant="outline" onClick={() => void active.refetch()}>
            Try again
          </Button>
        </div>
      )}
      {active.isSuccess && active.data.length === 0 && (
        <Card data-environments-empty>
          <CardContent className="py-6 text-center text-sm">
            <p className="font-medium">This workspace has no environments.</p>
            <p className="mx-auto mt-1 max-w-md text-muted">
              {canManage
                ? 'Add an environment for each place where your software runs. Then a watcher or GitHub can tell Conveyor which version runs there.'
                : 'A workspace manager can add an environment for each place where your software runs.'}
            </p>
          </CardContent>
        </Card>
      )}
      {active.isSuccess &&
        active.data.map((environment) => (
          <EnvironmentCard
            key={environment.id}
            environment={environment}
            canManage={canManage}
            onEdit={() => setForm({ mode: 'edit', environment })}
            onArchive={() => setArchived.mutate({ environment, archived: true })}
            onIssueKey={() => setKeyDialog({ kind: 'issue', environment })}
            onRevokeKey={(key) => setKeyDialog({ kind: 'revoke', environment, key })}
            pending={setArchived.isPending && setArchived.variables?.environment.id === environment.id}
            error={archiveError(environment)}
          />
        ))}

      {canManage && (suggestions.data?.length ?? 0) > 0 && (
        <Suggestions
          suggestions={suggestions.data ?? []}
          ignoring={ignore.isPending ? ignore.variables?.github_environment : undefined}
          error={ignore.error}
          onCreate={(suggestion) =>
            setForm({
              mode: 'create',
              seed: { name: suggestion.github_environment, githubEnvironment: suggestion.github_environment },
            })
          }
          onIgnore={(suggestion) => ignore.mutate(suggestion)}
        />
      )}
      {canManage && suggestions.isError && (
        <p role="alert" className="text-sm text-failure">
          Conveyor could not read the suggested environments. {errorMessage(suggestions.error)}
        </p>
      )}

      <div className="pt-1">
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={showArchived}
          onClick={() => setShowArchived((shown) => !shown)}
        >
          {showArchived ? 'Hide archived environments' : 'Show archived environments'}
        </Button>
      </div>
      {showArchived && (
        <section aria-label="Archived environments" className="space-y-3">
          {archived.isPending && <p className="text-sm text-muted">Loading archived environments…</p>}
          {archived.isError && (
            <p role="alert" className="text-sm text-failure">
              Conveyor could not read the archived environments. {errorMessage(archived.error)}
            </p>
          )}
          {archived.isSuccess && archived.data.length === 0 && (
            <p className="text-sm text-muted">This workspace has no archived environments.</p>
          )}
          {archived.isSuccess &&
            archived.data.map((environment) => (
              <EnvironmentCard
                key={environment.id}
                environment={environment}
                canManage={canManage}
                onRestore={() => setArchived.mutate({ environment, archived: false })}
                pending={setArchived.isPending && setArchived.variables?.environment.id === environment.id}
                error={archiveError(environment)}
              />
            ))}
        </section>
      )}

      {form && (
        <EnvironmentFormDialog
          workspace={workspace}
          repositories={repositories}
          environment={form.mode === 'edit' ? form.environment : undefined}
          seed={form.mode === 'create' ? form.seed : undefined}
          onClose={() => setForm(null)}
        />
      )}
      {keyDialog?.kind === 'issue' && (
        <IssueKeyDialog workspace={workspace} environment={keyDialog.environment} onClose={() => setKeyDialog(null)} />
      )}
      {keyDialog?.kind === 'revoke' && (
        <RevokeKeyDialog
          workspace={workspace}
          environment={keyDialog.environment}
          environmentKey={keyDialog.key}
          onClose={() => setKeyDialog(null)}
        />
      )}
    </div>
  )
}

function ServedTime({ value }: { value: string }) {
  return (
    <time dateTime={value} title={absoluteTime(value)}>
      {relativeTime(value)}
    </time>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  )
}

function EnvironmentCard({
  environment,
  canManage,
  onEdit,
  onArchive,
  onRestore,
  onIssueKey,
  onRevokeKey,
  pending,
  error,
}: {
  environment: EnvironmentRow
  canManage: boolean
  onEdit?: () => void
  onArchive?: () => void
  onRestore?: () => void
  onIssueKey?: () => void
  onRevokeKey?: (key: EnvironmentKey) => void
  pending: boolean
  error: unknown
}) {
  const branches = Object.entries(environment.tracked_branches ?? {})
  const githubNames = environment.github_environments ?? []
  const keys = environment.keys ?? []
  const atLimit = keys.length >= MAX_KEYS
  const limitId = `environment-${environment.id}-key-limit`
  return (
    <Card data-environment={environment.id} aria-label={`Environment ${environment.name}`} role="group">
      <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
        <h3 className="min-w-0 truncate text-sm font-semibold">{environment.name}</h3>
        {environment.archived && <Badge>Archived</Badge>}
        {canManage && (
          <div className="ml-auto flex flex-wrap gap-1">
            {!environment.archived && onEdit && (
              <Button size="sm" variant="ghost" onClick={onEdit} aria-label={`Edit ${environment.name}`}>
                <Pencil />
                Edit
              </Button>
            )}
            {!environment.archived && onArchive && (
              <Button
                size="sm"
                variant="ghost"
                disabled={pending}
                onClick={onArchive}
                aria-label={`Archive ${environment.name}`}
              >
                <Archive />
                Archive
              </Button>
            )}
            {environment.archived && onRestore && (
              <Button
                size="sm"
                variant="ghost"
                disabled={pending}
                onClick={onRestore}
                aria-label={`Restore ${environment.name}`}
              >
                <ArchiveRestore />
                Restore
              </Button>
            )}
          </div>
        )}
      </div>
      <CardContent className="space-y-4 pt-3">
        {error != null && (
          <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(error)}
          </p>
        )}
        <dl className="grid grid-cols-1 gap-x-4 gap-y-1 text-sm sm:grid-cols-[11rem_minmax(0,1fr)] sm:gap-y-2">
          <Fact label="Tracked branches">
            {branches.length === 0 ? (
              <span className="text-muted">None</span>
            ) : (
              <span className="flex flex-col">
                {branches.map(([repository, branch]) => (
                  <span key={repository}>
                    {repository}: <span className="font-mono text-xs">{branch}</span>
                  </span>
                ))}
              </span>
            )}
          </Fact>
          <Fact label="GitHub environments">
            {githubNames.length === 0 ? <span className="text-muted">None</span> : githubNames.join(', ')}
          </Fact>
          <Fact label="Version rule">
            {environment.version_rule ? (
              <>
                {environment.version_rule.kind === 'tag_template' ? 'Tag template' : 'Commit pattern'}:{' '}
                <span className="font-mono text-xs">{environment.version_rule.text}</span>
              </>
            ) : (
              <span className="text-muted">None</span>
            )}
          </Fact>
          <Fact label="Timing window">
            {environment.timing_window_minutes} {environment.timing_window_minutes === 1 ? 'minute' : 'minutes'}
          </Fact>
          {!environment.archived && (
            <>
              <Fact label="Watcher">
                <WatcherState listening={environment.listening} lastSignalAt={environment.last_signal_at} />
              </Fact>
              <Fact label="Newest unresolved version">
                {environment.newest_unresolved ? (
                  <span data-newest-unresolved>
                    {environment.newest_unresolved.service}:{' '}
                    <span className="font-mono text-xs">
                      {environment.newest_unresolved.version || environment.newest_unresolved.commit}
                    </span>
                    , deployed <ServedTime value={environment.newest_unresolved.deployed_at} />.{' '}
                    <span className="text-muted">Reason: {environment.newest_unresolved.reason}</span>
                  </span>
                ) : (
                  <span className="text-muted">None</span>
                )}
              </Fact>
            </>
          )}
        </dl>

        <section aria-label={`Keys of ${environment.name}`}>
          <div className="flex flex-wrap items-center gap-2">
            <h4 className="text-xs font-medium uppercase tracking-wide text-faint">
              Keys <span className="tabular-nums">({keys.length})</span>
            </h4>
            {canManage && !environment.archived && onIssueKey && (
              <Button
                size="sm"
                variant="outline"
                className="ml-auto"
                disabled={atLimit}
                aria-describedby={atLimit ? limitId : undefined}
                onClick={onIssueKey}
              >
                <KeyRound />
                Issue key
              </Button>
            )}
          </div>
          {canManage && !environment.archived && atLimit && (
            <p id={limitId} className="mt-1 text-xs text-muted">
              An environment can have five keys. Revoke a key before you issue another.
            </p>
          )}
          {keys.length === 0 ? (
            <p className="mt-1 text-sm text-muted">No keys. A watcher needs a key to send signals.</p>
          ) : (
            <ul className="mt-2 divide-y divide-border rounded-md border border-border">
              {keys.map((key) => (
                <li
                  key={key.id}
                  data-environment-key={key.id}
                  className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm"
                >
                  <span className="min-w-0 truncate font-medium">{key.label}</span>
                  <span className="text-xs text-muted">
                    Created by {key.created_by} <ServedTime value={key.created_at} />
                  </span>
                  <span className="text-xs text-muted">
                    {key.last_used_at ? (
                      <>
                        Last used <ServedTime value={key.last_used_at} />
                      </>
                    ) : (
                      'Never used'
                    )}
                  </span>
                  {canManage && onRevokeKey && (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="ml-auto"
                      aria-label={`Revoke key ${key.label}`}
                      onClick={() => onRevokeKey(key)}
                    >
                      Revoke
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </section>
      </CardContent>
    </Card>
  )
}

// Each suggested name with its repository and newest report time, and the two
// controls of req-environments AC-1.6. Accept opens the create form with the
// name and one GitHub environment name filled in, and writes nothing until
// the form is sent.
function Suggestions({
  suggestions,
  ignoring,
  error,
  onCreate,
  onIgnore,
}: {
  suggestions: EnvironmentSuggestion[]
  ignoring: string | undefined
  error: unknown
  onCreate: (suggestion: EnvironmentSuggestion) => void
  onIgnore: (suggestion: EnvironmentSuggestion) => void
}) {
  return (
    <section aria-label="Suggested environments" className="rounded-lg border border-border bg-card">
      <div className="border-b border-border px-4 py-3">
        <h3 className="text-sm font-semibold">Suggested environments</h3>
        <p className="mt-0.5 text-xs text-muted">
          GitHub reported deployments under these names. No environment lists them.
        </p>
      </div>
      {error != null && (
        <p role="alert" className="mx-4 mt-3 rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
          {errorMessage(error)}
        </p>
      )}
      <ul className="divide-y divide-border">
        {suggestions.map((suggestion) => (
          <li
            key={`${suggestion.github_environment}:${suggestion.repository}`}
            data-environment-suggestion={suggestion.github_environment}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-sm"
          >
            <span className="min-w-0 font-medium">{suggestion.github_environment}</span>
            <span className="text-xs text-muted">
              {suggestion.repository} · last report <ServedTime value={suggestion.last_reported_at} />
            </span>
            <span className="ml-auto flex gap-1">
              <Button size="sm" variant="outline" onClick={() => onCreate(suggestion)}>
                <Plus />
                Create environment
              </Button>
              <Button
                size="sm"
                variant="ghost"
                disabled={ignoring === suggestion.github_environment}
                aria-label={`Ignore ${suggestion.github_environment}`}
                onClick={() => onIgnore(suggestion)}
              >
                <X />
                Ignore
              </Button>
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}
