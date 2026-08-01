import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowRight, Bot, CheckCircle2, FileUp, MessageSquarePlus, Send, Square, User } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Attachment, Bubble, Marker, Message, MessageScroller } from '../components/ui/chat'
import { Dialog } from '../components/ui/dialog'
import { Input, Select } from '../components/ui/input'
import {
  abandonPlanningSession,
  createPlanningSession,
  fetchPlanningMessages,
  fetchPlanningSession,
  fetchPlanningSessions,
  fetchRequirements,
  fetchWorkspaceConfig,
  streamPlanningMessage,
  uploadArtifact,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type { Artifact, PlanningMessage, PlanningMessagePart, PlanningSession } from '../lib/types'

const sessionStatusLabels: Record<PlanningSession['status'], string> = {
  active: 'Active', finalized: 'Finalized', abandoned: 'Abandoned',
}

export function PlanningPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [selectedId, setSelectedId] = useState('')
  const restoredWorkspace = useRef('')
  const [title, setTitle] = useState('')
  const [model, setModel] = useState('')
  const requirementContext = sessionStorage.getItem('conveyor-planning-requirement') ?? ''
  const { data: requirements } = useQuery({ queryKey: ['requirements', workspace], queryFn: fetchRequirements, enabled: Boolean(workspace) })
  const { data: workspaceConfig } = useQuery({
    queryKey: ['workspace-config', token, workspace],
    queryFn: () => fetchWorkspaceConfig(token),
    enabled: Boolean(token && workspace),
  })
  const { data: sessions, isLoading, error: sessionsError } = useQuery({
    queryKey: ['planning-sessions', workspace], queryFn: fetchPlanningSessions, enabled: Boolean(workspace),
  })

  useEffect(() => {
    if (!workspace) return
    restoredWorkspace.current = workspace
    setSelectedId(localStorage.getItem(`conveyor-planning-session:${workspace}`) ?? '')
  }, [workspace])
  useEffect(() => {
    if (!sessions?.length || restoredWorkspace.current !== workspace) return
    if (!sessions.some((session) => session.id === selectedId)) setSelectedId(sessions[0].id)
  }, [sessions, selectedId, workspace])
  useEffect(() => {
    if (workspace && selectedId && restoredWorkspace.current === workspace) {
      localStorage.setItem(`conveyor-planning-session:${workspace}`, selectedId)
    }
  }, [selectedId, workspace])

  const configuredModels = workspaceConfig?.document.planning_models ?? []
  const defaultModel = workspaceConfig?.document.execution_settings.control_plane.planning?.model
    ?? workspaceConfig?.document.execution_settings.control_plane.triage.model
    ?? ''
  const modelOptions = configuredModels.length ? configuredModels : (defaultModel ? [defaultModel] : [])
  useEffect(() => {
    if (!workspaceConfig) return
    const next = configuredModels.includes(model) ? model : (configuredModels[0] ?? defaultModel)
    if (next !== model) setModel(next)
  }, [configuredModels, defaultModel, model, workspaceConfig])

  const create = useMutation({
    mutationFn: () => createPlanningSession(token, {
      title: title.trim() || (requirementContext ? 'Plan work' : 'New requirement'),
      requirement_context_id: requirementContext || undefined,
      model: configuredModels.length ? model || undefined : undefined,
    }),
    onSuccess: (session) => {
      sessionStorage.removeItem('conveyor-planning-requirement')
      setTitle('')
      setSelectedId(session.id)
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  const selected = sessions?.find((session) => session.id === selectedId)
  const contextTitle = requirements?.find((item) => item.requirement.id === requirementContext)?.requirement.title

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-b border-border px-6 py-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">Planning</h1>
          <p className="mt-0.5 text-xs text-muted">Turn a conversation into a requirement or a blueprint.</p>
        </div>
        <form className="flex flex-wrap items-center gap-2" onSubmit={(event) => { event.preventDefault(); if (token) create.mutate() }}>
          <Input
            aria-label="Planning session title"
            className="w-56"
            placeholder={contextTitle ? `Plan work for ${contextTitle}` : 'What are we planning?'}
            value={title}
            onChange={(event) => setTitle(event.target.value)}
          />
          <Select
            aria-label="Planning model"
            className="w-52 font-mono"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            disabled={!workspaceConfig || configuredModels.length === 0}
          >
            {modelOptions.map((candidate) => <option key={candidate} value={candidate}>{candidate}</option>)}
          </Select>
          <Button type="submit" disabled={!token || create.isPending}>
            <MessageSquarePlus /> {create.isPending ? 'Starting…' : 'New session'}
          </Button>
        </form>
        {create.error && <p className="basis-full text-xs text-failure">{errorMessage(create.error, 'Could not start this planning session.')}</p>}
      </header>
      <div className="flex min-h-0 flex-1">
        <aside className="w-72 shrink-0 overflow-y-auto border-r border-border bg-surface/40">
          <div className="px-4 py-3"><p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Sessions</p></div>
          {isLoading && <p className="px-4 text-xs text-muted">Restoring sessions…</p>}
          {sessionsError && <p className="px-4 text-xs text-failure">{errorMessage(sessionsError, 'Could not restore planning sessions.')}</p>}
          {sessions?.length === 0 && <p className="px-4 text-xs leading-5 text-muted">No planning sessions yet.</p>}
          <div className="divide-y divide-border">
            {sessions?.map((session) => (
              <button
                key={session.id}
                type="button"
                aria-current={selectedId === session.id ? 'true' : undefined}
                onClick={() => setSelectedId(session.id)}
                className={`block w-full px-4 py-3 text-left ${selectedId === session.id ? 'bg-primary-soft' : 'hover:bg-surface'}`}
              >
                <strong className="block truncate text-sm font-medium">{session.title || 'Untitled planning session'}</strong>
                <span className="mt-1 flex items-center justify-between gap-2">
                  <Badge variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}>
                    {sessionStatusLabels[session.status]}
                  </Badge>
                  <time className="text-[10px] text-faint">{relativeDate(session.updated_at)}</time>
                </span>
              </button>
            ))}
          </div>
        </aside>
        <main className="min-w-0 flex-1">
          {selected
            ? <PlanningChat key={`${workspace}:${selected.id}`} summary={selected} token={token} workspace={workspace} />
            : <div className="grid h-full place-items-center"><p className="text-sm text-muted">Start or select a planning session.</p></div>}
        </main>
      </div>
    </div>
  )
}

function PlanningChat({ summary, token, workspace }: { summary: PlanningSession; token: string; workspace: string }) {
  const client = useQueryClient()
  const [draft, setDraft] = useState('')
  const [streamed, setStreamed] = useState<PlanningMessagePart[]>([])
  const [attachments, setAttachments] = useState<Artifact[]>([])
  const [showAbandon, setShowAbandon] = useState(false)
  const [abandonReason, setAbandonReason] = useState('')
  const scrollRef = useRef<HTMLDivElement>(null)
  const endRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true)
  const controller = useRef<AbortController | null>(null)
  const { data: session = summary } = useQuery({
    queryKey: ['planning-session', workspace, summary.id],
    queryFn: () => fetchPlanningSession(summary.id),
    initialData: summary,
  })
  const { data: messages } = useQuery({
    queryKey: ['planning-messages', workspace, session.id],
    queryFn: () => fetchPlanningMessages(session.id),
  })
  const send = useMutation({
    mutationFn: async (content: string) => {
      controller.current?.abort()
      controller.current = new AbortController()
      setStreamed([])
      await streamPlanningMessage(token, session.id, content, (part) => {
        setStreamed((current) => [...current, part])
      }, { signal: controller.current.signal, attachments })
    },
    onSuccess: () => {
      setDraft('')
      setAttachments([])
      setStreamed([])
      void client.invalidateQueries({ queryKey: ['planning-messages', workspace, session.id] })
      void client.invalidateQueries({ queryKey: ['planning-session', workspace, session.id] })
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
      void client.invalidateQueries({ queryKey: ['requirements', workspace] })
      void client.invalidateQueries({ queryKey: ['tasks', workspace] })
    },
    onError: (error) => {
      if (error instanceof DOMException && error.name === 'AbortError') return
      setStreamed((current) => failPendingMarkers(current, errorMessage(error)))
    },
  })
  const upload = useMutation({
    // The durable user-message file part is the session ownership edge. A
    // requirement context is also attached when one exists.
    mutationFn: (file: File) => uploadArtifact(token, file, undefined, session.requirement_context_id),
    onSuccess: (artifact) => setAttachments((current) => [...current, artifact]),
  })
  const abandon = useMutation({
    mutationFn: () => abandonPlanningSession(token, session.id, abandonReason),
    onSuccess: () => {
      setShowAbandon(false)
      void client.invalidateQueries({ queryKey: ['planning-session', workspace, session.id] })
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  useEffect(() => () => {
    controller.current?.abort()
    controller.current = null
  }, [session.id, workspace])
  useEffect(() => {
    if (stickToBottom.current) endRef.current?.scrollIntoView({ block: 'end' })
  }, [messages, streamed])

  const optimisticUser: PlanningMessage | undefined = send.isPending
    ? {
        session_id: session.id, seq: Number.MAX_SAFE_INTEGER, role: 'user',
        content: send.variables ?? draft,
        parts: [
          { type: 'text', text: send.variables ?? draft },
          ...attachments.map((artifact) => ({ type: 'file', artifactId: artifact.id, filename: artifact.name, mediaType: artifact.content_type })),
        ],
        workspace, created_at: new Date().toISOString(),
      }
    : undefined
  const visibleMessages = optimisticUser ? [...(messages ?? []), optimisticUser] : (messages ?? [])
  const groups = useMemo(() => groupMessages(visibleMessages), [visibleMessages])
  const showLiveReply = send.isPending || streamed.length > 0 || Boolean(send.error)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 items-center gap-3 border-b border-border px-5 py-3">
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-semibold">{session.title || 'Untitled planning session'}</h2>
          <p className="mt-0.5 truncate font-mono text-[10px] text-faint">{session.id}</p>
        </div>
        {session.requirement_context_id && <Badge variant="accent">Requirement context</Badge>}
        {session.model && <Badge variant="mono">{session.model}{session.effort ? ` · ${session.effort}` : ''}</Badge>}
        {session.exploration_output_tokens && <Badge variant="mono">{session.exploration_output_tokens.toLocaleString()} tokens/call</Badge>}
        <Badge variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}>{sessionStatusLabels[session.status]}</Badge>
        {session.status === 'active' && (
          <Button variant="ghost" size="sm" disabled={!token || abandon.isPending} onClick={() => setShowAbandon(true)}>
            <Square /> Abandon
          </Button>
        )}
      </div>
      {session.pinned_revisions && Object.keys(session.pinned_revisions).length > 0 && (
        <div className="flex shrink-0 flex-wrap gap-2 border-b border-border bg-surface/50 px-5 py-2">
          {Object.entries(session.pinned_revisions).sort(([left], [right]) => left.localeCompare(right)).map(([repo, revision]) => (
            <Badge key={repo} variant="mono">{repo}@{revision.slice(0, 12)}</Badge>
          ))}
        </div>
      )}

      <MessageScroller
        ref={scrollRef}
        role="log"
        aria-label="Planning conversation"
        aria-live="polite"
        aria-relevant="additions text"
        className="px-5 py-5"
        onScroll={(event) => {
          const element = event.currentTarget
          stickToBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight < 48
        }}
      >
        <div className="mx-auto max-w-3xl space-y-4">
          {groups.length === 0 && (
            <div className="rounded-xl border border-dashed border-edge px-6 py-10 text-center">
              <Bot className="mx-auto size-7 text-primary" />
              <h3 className="mt-3 text-sm font-semibold">State the intent</h3>
              <p className="mx-auto mt-1 max-w-md text-xs leading-5 text-muted">Planning can read the requirement corpus, approved specifications, artifacts, and task lineage before drafting.</p>
            </div>
          )}
          {groups.map((group) => <ConversationMessage key={group.key} group={group} />)}
          {showLiveReply && (
            <Message from="assistant">
              <Avatar role="assistant" />
              <div className="min-w-0 flex-1 space-y-2">
                <RenderedParts parts={streamed} fallback={send.isPending && streamed.length === 0 ? 'Planning…' : ''} />
                {send.error && !(send.error instanceof DOMException && send.error.name === 'AbortError') && (
                  <p className="rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">{errorMessage(send.error, 'Planning stopped before the reply finished. You can retry.')}</p>
                )}
              </div>
            </Message>
          )}
          <div ref={endRef} />
        </div>
      </MessageScroller>

      {session.status === 'finalized' && <FinalizedHandoff session={session} />}
      {session.status === 'active' && (
        <form
          className="shrink-0 border-t border-border bg-background px-5 py-4"
          onSubmit={(event) => { event.preventDefault(); if (draft.trim() && token && !send.isPending) send.mutate(draft.trim()) }}
        >
          <div className="mx-auto max-w-3xl rounded-xl border border-edge bg-card p-2 shadow-sm focus-within:outline-2 focus-within:outline-primary">
            {attachments.length > 0 && (
              <div className="mb-2 flex flex-wrap gap-1.5 px-1">
                {attachments.map((artifact) => <Attachment key={artifact.id} name={artifact.name} contentType={artifact.content_type} />)}
              </div>
            )}
            <textarea
              aria-label="Planning message"
              rows={3}
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter' && !event.shiftKey) {
                  event.preventDefault()
                  if (draft.trim() && token && !send.isPending) send.mutate(draft.trim())
                }
              }}
              placeholder="Describe the outcome, constraints, and why it matters…"
              className="w-full resize-none bg-transparent px-2 py-1 text-sm leading-6 outline-none placeholder:text-faint"
            />
            <div className="flex items-center justify-between gap-3">
              <label className={`inline-flex cursor-pointer items-center gap-1.5 rounded-md px-2 py-1.5 text-xs text-muted hover:bg-surface ${!token ? 'pointer-events-none opacity-40' : ''}`}>
                <FileUp className="size-4" /> {upload.isPending ? 'Uploading…' : 'Attach'}
                <input className="hidden" type="file" disabled={!token || upload.isPending} onChange={(event) => {
                  const file = event.target.files?.[0]
                  if (file) upload.mutate(file)
                  event.currentTarget.value = ''
                }} />
              </label>
              <Button type="submit" size="sm" disabled={!token || !draft.trim() || send.isPending}><Send /> Send</Button>
            </div>
          </div>
          {upload.error && <p className="mx-auto mt-2 max-w-3xl text-xs text-failure">{errorMessage(upload.error, 'Could not attach that file.')}</p>}
          {!token && <p className="mx-auto mt-2 max-w-3xl text-xs text-attention">Set the operator token in Settings to plan.</p>}
        </form>
      )}

      {showAbandon && (
        <Dialog label="Abandon planning session" onClose={() => { if (!abandon.isPending) setShowAbandon(false) }}>
          <div className="space-y-4 p-5">
            <div>
              <h3 className="text-base font-semibold">Abandon this planning session?</h3>
              <p className="mt-1 text-sm leading-6 text-muted">The conversation remains in history, but it cannot receive more replies or produce an artifact.</p>
            </div>
            <Input aria-label="Reason for abandoning" placeholder="Reason (optional)" value={abandonReason} onChange={(event) => setAbandonReason(event.target.value)} />
            {abandon.error && <p className="text-xs text-failure">{errorMessage(abandon.error, 'Could not abandon this session.')}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" disabled={abandon.isPending} onClick={() => setShowAbandon(false)}>Keep session</Button>
              <Button variant="destructive" disabled={!token || abandon.isPending} onClick={() => abandon.mutate()}>{abandon.isPending ? 'Abandoning…' : 'Abandon session'}</Button>
            </div>
          </div>
        </Dialog>
      )}
    </div>
  )
}

