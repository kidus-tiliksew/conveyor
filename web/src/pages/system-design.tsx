import { useEffect, useMemo } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { Check, MessageCircleQuestion, PenLine, Sparkles } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { PlanningChat } from '../components/planning/planning-chat'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  confirmSystemDesignVersion,
  createPlanningSession,
  fetchDecisions,
  fetchPlanningSession,
  fetchSystemDesigns,
  SystemDesignConflictError,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type { PlanningSession, PlanningSessionGoal, SystemDesignVersion, SystemDesignView } from '../lib/types'

export function SystemDesignPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const client = useQueryClient()
  const search = useSearch({ from: '/system-design' })
  const designs = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
  })
  const decisions = useQuery({
    queryKey: ['decisions', workspace],
    queryFn: fetchDecisions,
    enabled: Boolean(workspace),
  })
  const selected = designs.data?.find((item) => item.document.id === search.document)
  useEffect(() => {
    if (!designs.data?.length || selected) return
    void navigate({
      to: '/system-design',
      search: { document: designs.data[0].document.id, session: search.session },
      replace: true,
    })
  }, [designs.data, navigate, search.session, selected])
  const grouped = useMemo(() => {
    const groups = new Map<string, SystemDesignView[]>()
    for (const item of designs.data ?? []) {
      const list = groups.get(item.document.category) ?? []
      list.push(item)
      groups.set(item.document.category, list)
    }
    return [...groups.entries()]
  }, [designs.data])
  const start = useMutation({
    mutationFn: ({ goal, contextId }: { goal: PlanningSessionGoal; contextId?: string }) =>
      createPlanningSession(token, {
        goal,
        system_design_context_id: contextId,
      }),
    onSuccess: (session, variables) => {
      void navigate({
        to: '/system-design',
        search: { document: variables.contextId ?? selected?.document.id, session: session.id },
        replace: true,
      })
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  const session = useQuery({
    queryKey: ['planning-session', workspace, search.session],
    queryFn: () => fetchPlanningSession(search.session!),
    enabled: Boolean(search.session),
  })
  const adopt = (value: PlanningSession) => {
    void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
    void client.invalidateQueries({ queryKey: ['decisions', workspace] })
    if (value.produced_system_design_id)
      void navigate({
        to: '/system-design',
        search: { document: value.produced_system_design_id, session: value.id },
        replace: true,
      })
  }
  return (
    <div className="flex h-full min-h-0">
      <aside
        className="w-64 shrink-0 overflow-y-auto border-r border-border bg-surface/40 p-3"
        aria-label="System Design documents"
      >
        <div className="mb-3 flex items-center justify-between">
          <div>
            <h1 className="text-base font-semibold">System Design</h1>
            <p className="text-xs text-muted">Confirmed guides for how the system works</p>
          </div>
          <Button
            size="sm"
            variant="secondary"
            disabled={!token || start.isPending}
            onClick={() => start.mutate({ goal: 'system_design' })}
          >
            <Sparkles />
            Draft
          </Button>
        </div>
        {grouped.map(([category, items]) => (
          <section key={category} className="mb-4">
            <h2 className="mb-1 px-2 text-[10px] font-semibold uppercase tracking-wider text-faint">{category}</h2>
            {items.map((item) => (
              <button
                type="button"
                key={item.document.id}
                onClick={() =>
                  void navigate({ to: '/system-design', search: { document: item.document.id }, replace: true })
                }
                className={`mb-1 w-full rounded-md px-2 py-2 text-left text-sm ${selected?.document.id === item.document.id ? 'bg-primary-soft text-primary' : 'hover:bg-raised'}`}
              >
                <span className="block truncate font-medium">{item.document.title}</span>
                <span className="text-[11px] text-faint">
                  v{item.document.current_version || 'pending'}
                  {item.pending_versions.length ? ` · ${item.pending_versions.length} pending` : ''}
                </span>
              </button>
            ))}
          </section>
        ))}
        {!designs.isLoading && !designs.data?.length && (
          <p className="px-2 py-6 text-sm text-muted">No designs yet. Draft one with the assistant.</p>
        )}
      </aside>
      <main className="min-w-0 flex-1 overflow-y-auto px-8 py-6">
        {selected ? (
          <DesignCanvas item={selected} token={token} workspace={workspace} />
        ) : (
          <div className="mx-auto mt-24 max-w-md text-center">
            <h2 className="text-lg font-semibold">Document how this system works</h2>
            <p className="mt-2 text-sm text-muted">
              Use the assistant to draft a reviewable design, then confirm the version you want the team to follow.
            </p>
          </div>
        )}
        <section className="mx-auto mt-8 max-w-4xl border-t border-border pt-5">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-muted">Decision records</h2>
          <div className="mt-3 space-y-2">
            {(decisions.data ?? []).map((decision) => (
              <article key={decision.id} className="rounded-md border border-border p-3">
                <div className="flex gap-2">
                  <Badge variant="mono">{decision.id}</Badge>
                  <Badge>{decision.status}</Badge>
                </div>
                <p className="mt-2 text-sm font-medium">{decision.statement}</p>
                <p className="mt-1 text-xs text-muted">{decision.context}</p>
              </article>
            ))}
          </div>
        </section>
      </main>
      <aside
        className="flex w-[380px] shrink-0 flex-col border-l border-border bg-surface/40"
        aria-label="Design assistant"
      >
        <div className="border-b border-border px-4 py-3">
          <p className="text-[10px] font-semibold uppercase tracking-wider text-faint">Assistant</p>
          <p className="truncate text-xs text-muted">{selected?.document.title ?? 'New System Design'}</p>
        </div>
        <div className="flex gap-2 border-b border-border p-3">
          <Button
            size="sm"
            variant="secondary"
            disabled={!token || !selected || start.isPending}
            onClick={() => start.mutate({ goal: 'system_design', contextId: selected?.document.id })}
          >
            <PenLine />
            Revise
          </Button>
          <Button
            size="sm"
            variant="secondary"
            disabled={!token || !selected || start.isPending}
            onClick={() => start.mutate({ goal: 'open', contextId: selected?.document.id })}
          >
            <MessageCircleQuestion />
            Q&amp;A
          </Button>
        </div>
        {start.error && (
          <p className="p-3 text-xs text-failure">{errorMessage(start.error, 'Could not start planning.')}</p>
        )}
        <div className="flex min-h-0 flex-1 flex-col">
          {session.data ? (
            <PlanningChat
              key={session.data.id}
              summary={session.data}
              token={token}
              workspace={workspace}
              variant="sidebar"
              onFinalized={adopt}
            />
          ) : (
            <div className="p-6 text-center text-sm text-muted">
              Draft, revise, or ask about the open document. Proposed versions land on the canvas for confirmation.
            </div>
          )}
        </div>
      </aside>
    </div>
  )
}

function DesignCanvas({ item, token, workspace }: { item: SystemDesignView; token: string; workspace: string }) {
  const client = useQueryClient()
  const displayed = item.current_version ?? item.pending_versions[0] ?? item.versions[item.versions.length - 1]
  const confirm = useMutation({
    mutationFn: (version: number) =>
      confirmSystemDesignVersion(token, item.document.id, version, item.document.current_version ?? 0),
    onSuccess: () => void client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
    onError: (error) => {
      if (error instanceof SystemDesignConflictError)
        void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
    },
  })
  return (
    <article className="mx-auto max-w-4xl">
      <header className="mb-5 flex items-start justify-between gap-4">
        <div>
          <div className="flex gap-2">
            <span title="The operator-defined group for this design">
              <Badge>{item.document.category}</Badge>
            </span>
            {item.drift.length > 0 && (
              <span title="Governed code changed without a matching design proposal">
                <Badge variant="attention">Design drift · {item.drift.length}</Badge>
              </span>
            )}
          </div>
          <h2 className="mt-2 text-2xl font-semibold tracking-tight">{item.document.title}</h2>
          <p className="mt-1 text-xs text-muted">
            Confirmed versions stay in history. Propose a revision to change governed scope.
          </p>
        </div>
      </header>
      {item.drift.length > 0 && (
        <section
          className="mb-4 rounded-md border border-attention/30 bg-attention-soft/30 p-3"
          aria-label="Design drift"
        >
          <p className="text-sm font-semibold">Governed code changed without a related design proposal.</p>
          <p className="mt-1 text-xs text-muted">
            {[...new Set(item.drift.flatMap((entry) => entry.matching_paths ?? []))].join(', ')}
          </p>
        </section>
      )}
      {confirm.error && (
        <p className="mb-4 text-sm text-failure">{errorMessage(confirm.error, 'Could not confirm this version.')}</p>
      )}
      {item.pending_versions.length > 0 && (
        <section className="mb-5 space-y-4" aria-label="Pending design revisions">
          <h3 className="text-sm font-semibold">Pending revisions</h3>
          {item.pending_versions.map((pending) => (
            <div key={pending.version} className="rounded-lg border border-attention/30 bg-attention-soft/20 p-4">
              <div className="mb-3 flex items-center justify-between gap-4">
                <p className="text-sm font-medium">Version {pending.version}</p>
                <Button disabled={!token || confirm.isPending} onClick={() => confirm.mutate(pending.version)}>
                  <Check />
                  {confirm.isPending && confirm.variables === pending.version
                    ? 'Confirming…'
                    : `Confirm version ${pending.version}`}
                </Button>
              </div>
              {item.current_version && <DesignDiff from={item.current_version} to={pending} />}
            </div>
          ))}
          {item.pending_versions.length > 1 && (
            <p className="text-xs text-muted">Confirming a later revision dismisses any earlier pending revisions.</p>
          )}
        </section>
      )}
      <section className="rounded-lg border border-border bg-background p-6">
        <div className="mb-4 flex flex-wrap gap-2">
          <span title="The immutable revision number">
            <Badge variant="mono">Version {displayed?.version}</Badge>
          </span>
          {displayed?.confirmed ? (
            <Badge variant="positive">Confirmed</Badge>
          ) : (
            <Badge variant="attention">Pending confirmation</Badge>
          )}
          {displayed?.governs.flatMap((scope) =>
            scope.paths.map((path) => (
              <Badge key={`${scope.repository}:${path}`} variant="mono" title="Repository path governed by this design">
                {scope.repository}:{path}
              </Badge>
            )),
          )}
        </div>
        {displayed && <MarkdownProse>{displayed.content}</MarkdownProse>}
      </section>
      <details className="mt-4 rounded-md border border-border p-3">
        <summary className="cursor-pointer text-sm font-medium">Prior versions</summary>
        <ol className="mt-3 space-y-2">
          {item.versions.map((version) => (
            <li key={version.version} className="text-sm">
              <span className="font-mono">v{version.version}</span> ·{' '}
              {version.confirmed ? 'confirmed' : version.dismissed ? 'dismissed' : 'pending'}{' '}
              <details>
                <summary className="cursor-pointer text-xs text-primary">Read version</summary>
                <div className="mt-2 rounded-md bg-surface p-4">
                  <MarkdownProse>{version.content}</MarkdownProse>
                </div>
              </details>
            </li>
          ))}
        </ol>
      </details>
    </article>
  )
}

function DesignDiff({ from, to }: { from: SystemDesignVersion; to: SystemDesignVersion }) {
  return (
    <section
      className="mb-5 rounded-lg border border-attention/30 bg-attention-soft/30 p-4"
      aria-label="Pending version diff"
    >
      <h3 className="text-sm font-semibold">Review the proposed changes</h3>
      <div className="mt-3 grid gap-3 lg:grid-cols-2">
        <DiffSide title={`From version ${from.version}`} content={from.content} />
        <DiffSide title={`To version ${to.version}`} content={to.content} />
      </div>
    </section>
  )
}
function DiffSide({ title, content }: { title: string; content: string }) {
  return (
    <div className="min-w-0 rounded-md border border-border bg-background p-3">
      <p className="mb-2 text-xs font-semibold text-muted">{title}</p>
      <pre className="max-h-72 overflow-auto whitespace-pre-wrap text-xs leading-5">{content}</pre>
    </div>
  )
}
