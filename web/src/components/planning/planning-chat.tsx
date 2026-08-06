import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowRight, Bot, CheckCircle2, FileUp, Send, Square, User } from 'lucide-react'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Attachment, Bubble, Marker, Message, MessageScroller } from '../ui/chat'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'
import {
  abandonPlanningSession,
  fetchArtifacts,
  fetchPlanningMessages,
  fetchPlanningSession,
  streamPlanningMessage,
  uploadArtifact,
} from '../../lib/api'
import { sessionGoalLabel } from '../../lib/contracts'
import { errorMessage } from '../../lib/errors'
import type { Artifact, PlanningMessage, PlanningMessagePart, PlanningSession } from '../../lib/types'

export const sessionStatusLabels: Record<PlanningSession['status'], string> = {
  active: 'Active',
  finalized: 'Finalized',
  abandoned: 'Abandoned',
}

export function PlanningChat({
  summary,
  token,
  workspace,
  variant = 'page',
  onFinalized,
}: {
  summary: PlanningSession
  token: string
  workspace: string
  /** `sidebar` drops the page chrome the document canvas already provides. */
  variant?: 'page' | 'sidebar'
  onFinalized?: (session: PlanningSession) => void
}) {
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
  const sidebar = variant === 'sidebar'
  const gutter = sidebar ? 'px-4' : 'px-5'
  const { data: session = summary } = useQuery({
    queryKey: ['planning-session', workspace, summary.id],
    queryFn: () => fetchPlanningSession(summary.id),
    initialData: summary,
  })
  const { data: messages } = useQuery({
    queryKey: ['planning-messages', workspace, session.id],
    queryFn: () => fetchPlanningMessages(session.id),
  })
  const { data: allArtifacts = [] } = useQuery({
    queryKey: ['artifacts', workspace],
    queryFn: () => fetchArtifacts(token),
    enabled: Boolean(token),
  })
  const sessionArtifacts = (Array.isArray(allArtifacts) ? allArtifacts : [])
    .filter(
      (artifact) =>
        artifact.planning_session_id === session.id ||
        (session.produced_requirement_id && artifact.requirement_id === session.produced_requirement_id) ||
        (session.produced_task_id && artifact.task_id === session.produced_task_id),
    )
    .filter((artifact) => artifact.id !== session.transcript_artifact_id)
  const send = useMutation({
    mutationFn: async (content: string) => {
      controller.current?.abort()
      controller.current = new AbortController()
      setStreamed([])
      await streamPlanningMessage(
        token,
        session.id,
        content,
        (part) => {
          setStreamed((current) => [...current, part])
        },
        { signal: controller.current.signal, attachments },
      )
    },
    onSuccess: () => {
      setDraft('')
      setAttachments([])
      setStreamed([])
      void client.invalidateQueries({
        queryKey: ['planning-messages', workspace, session.id],
      })
      void client.invalidateQueries({
        queryKey: ['planning-session', workspace, session.id],
      })
      void client.invalidateQueries({
        queryKey: ['planning-sessions', workspace],
      })
      void client.invalidateQueries({ queryKey: ['requirements', workspace] })
      void client.invalidateQueries({ queryKey: ['tasks', workspace] })
      void client.invalidateQueries({ queryKey: ['planning-bundles', workspace] })
    },
    onError: (error) => {
      void client.invalidateQueries({ queryKey: ['planning-bundles', workspace] })
      if (error instanceof DOMException && error.name === 'AbortError') return
      setStreamed((current) => failPendingMarkers(current, errorMessage(error)))
    },
  })
  const upload = useMutation({
    mutationFn: (file: File) => uploadArtifact(token, file, undefined, undefined, undefined, session.id),
    onSuccess: (artifact) => setAttachments((current) => [...current, artifact]),
  })
  const abandon = useMutation({
    mutationFn: () => abandonPlanningSession(token, session.id, abandonReason),
    onSuccess: () => {
      setShowAbandon(false)
      void client.invalidateQueries({
        queryKey: ['planning-session', workspace, session.id],
      })
      void client.invalidateQueries({
        queryKey: ['planning-sessions', workspace],
      })
    },
  })
  useEffect(
    () => () => {
      controller.current?.abort()
      controller.current = null
    },
    [session.id, workspace],
  )
  useEffect(() => {
    if (stickToBottom.current) endRef.current?.scrollIntoView({ block: 'end' })
  }, [messages, streamed])
  // A finalized session hands its produced artifact back to the surface that
  // hosts the chat, so the document canvas refreshes without navigating away
  // (spec §21.57 change 1). It reports once per produced artifact.
  const finalizeHandler = useRef(onFinalized)
  useEffect(() => {
    finalizeHandler.current = onFinalized
  }, [onFinalized])
  // Only a finalize observed while this conversation is open is news. Adopting
  // whatever was already produced on mount keeps a deep link to an earlier
  // finalized session from yanking the canvas off the document the URL asked
  // for.
  const reported = useRef<string | null>(null)
  useEffect(() => {
    const produced =
      session.produced_requirement_id ||
      session.produced_system_design_id ||
      session.produced_bundle_id ||
      session.produced_task_id ||
      ''
    if (reported.current === null) {
      reported.current = produced
      return
    }
    if (!produced || reported.current === produced) return
    reported.current = produced
    finalizeHandler.current?.(session)
  }, [session])

  const optimisticUser: PlanningMessage | undefined = send.isPending
    ? {
        session_id: session.id,
        seq: Number.MAX_SAFE_INTEGER,
        role: 'user',
        content: send.variables ?? draft,
        parts: [
          { type: 'text', text: send.variables ?? draft },
          ...attachments.map((artifact) => ({
            type: 'file',
            artifactId: artifact.id,
            filename: artifact.name,
            mediaType: artifact.content_type,
          })),
        ],
        workspace,
        created_at: new Date().toISOString(),
      }
    : undefined
  const visibleMessages = optimisticUser ? [...(messages ?? []), optimisticUser] : (messages ?? [])
  const groups = useMemo(() => groupMessages(visibleMessages), [visibleMessages])
  const showLiveReply = send.isPending || streamed.length > 0 || Boolean(send.error)
  const liveGroups = useMemo(
    () => groupLiveReply(streamed, send.isPending && streamed.length === 0 ? 'Planning…' : ''),
    [send.isPending, streamed],
  )

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className={`flex shrink-0 items-center gap-3 border-b border-border ${gutter} py-3`}>
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-semibold">{session.title || 'Untitled planning session'}</h2>
          {sidebar ? (
            <p className="mt-0.5 text-[11px] text-muted">{sessionGoalLabel(session)}</p>
          ) : (
            <p className="mt-0.5 truncate font-mono text-[10px] text-faint">{session.id}</p>
          )}
        </div>
        {!sidebar && <Badge variant="mono">{sessionGoalLabel(session)}</Badge>}
        {!sidebar && session.requirement_context_id && <Badge variant="accent">Requirement context</Badge>}
        {!sidebar && session.model && (
          <Badge variant="mono">
            {session.model}
            {session.effort ? ` · ${session.effort}` : ''}
          </Badge>
        )}
        {!sidebar && session.exploration_output_tokens && (
          <Badge variant="mono">{session.exploration_output_tokens.toLocaleString()} tokens/call</Badge>
        )}
        <Badge
          variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}
        >
          {sessionStatusLabels[session.status]}
        </Badge>
        {session.status === 'active' && (
          <Button
            variant="ghost"
            size="sm"
            aria-label="Abandon"
            title="Abandon this planning session"
            disabled={!token || abandon.isPending}
            onClick={() => setShowAbandon(true)}
          >
            <Square /> {sidebar ? '' : 'Abandon'}
          </Button>
        )}
      </div>
      {!sidebar && session.pinned_revisions && Object.keys(session.pinned_revisions).length > 0 && (
        <div className="flex shrink-0 flex-wrap gap-2 border-b border-border bg-surface/50 px-5 py-2">
          {Object.entries(session.pinned_revisions)
            .sort(([left], [right]) => left.localeCompare(right))
            .map(([repo, revision]) => (
              <Badge key={repo} variant="mono">
                {repo}@{revision.slice(0, 12)}
              </Badge>
            ))}
        </div>
      )}
      {sessionArtifacts.length > 0 && (
        <section
          className={`flex shrink-0 flex-wrap items-center gap-2 border-b border-border bg-surface/50 ${gutter} py-2`}
          aria-label="Planning attachments"
        >
          <span className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Attachments</span>
          {sessionArtifacts.map((artifact) => (
            <Attachment
              key={`${artifact.id}-${artifact.role}`}
              name={artifact.name}
              contentType={artifact.content_type}
            />
          ))}
        </section>
      )}

      <MessageScroller
        ref={scrollRef}
        role="log"
        aria-label="Planning conversation"
        aria-live="polite"
        aria-relevant="additions text"
        className={`${gutter} py-5`}
        onScroll={(event) => {
          const element = event.currentTarget
          stickToBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight < 48
        }}
      >
        <div className={`space-y-4 ${sidebar ? '' : 'mx-auto max-w-3xl'}`}>
          {groups.length === 0 && (
            <div className="rounded-xl border border-dashed border-edge px-6 py-10 text-center">
              <Bot className="mx-auto size-7 text-primary" />
              <h3 className="mt-3 text-sm font-semibold">State the intent</h3>
              <p className="mx-auto mt-1 max-w-md text-xs leading-5 text-muted">
                Planning can read the requirement corpus, approved execution plans, artifacts, and task lineage before
                drafting.
              </p>
            </div>
          )}
          {groups.map((group) => (
            <ConversationMessage key={group.key} group={group} />
          ))}
          {showLiveReply &&
            liveGroups.map((group) => (
              <Message from={group.role} key={group.key}>
                {group.role === 'assistant' && <Avatar kind="assistant" />}
                <div className={group.role === 'system' ? 'contents' : 'min-w-0 flex-1 space-y-2'}>
                  <RenderedParts parts={group.parts} fallback={group.fallback} role={group.role} />
                </div>
              </Message>
            ))}
          {showLiveReply && send.error && !(send.error instanceof DOMException && send.error.name === 'AbortError') && (
            <Message from="assistant">
              <Avatar kind="assistant" />
              <p className="min-w-0 flex-1 rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
                {errorMessage(send.error, 'Planning stopped before the reply finished. You can retry.')}
              </p>
            </Message>
          )}
          <div ref={endRef} />
        </div>
      </MessageScroller>

      {session.status === 'finalized' && <FinalizedHandoff session={session} gutter={gutter} sidebar={sidebar} />}
      {session.status === 'active' && (
        <form
          className={`shrink-0 border-t border-border bg-background ${gutter} py-4`}
          onSubmit={(event) => {
            event.preventDefault()
            if (draft.trim() && token && !send.isPending) send.mutate(draft.trim())
          }}
        >
          <div
            className={`rounded-xl border border-edge bg-card p-2 shadow-sm focus-within:outline-2 focus-within:outline-primary ${sidebar ? '' : 'mx-auto max-w-3xl'}`}
          >
            {attachments.length > 0 && (
              <div className="mb-2 flex flex-wrap gap-1.5 px-1">
                {attachments.map((artifact) => (
                  <Attachment key={artifact.id} name={artifact.name} contentType={artifact.content_type} />
                ))}
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
              <label
                className={`inline-flex cursor-pointer items-center gap-1.5 rounded-md px-2 py-1.5 text-xs text-muted hover:bg-surface ${!token ? 'pointer-events-none opacity-40' : ''}`}
              >
                <FileUp className="size-4" /> {upload.isPending ? 'Uploading…' : 'Attach'}
                <input
                  className="hidden"
                  type="file"
                  disabled={!token || upload.isPending}
                  onChange={(event) => {
                    const file = event.target.files?.[0]
                    if (file) upload.mutate(file)
                    event.currentTarget.value = ''
                  }}
                />
              </label>
              <Button type="submit" size="sm" disabled={!token || !draft.trim() || send.isPending}>
                <Send /> Send
              </Button>
            </div>
          </div>
          {upload.error && (
            <p className={`mt-2 text-xs text-failure ${sidebar ? '' : 'mx-auto max-w-3xl'}`}>
              {errorMessage(upload.error, 'Could not attach that file.')}
            </p>
          )}
          {!token && (
            <p className={`mt-2 text-xs text-attention ${sidebar ? '' : 'mx-auto max-w-3xl'}`}>
              Set the operator token in Settings to plan.
            </p>
          )}
        </form>
      )}

      {showAbandon && (
        <Dialog
          label="Abandon planning session"
          onClose={() => {
            if (!abandon.isPending) setShowAbandon(false)
          }}
        >
          <div className="space-y-4 p-5">
            <div>
              <h3 className="text-base font-semibold">Abandon this planning session?</h3>
              <p className="mt-1 text-sm leading-6 text-muted">
                The conversation remains in history, but it cannot receive more replies or produce an artifact.
              </p>
            </div>
            <Input
              aria-label="Reason for abandoning"
              placeholder="Reason (optional)"
              value={abandonReason}
              onChange={(event) => setAbandonReason(event.target.value)}
            />
            {abandon.error && (
              <p className="text-xs text-failure">{errorMessage(abandon.error, 'Could not abandon this session.')}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" disabled={abandon.isPending} onClick={() => setShowAbandon(false)}>
                Keep session
              </Button>
              <Button variant="destructive" disabled={!token || abandon.isPending} onClick={() => abandon.mutate()}>
                {abandon.isPending ? 'Abandoning…' : 'Abandon session'}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </div>
  )
}

type MessageGroup = {
  key: string
  role: 'assistant' | 'user' | 'system'
  content: string
  parts: PlanningMessagePart[]
}

function groupMessages(messages: PlanningMessage[]) {
  const groups: MessageGroup[] = []
  for (const message of messages) {
    const parts = message.parts ?? []
    const baseRole = message.role === 'tool' ? 'assistant' : message.role
    if (parts.length === 0 || baseRole === 'user') {
      groups.push({
        key: `${message.seq}-${baseRole}`,
        role: baseRole,
        content: message.content,
        parts: [...parts],
      })
      continue
    }
    let contentAssigned = false
    if (message.content && parts[0]?.type === 'system-correction' && baseRole !== 'system') {
      groups.push({ key: `${message.seq}-${baseRole}-content`, role: baseRole, content: message.content, parts: [] })
      contentAssigned = true
    }
    for (const part of parts) {
      const role = part.type === 'system-correction' ? 'system' : baseRole
      const current = groups.at(-1)
      if (current?.role === role && (message.role === 'tool' || current.key.startsWith(`${message.seq}-`))) {
        current.parts.push(part)
      } else {
        const content: string = role === baseRole && !contentAssigned ? message.content : ''
        groups.push({
          key: `${message.seq}-${role}-${groups.length}`,
          role,
          content,
          parts: [part],
        })
        contentAssigned ||= content !== ''
      }
    }
  }
  return groups
}

type LiveMessageGroup = {
  key: string
  role: 'assistant' | 'system'
  parts: PlanningMessagePart[]
  fallback: string
}

function groupLiveReply(parts: PlanningMessagePart[], fallback: string): LiveMessageGroup[] {
  if (parts.length === 0) return fallback ? [{ key: 'live-assistant-0', role: 'assistant', parts: [], fallback }] : []
  const groups: LiveMessageGroup[] = []
  for (const part of parts) {
    const role = part.type === 'system-correction' ? 'system' : 'assistant'
    const current = groups.at(-1)
    if (current?.role === role) current.parts.push(part)
    else groups.push({ key: `live-${role}-${groups.length}`, role, parts: [part], fallback: '' })
  }
  return groups
}

function ConversationMessage({ group }: { group: MessageGroup }) {
  return (
    <Message from={group.role}>
      {group.role !== 'system' && <Avatar kind={group.role} />}
      <div className={group.role === 'system' ? 'contents' : 'min-w-0 max-w-[82%] space-y-2'}>
        <RenderedParts parts={group.parts} fallback={group.content} role={group.role} />
      </div>
    </Message>
  )
}

function Avatar({ kind }: { kind: 'assistant' | 'user' }) {
  return (
    <span
      className={`grid size-7 shrink-0 place-items-center rounded-full ${kind === 'assistant' ? 'bg-primary-soft text-primary' : 'bg-raised text-muted'}`}
    >
      {kind === 'assistant' ? <Bot className="size-4" /> : <User className="size-4" />}
    </span>
  )
}

type DisplayPart =
  | { kind: 'text'; text: string }
  | { kind: 'file'; name: string; contentType?: string }
  | { kind: 'correction'; text: string; detail: string }
  | {
      kind: 'tool'
      id: string
      name: string
      state: 'pending' | 'complete' | 'corrected' | 'deferred' | 'cancelled' | 'failed'
    }

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
    if (part.type === 'system-correction') {
      output.push({
        kind: 'correction',
        text: String(part.text || 'The assistant response needed correction — retrying.'),
        detail: String(part.detail || ''),
      })
      continue
    }
    if (part.type === 'text' || part.type === 'text-delta') {
      appendText(String(part.text || part.delta || ''))
      continue
    }
    if (part.type === 'file' || part.type === 'attachment') {
      output.push({
        kind: 'file',
        name: String(part.filename || part.name || 'Attachment'),
        contentType: String(part.mediaType || part.contentType || '') || undefined,
      })
      continue
    }
    if (!part.type.includes('tool') && part.type !== 'dynamic-tool') continue
    const id = String(part.toolCallId || `tool-${output.length}`)
    const previousIndex = tools.get(id)
    const complete = part.type.includes('output') || part.state === 'output-available'
    const status =
      typeof part.output === 'object' && part.output
        ? String((part.output as Record<string, unknown>).status || '')
        : ''
    const nextState =
      status === 'invalid' || status === 'corrected'
        ? 'corrected'
        : status === 'deferred'
          ? 'deferred'
          : status === 'cancelled'
            ? 'cancelled'
            : part.type.includes('error') || part.state === 'output-error'
              ? 'failed'
              : complete
                ? 'complete'
                : 'pending'
    if (previousIndex != null) {
      const previous = output[previousIndex]
      if (previous.kind === 'tool') {
        previous.state = nextState === 'pending' ? previous.state : nextState
        if (part.toolName) previous.name = String(part.toolName)
      }
    } else {
      tools.set(id, output.length)
      output.push({
        kind: 'tool',
        id,
        name: String(part.toolName || 'Planning tool'),
        state: nextState,
      })
    }
  }
  if (!output.some((part) => part.kind === 'text' || part.kind === 'correction') && fallback)
    output.unshift({ kind: 'text', text: fallback })
  return output
}

function RenderedParts({
  parts,
  fallback,
  role = 'assistant',
}: {
  parts: PlanningMessagePart[]
  fallback: string
  role?: MessageGroup['role']
}) {
  const display = normalizeParts(parts, fallback)
  return (
    <>
      {display.map((part, index) => {
        if (part.kind === 'text')
          return (
            <Bubble key={`text-${index}`} from={role}>
              {part.text}
            </Bubble>
          )
        if (part.kind === 'file')
          return <Attachment key={`file-${index}`} name={part.name} contentType={part.contentType} />
        if (part.kind === 'correction')
          return (
            <Bubble key={`correction-${index}`} from="system">
              <span>{part.text}</span>
              {part.detail && (
                <details className="mt-1 text-xs">
                  <summary>Technical details</summary>
                  <pre className="mt-1 whitespace-pre-wrap">{part.detail}</pre>
                </details>
              )}
            </Bubble>
          )
        return <Marker key={part.id} name={part.name} state={part.state} />
      })}
    </>
  )
}

function failPendingMarkers(parts: PlanningMessagePart[], message: string) {
  const completed = new Set(
    parts.filter((part) => part.type.includes('output')).map((part) => String(part.toolCallId ?? '')),
  )
  const failures = parts
    .filter(
      (part) =>
        (part.type.includes('input') || part.type === 'dynamic-tool') && !completed.has(String(part.toolCallId ?? '')),
    )
    .map((part) => ({
      type: 'tool-output-error',
      toolCallId: part.toolCallId,
      toolName: part.toolName,
      state: 'output-error',
      errorText: message,
    }))
  return [...parts, ...failures]
}

function FinalizedHandoff({
  session,
  gutter,
  sidebar,
}: {
  session: PlanningSession
  gutter: string
  sidebar: boolean
}) {
  return (
    <div className={`shrink-0 border-t border-border bg-positive-soft ${gutter} py-4`}>
      <div className={`flex flex-wrap items-center gap-3 ${sidebar ? '' : 'mx-auto max-w-3xl'}`}>
        <CheckCircle2 className="size-5 text-positive" />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-positive">Planning artifact finalized</p>
          <p className="text-xs text-muted">
            The conversation is archived in lineage; authority remains with the ordinary confirmation or spec gate.
          </p>
        </div>
        {session.produced_requirement_id && (
          <Link
            to="/requirements"
            search={{ requirement: session.produced_requirement_id }}
            className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
          >
            Review requirement <ArrowRight className="size-4" />
          </Link>
        )}
        {session.produced_task_id && (
          <Link
            to="/tasks/$taskId"
            params={{ taskId: session.produced_task_id }}
            className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
          >
            Open spec gate <ArrowRight className="size-4" />
          </Link>
        )}
      </div>
    </div>
  )
}

export function relativeDate(value: string) {
  const date = new Date(value)
  const minutes = Math.max(0, Math.round((Date.now() - date.getTime()) / 60_000))
  if (minutes < 1) return 'now'
  if (minutes < 60) return `${minutes}m`
  if (minutes < 1440) return `${Math.round(minutes / 60)}h`
  return `${Math.round(minutes / 1440)}d`
}