type MessageGroup = { key: string; role: 'assistant' | 'user' | 'system'; content: string; parts: PlanningMessagePart[] }

function groupMessages(messages: PlanningMessage[]) {
  const groups: MessageGroup[] = []
  for (const message of messages) {
    const parts = message.parts ?? []
    if (message.role === 'tool' && groups.at(-1)?.role === 'assistant') {
      groups.at(-1)!.parts.push(...parts)
      continue
    }
    groups.push({
      key: `${message.seq}-${message.role}`,
      role: message.role === 'tool' ? 'assistant' : message.role,
      content: message.content,
      parts: [...parts],
    })
  }
  return groups
}

function ConversationMessage({ group }: { group: MessageGroup }) {
  return (
    <Message from={group.role}>
      {group.role !== 'system' && <Avatar role={group.role} />}
      <div className={group.role === 'system' ? 'contents' : 'min-w-0 max-w-[82%] space-y-2'}>
        <RenderedParts parts={group.parts} fallback={group.content} role={group.role} />
      </div>
    </Message>
  )
}

function Avatar({ role }: { role: 'assistant' | 'user' }) {
  return (
    <span className={`grid size-7 shrink-0 place-items-center rounded-full ${role === 'assistant' ? 'bg-primary-soft text-primary' : 'bg-raised text-muted'}`}>
      {role === 'assistant' ? <Bot className="size-4" /> : <User className="size-4" />}
    </span>
  )
}

