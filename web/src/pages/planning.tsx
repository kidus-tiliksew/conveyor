import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowRight, Bot, CheckCircle2, FileUp, MessageSquarePlus, Paperclip, Send, Square, ToolCase, User } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Input, Select } from '../components/ui/input'
import {
  abandonPlanningSession,
  createPlanningSession,
  fetchPlanningMessages,
  fetchPlanningSessions,
  fetchRequirements,
  fetchWorkspaceConfig,
  streamPlanningMessage,
  uploadArtifact,
} from '../lib/api'
import type { Artifact, PlanningMessage, PlanningMessagePart, PlanningSession } from '../lib/types'

export function PlanningPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const storageKey = `conveyor-planning-session:${workspace}`
  const [selectedId, setSelectedId] = useState(() => localStorage.getItem(storageKey) ?? '')
  const [title, setTitle] = useState('')
  const [model, setModel] = useState('')
  const requirementContext = sessionStorage.getItem('conveyor-planning-requirement') ?? ''
  const { data: requirements } = useQuery({ queryKey: ['requirements', workspace], queryFn: fetchRequirements, enabled: Boolean(workspace) })
  const { data: workspaceConfig } = useQuery({
    queryKey: ['workspace-config', token, workspace],
    queryFn: () => fetchWorkspaceConfig(token),
    enabled: Boolean(token && workspace),
  })
  const { data: sessions, isLoading } = useQuery({
    queryKey: ['planning-sessions', workspace], queryFn: fetchPlanningSessions, enabled: Boolean(workspace),
  })
  useEffect(() => {
    if (!sessions?.length) return
    if (!sessions.some((session) => session.id === selectedId)) setSelectedId(sessions[0].id)
  }, [sessions, selectedId])
  useEffect(() => {
    if (selectedId) localStorage.setItem(storageKey, selectedId)
  }, [selectedId, storageKey])
  useEffect(() => {
    const document = workspaceConfig?.document
    if (!document) return
    const defaultModel = document.execution_settings.control_plane.planning?.model ?? document.execution_settings.control_plane.triage.model
    const allowlist = document.planning_models ?? [defaultModel]
    if (!model || !allowlist.includes(model)) {
      setModel(defaultModel)
    }
  }, [workspaceConfig, model])

  const create = useMutation({
    mutationFn: () => createPlanningSession(token, {
      title: title.trim() || (requirementContext ? 'Plan work' : 'New requirement'),
      requirement_context_id: requirementContext || undefined,
      model: model || undefined,
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
            disabled={!workspaceConfig}
          >
            {(workspaceConfig?.document.planning_models ?? []).map((candidate) => (
              <option key={candidate} value={candidate}>{candidate}</option>
            ))}
          </Select>
          <Button type="submit" disabled={!token || create.isPending}>
            <MessageSquarePlus /> {create.isPending ? 'Starting…' : 'New session'}
          </Button>
        </form>
        {create.error && <p className="basis-full text-xs text-failure">{String(create.error)}</p>}
      </header>
      <div className="flex min-h-0 flex-1">
        <aside className="w-72 shrink-0 overflow-y-auto border-r border-border bg-surface/40">
          <div className="px-4 py-3">
            <p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Sessions</p>
          </div>
          {isLoading && <p className="px-4 text-xs text-muted">Restoring sessions…</p>}
          {sessions?.length === 0 && <p className="px-4 text-xs leading-5 text-muted">No planning sessions yet.</p>}
          <div className="divide-y divide-border">
            {sessions?.map((session) => (
              <button
                key={session.id}
                type="button"
                onClick={() => setSelectedId(session.id)}
                className={`block w-full px-4 py-3 text-left ${selectedId === session.id ? 'bg-primary-soft' : 'hover:bg-surface'}`}
              >
                <strong className="block truncate text-sm font-medium">{session.title || 'Untitled planning session'}</strong>
                <span className="mt-1 flex items-center justify-between gap-2">
                  <Badge variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}>
                    {session.status}
                  </Badge>
                  <time className="text-[10px] text-faint">{relativeDate(session.updated_at)}</time>
                </span>
              </button>
            ))}
          </div>
        </aside>
        <main className="min-w-0 flex-1">
          {selected
            ? <PlanningChat key={selected.id} session={selected} token={token} workspace={workspace} />
            : <div className="grid h-full place-items-center"><p className="text-sm text-muted">Start or select a planning session.</p></div>}
        </main>
      </div>
    </div>
  )
}

function PlanningChat({ session, token, workspace }: { session: PlanningSession; token: string; workspace: string }) {
  const client = useQueryClient()
  const [draft, setDraft] = useState('')
  const [streamed, setStreamed] = useState<PlanningMessagePart[]>([])
  const [attachments, setAttachments] = useState<Artifact[]>([])
  const endRef = useRef<HTMLDivElement>(null)
  const { data: messages } = useQuery({
    queryKey: ['planning-messages', workspace, session.id],
    queryFn: () => fetchPlanningMessages(session.id),
  })
  const send = useMutation({
    mutationFn: async (content: string) => {
      setStreamed([])
      const attachmentNote = attachments.length
        ? `\n\nAttached context: ${attachments.map((item) => `${item.name} (artifact ${item.id})`).join(', ')}.`
        : ''
      await streamPlanningMessage(token, session.id, content + attachmentNote, (part) => {
        setStreamed((current) => [...current, part])
      })
    },
    onSuccess: () => {
      setDraft('')
      setAttachments([])
      setStreamed([])
      void client.invalidateQueries({ queryKey: ['planning-messages', workspace, session.id] })
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
      void client.invalidateQueries({ queryKey: ['requirements', workspace] })
      void client.invalidateQueries({ queryKey: ['tasks', workspace] })
    },
  })
  const upload = useMutation({
    mutationFn: (file: File) => uploadArtifact(token, file, undefined, session.requirement_context_id),
    onSuccess: (artifact) => setAttachments((current) => [...current, artifact]),
  })
  const abandon = useMutation({
    mutationFn: () => abandonPlanningSession(token, session.id),
    onSuccess: () => void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] }),
  })
  useEffect(() => { endRef.current?.scrollIntoView({ block: 'end' }) }, [messages, streamed])
  const optimisticUser: PlanningMessage | undefined = send.isPending
    ? {
        session_id: session.id, seq: Number.MAX_SAFE_INTEGER, role: 'user',
        content: draft, workspace, created_at: new Date().toISOString(),
      }
    : undefined
  const visibleMessages = optimisticUser ? [...(messages ?? []), optimisticUser] : (messages ?? [])
  const liveText = streamed.filter((part) => part.type === 'text-delta')
    .map((part) => String(part.delta ?? part.text ?? '')).join('')
  const toolParts = streamed.filter((part) => part.type.includes('tool') || part.type === 'dynamic-tool')

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
        <Badge variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}>{session.status}</Badge>
        {session.status === 'active' && (
          <Button variant="ghost" size="sm" disabled={!token || abandon.isPending} onClick={() => abandon.mutate()}>
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

      <div className="min-h-0 flex-1 overflow-y-auto px-5 py-5">
        <div className="mx-auto max-w-3xl space-y-4">
          {visibleMessages.length === 0 && (
            <div className="rounded-xl border border-dashed border-edge px-6 py-10 text-center">
              <Bot className="mx-auto size-7 text-primary" />
              <h3 className="mt-3 text-sm font-semibold">State the intent</h3>
              <p className="mx-auto mt-1 max-w-md text-xs leading-5 text-muted">
                Planning can read the requirement corpus, approved specifications, artifacts, and task lineage before drafting.
              </p>
            </div>
          )}
          {visibleMessages.map((message) => <MessageBubble key={`${message.seq}-${message.role}`} message={message} />)}
          {send.isPending && (
            <div className="flex gap-3">
              <span className="grid size-7 shrink-0 place-items-center rounded-full bg-primary-soft text-primary"><Bot className="size-4" /></span>
              <div className="min-w-0 flex-1 space-y-2">
                {toolParts.map((part, index) => <ToolMarker key={`${part.type}-${index}`} part={part} />)}
                {liveText
                  ? <div className="rounded-xl rounded-tl-sm border border-border bg-card px-4 py-3 text-sm leading-6 whitespace-pre-wrap">{liveText}</div>
                  : <p className="animate-pulse py-2 text-xs text-muted">Planning…</p>}
              </div>
            </div>
          )}
          {send.error && <p className="rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">{String(send.error)}</p>}
          <div ref={endRef} />
        </div>
      </div>

      {session.status === 'finalized' && <FinalizedHandoff session={session} />}
      {session.status === 'active' && (
        <form
          className="shrink-0 border-t border-border bg-background px-5 py-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (draft.trim() && token && !send.isPending) send.mutate(draft.trim())
          }}
        >
          <div className="mx-auto max-w-3xl rounded-xl border border-edge bg-card p-2 shadow-sm focus-within:outline-2 focus-within:outline-primary">
            {attachments.length > 0 && (
              <div className="mb-2 flex flex-wrap gap-1.5 px-1">
                {attachments.map((artifact) => <Badge key={artifact.id} variant="mono"><Paperclip /> {artifact.name}</Badge>)}
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
              <Button type="submit" size="sm" disabled={!token || !draft.trim() || send.isPending}>
                <Send /> Send
              </Button>
            </div>
          </div>
          {upload.error && <p className="mx-auto mt-2 max-w-3xl text-xs text-failure">{String(upload.error)}</p>}
          {!token && <p className="mx-auto mt-2 max-w-3xl text-xs text-attention">Set the operator token in Settings to plan.</p>}
        </form>
      )}
    </div>
  )
}