type DisplayPart =
  | { kind: 'text'; text: string }
  | { kind: 'file'; name: string; contentType?: string }
  | { kind: 'tool'; id: string; name: string; state: 'pending' | 'complete' | 'failed' }

function normalizeParts(parts: PlanningMessagePart[], fallback: string): DisplayPart[] {
  const output: DisplayPart[] = []
  const tools = new Map<string, number>()
  const appendText = (text: string) => {
    if (!text) return
    const last = output.at(-1)
    if (last?.kind === 'text') last.text += text
    else output.push({ kind: 'text', text })
  }
  for (const part of parts) {
    if (part.type === 'text' || part.type === 'text-delta') {
      appendText(String(part.text ?? part.delta ?? ''))
      continue
    }
    if (part.type === 'file' || part.type === 'attachment') {
      output.push({ kind: 'file', name: String(part.filename ?? part.name ?? 'Attachment'), contentType: String(part.mediaType ?? part.contentType ?? '') || undefined })
      continue
    }
    if (!part.type.includes('tool') && part.type !== 'dynamic-tool') continue
    const id = String(part.toolCallId ?? `tool-${output.length}`)
    const previousIndex = tools.get(id)
    const complete = part.type.includes('output') || part.state === 'output-available'
    const failed = part.type.includes('error') || part.state === 'output-error'
    if (previousIndex != null) {
      const previous = output[previousIndex]
      if (previous.kind === 'tool') {
        previous.state = failed ? 'failed' : complete ? 'complete' : previous.state
        if (part.toolName) previous.name = String(part.toolName)
      }
    } else {
      tools.set(id, output.length)
      output.push({ kind: 'tool', id, name: String(part.toolName ?? 'Planning tool'), state: failed ? 'failed' : complete ? 'complete' : 'pending' })
    }
  }
  if (!output.some((part) => part.kind === 'text') && fallback) output.unshift({ kind: 'text', text: fallback })
  return output
}