function MessageBubble({ message }: { message: PlanningMessage }) {
  const assistant = message.role === 'assistant'
  const parts = message.parts ?? []
  const text = message.content || parts.filter((part) => part.type === 'text' || part.type === 'text-delta')
    .map((part) => String(part.text ?? part.delta ?? '')).join('')
  const tools = parts.filter((part) => part.type.includes('tool') || part.type === 'dynamic-tool')
  return (
    <div className={`flex gap-3 ${assistant ? '' : 'flex-row-reverse'}`}>
      <span className={`grid size-7 shrink-0 place-items-center rounded-full ${assistant ? 'bg-primary-soft text-primary' : 'bg-raised text-muted'}`}>
        {assistant ? <Bot className="size-4" /> : <User className="size-4" />}
      </span>
      <div className={`max-w-[82%] space-y-2 ${assistant ? '' : 'items-end'}`}>
        {tools.map((part, index) => <ToolMarker key={`${part.type}-${index}`} part={part} />)}
        {text && <div className={`rounded-xl px-4 py-3 text-sm leading-6 whitespace-pre-wrap ${assistant ? 'rounded-tl-sm border border-border bg-card' : 'rounded-tr-sm bg-primary text-primary-foreground'}`}>{text}</div>}
      </div>
    </div>
  )
}

function ToolMarker({ part }: { part: PlanningMessagePart }) {
  const name = String(part.toolName ?? part.type.replaceAll('-', ' '))
  const complete = part.type.includes('output') || part.state === 'output-available'
  return (
    <div className="inline-flex max-w-full items-center gap-2 rounded-full border border-border bg-surface px-2.5 py-1 text-[11px] text-muted">
      {complete ? <CheckCircle2 className="size-3 text-positive" /> : <ToolCase className="size-3 text-primary" />}
      <span className="truncate">{name}</span>
    </div>
  )
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
          <Link to="/requirements" className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline">
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