function RenderedParts({ parts, fallback, role = 'assistant' }: { parts: PlanningMessagePart[]; fallback: string; role?: MessageGroup['role'] }) {
  const display = normalizeParts(parts, fallback)
  return <>{display.map((part, index) => {
    if (part.kind === 'text') return <Bubble key={`text-${index}`} from={role}>{part.text}</Bubble>
    if (part.kind === 'file') return <Attachment key={`file-${index}`} name={part.name} contentType={part.contentType} />
    return <Marker key={part.id} name={part.name} state={part.state} />
  })}</>
}

function failPendingMarkers(parts: PlanningMessagePart[], message: string) {
  const completed = new Set(parts.filter((part) => part.type.includes('output')).map((part) => String(part.toolCallId ?? '')))
  const failures = parts
    .filter((part) => (part.type.includes('input') || part.type === 'dynamic-tool') && !completed.has(String(part.toolCallId ?? '')))
    .map((part) => ({ type: 'tool-output-error', toolCallId: part.toolCallId, toolName: part.toolName, state: 'output-error', errorText: message }))
  return [...parts, ...failures]
}

function FinalizedHandoff({ session }: { session: PlanningSession }) {
  return (
    <div className="shrink-0 border-t border-border bg-positive-soft px-5 py-4">
      <div className="mx-auto flex max-w-3xl flex-wrap items-center gap-3">
        <CheckCircle2 className="size-5 text-positive" />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-positive">Planning artifact finalized</p>
          <p className="text-xs text-muted">The conversation is archived in lineage; authority remains with the ordinary confirmation or spec gate.</p>
        </div>
        {session.produced_requirement_id && (
          <Link to="/requirements" search={{ requirement: session.produced_requirement_id }} className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline">
            Review requirement <ArrowRight className="size-4" />
          </Link>
        )}
        {session.produced_task_id && (
          <Link to="/tasks/$taskId" params={{ taskId: session.produced_task_id }} className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline">
            Open spec gate <ArrowRight className="size-4" />
          </Link>
        )}
      </div>
    </div>
  )
}

function relativeDate(value: string) {
  const date = new Date(value)
  const minutes = Math.max(0, Math.round((Date.now() - date.getTime()) / 60_000))
  if (minutes < 1) return 'now'
  if (minutes < 60) return `${minutes}m`
  if (minutes < 1440) return `${Math.round(minutes / 60)}h`
  return `${Math.round(minutes / 1440)}d`
}
