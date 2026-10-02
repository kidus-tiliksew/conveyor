import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronRight } from 'lucide-react'
import { useEffect, useId, useState } from 'react'
import {
  fetchVerificationPermissions,
  sendVerificationPermissionRequest,
  VerificationGrantRefusal,
} from '../../lib/api'
import type {
  VerificationActionRequirement,
  VerificationGrantAction,
  VerificationPermissionGrant,
  VerificationPermissionRequest,
  VerificationPermissionSubject,
  VerificationPermissionView,
  VerificationSubjectRef,
  WorkOrder,
} from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Input, Textarea } from '../ui/input'

// Operator grant disclosure (component-web-dashboard VK-WEB-5;
// feature-verification-kit-execution VK-12.1). Opening the task never sends a
// grant: every POST follows an explicit review and Issue grant action, and the
// locked server mutation stays the admission authority.

export function subjectRef(subject: VerificationSubjectRef) {
  return subject.kind === 'kit' ? `kit:${subject.kit_id}/${subject.exercise_id}` : `ordinary:${subject.obligation_id}`
}

function sameSubject(a: VerificationSubjectRef, b: VerificationSubjectRef) {
  return JSON.stringify(a) === JSON.stringify(b)
}

function permissionsKey(workspace: string, orderId: string) {
  return ['verification-permissions', workspace, orderId] as const
}

export function VerificationPermissions({ order }: { order: WorkOrder }) {
  const allowed = useWorkspaceCapability('operate_gates')
  const { workspace } = useWorkspaceSelection()
  const [open, setOpen] = useState(false)
  const id = useId()
  useEffect(() => setOpen(false), [workspace])
  if (!allowed || order.stage !== 'verify') return null
  return (
    <section aria-label="Verification permissions" className="border-t border-border px-4 py-2.5">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => setOpen(!open)}
        className="inline-flex items-center gap-1 text-xs font-medium text-muted hover:text-foreground"
      >
        <ChevronRight className={cn('size-3.5 transition-transform', open && 'rotate-90')} aria-hidden />
        Permissions
      </button>
      {open && (
        <div id={id} className="mt-2">
          <PermissionsPanel key={`${workspace}:${order.id}`} workspace={workspace} orderId={order.id} />
        </div>
      )}
    </section>
  )
}

type Draft = { binding: string; target: string; include: boolean }

function initialDrafts(row: VerificationPermissionSubject, repository: string): Draft[] {
  return row.action_requirements.map((slot) => ({
    // A slot without a declared binding accepts any name; the runner requests
    // the repository name for it. Targets always start unresolved.
    binding: slot.binding || repository,
    target: '',
    include: slot.required,
  }))
}

function actionsFrom(row: VerificationPermissionSubject, drafts: Draft[]) {
  const actions: VerificationGrantAction[] = []
  const unresolved: VerificationActionRequirement[] = []
  row.action_requirements.forEach((slot, index) => {
    const draft = drafts[index]
    if (!draft) return
    const target = draft.target.trim()
    const included = slot.kind === 'operator_interaction' ? draft.include : target !== ''
    if (!included) {
      if (slot.required) unresolved.push(slot)
      return
    }
    actions.push(
      slot.kind === 'operator_interaction'
        ? { kind: slot.kind, binding: draft.binding.trim() }
        : { kind: slot.kind, binding: draft.binding.trim(), target },
    )
  })
  return { actions, unresolved }
}

function PermissionsPanel({ workspace, orderId }: { workspace: string; orderId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: permissionsKey(workspace, orderId),
    queryFn: ({ signal }) => fetchVerificationPermissions(workspace, orderId, {}, signal),
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchInterval: false,
  })
  const view = query.data
  const binding = view ? `${view.context?.id}|${view.context?.work_order_attempt_id}|${view.submitted_head}` : ''
  const [selected, setSelected] = useState<VerificationSubjectRef>()
  const [drafts, setDrafts] = useState<Draft[]>([])
  const [requestKey, setRequestKey] = useState<string>(() => crypto.randomUUID())
  const [reviewing, setReviewing] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<VerificationGrantRefusal | Error>()
  const [receipt, setReceipt] = useState<VerificationPermissionGrant>()
  // A different context, attempt or head invalidates every selection and any
  // pending request built for the previous one.
  useEffect(() => {
    setSelected(undefined)
    setDrafts([])
    setReviewing(false)
    setReceipt(undefined)
    setError(undefined)
    setRequestKey(crypto.randomUUID())
  }, [binding])

  if (query.isPending)
    return (
      <p role="status" className="text-xs text-muted">
        Loading permissions…
      </p>
    )
  if (query.error)
    return (
      <p role="alert" className="text-xs text-attention">
        Permissions could not be loaded: {query.error.message}
      </p>
    )
  if (!view) return null
  const eligible = view.eligibility.state === 'eligible'
  const row = selected ? view.subjects.find((candidate) => sameSubject(candidate.subject, selected)) : undefined
  const repository = view.context?.revisions[0]?.repository ?? ''
  const built = row ? actionsFrom(row, drafts) : undefined
  const request: VerificationPermissionRequest | undefined =
    row && view.context && built
      ? { context_id: view.context.id, request_key: requestKey.trim(), subject: row.subject, actions: built.actions }
      : undefined

  const issue = async () => {
    if (!request) return
    setPending(true)
    setError(undefined)
    try {
      // Refresh the authoritative projection first; the claim may have moved.
      const fresh = await queryClient.fetchQuery({
        queryKey: permissionsKey(workspace, orderId),
        queryFn: ({ signal }) => fetchVerificationPermissions(workspace, orderId, {}, signal),
        staleTime: 0,
      })
      const freshBinding = `${fresh.context?.id}|${fresh.context?.work_order_attempt_id}|${fresh.submitted_head}`
      if (freshBinding !== binding) throw new Error('The verification context changed. Review the new context.')
      if (fresh.eligibility.state !== 'eligible')
        throw new VerificationGrantRefusal(
          fresh.eligibility.reason ?? fresh.eligibility.state,
          fresh.eligibility.state,
          fresh.eligibility.recovery,
        )
      const sent = await sendVerificationPermissionRequest(workspace, orderId, request)
      const readBack = await fetchVerificationPermissions(workspace, orderId, {
        contextId: request.context_id,
        grantId: sent.ID,
      })
      setReceipt(readBack.grants[0])
      setReviewing(false)
      setSelected(undefined)
      setRequestKey(crypto.randomUUID())
    } catch (cause) {
      setError(cause instanceof Error ? cause : new Error(String(cause)))
    } finally {
      setPending(false)
      void queryClient.invalidateQueries({ queryKey: permissionsKey(workspace, orderId) })
    }
  }

  return (
    <div className="space-y-3 text-sm">
      <HeaderFacts view={view} />
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={eligible ? 'positive' : 'default'}>
          {eligible ? 'Grants can be issued' : view.eligibility.state}
        </Badge>
        <Button variant="ghost" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
          Refresh
        </Button>
      </div>
      {!eligible && (
        <p role="status" className="text-xs text-muted">
          {view.eligibility.reason} {view.eligibility.recovery}
        </p>
      )}
      {receipt && <Receipt grant={receipt} />}
      {error && <RefusalText error={error} />}
      <section aria-label="Subjects" className="space-y-2">
        <h4 className="text-xs font-semibold uppercase tracking-[0.08em] text-muted">Subjects</h4>
        {view.subjects.length === 0 && (
          <p className="text-xs text-muted">No selected kit exercises or registered ordinary obligations yet.</p>
        )}
        {view.subjects.map((candidate) => (
          <SubjectRow
            key={JSON.stringify(candidate.subject)}
            row={candidate}
            selected={Boolean(selected && sameSubject(candidate.subject, selected))}
            disabled={!eligible || pending}
            onSelect={() => {
              setSelected(candidate.subject)
              setDrafts(initialDrafts(candidate, repository))
              setReviewing(false)
              setReceipt(undefined)
              setError(undefined)
            }}
          />
        ))}
      </section>
      {row && built && request && (
        <section aria-label="Grant request" className="space-y-2 rounded-md border border-border p-3">
          <h4 className="text-xs font-semibold">Actions for {subjectRef(row.subject)}</h4>
          {row.action_requirements.length === 0 && (
            <p className="text-xs text-muted">
              This subject declares no actions. The grant carries an explicit empty list.
            </p>
          )}
          {row.action_requirements.map((slot, index) => (
            <ActionEditor
              key={`${slot.kind}:${slot.binding}:${slot.path ?? ''}`}
              slot={slot}
              draft={drafts[index]}
              disabled={reviewing || pending}
              onChange={(next) => setDrafts(drafts.map((draft, at) => (at === index ? next : draft)))}
            />
          ))}
          <RequestKeyField value={requestKey} disabled={reviewing || pending} onChange={setRequestKey} />
          {!reviewing ? (
            <Button
              size="sm"
              variant="outline"
              disabled={!eligible || built.unresolved.length > 0 || !requestKey.trim()}
              onClick={() => setReviewing(true)}
            >
              Review request
            </Button>
          ) : (
            <div className="space-y-2">
              <p className="text-xs text-muted">This exact request is sent when you issue the grant.</p>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-surface p-2 font-mono text-[11px]">
                {JSON.stringify(request, null, 2)}
              </pre>
              <div className="flex flex-wrap gap-2">
                <Button size="sm" disabled={!eligible || pending} onClick={() => void issue()}>
                  {pending ? 'Issuing…' : 'Issue grant'}
                </Button>
                <Button size="sm" variant="ghost" disabled={pending} onClick={() => setReviewing(false)}>
                  Edit
                </Button>
              </div>
            </div>
          )}
          {built.unresolved.length > 0 && (
            <p className="text-xs text-muted">
              Resolve every required target before review: {built.unresolved.map((slot) => slot.kind).join(', ')}.
            </p>
          )}
        </section>
      )}
      <section aria-label="Grants" className="space-y-2">
        <h4 className="text-xs font-semibold uppercase tracking-[0.08em] text-muted">Grants</h4>
        {view.grants.length === 0 && <p className="text-xs text-muted">No grants for this context.</p>}
        {view.grants.map((grant) => (
          <GrantRow
            key={grant.id}
            grant={grant}
            workspace={workspace}
            orderId={orderId}
            eligible={eligible}
            onDone={() => void queryClient.invalidateQueries({ queryKey: permissionsKey(workspace, orderId) })}
          />
        ))}
      </section>
    </div>
  )
}

function HeaderFacts({ view }: { view: VerificationPermissionView }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-muted">Head</dt>
      <dd className="break-all font-mono">{view.submitted_head}</dd>
      <dt className="text-muted">Context</dt>
      <dd className="break-all font-mono">{view.context ? view.context.id : 'Not prepared yet'}</dd>
      {view.context?.revisions.map((revision) => (
        <div key={`${revision.repository}:${revision.sha}`} className="contents">
          <dt className="text-muted">Revision</dt>
          <dd className="break-all font-mono">
            {revision.repository} @ {revision.sha}
          </dd>
        </div>
      ))}
      <dt className="text-muted">Order</dt>
      <dd>
        {view.order_state} · attempt <span className="font-mono">{view.work_order_attempt_id || '—'}</span>
      </dd>
      {view.lease_expires_at && (
        <>
          <dt className="text-muted">Lease expires</dt>
          <dd>
            <time dateTime={view.lease_expires_at}>{absoluteTime(view.lease_expires_at)}</time> (renewed while the
            verifier holds the claim)
          </dd>
        </>
      )}
      {view.execution_deadline && (
        <>
          <dt className="text-muted">Deadline</dt>
          <dd>
            <time dateTime={view.execution_deadline}>{absoluteTime(view.execution_deadline)}</time> (fixed)
          </dd>
        </>
      )}
      <dt className="text-muted">Observed</dt>
      <dd>
        <time dateTime={view.observed_at}>{absoluteTime(view.observed_at)}</time>
      </dd>
    </dl>
  )
}

function SubjectRow({
  row,
  selected,
  disabled,
  onSelect,
}: {
  row: VerificationPermissionSubject
  selected: boolean
  disabled: boolean
  onSelect: () => void
}) {
  const digest = row.subject.kind === 'kit' ? row.subject.content_digest : row.subject.contract_digest
  return (
    <div className={cn('rounded-md border border-border p-2.5', selected && 'border-primary')}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs">{subjectRef(row.subject)}</span>
        {row.subject.kit_version && <Badge variant="mono">v{row.subject.kit_version}</Badge>}
        {row.grant_ids.length > 0 && <Badge variant="default">{row.grant_ids.length} granted</Badge>}
        <Button
          size="sm"
          variant="outline"
          className="ml-auto"
          disabled={disabled}
          aria-pressed={selected}
          onClick={onSelect}
        >
          {selected ? 'Selected' : 'Select'}
        </Button>
      </div>
      {row.description && <p className="mt-1 whitespace-pre-wrap break-words text-xs text-muted">{row.description}</p>}
      <div className="mt-1 flex items-start gap-1">
        <code className="break-all font-mono text-[11px] text-muted" title="Exact subject digest">
          {digest}
        </code>
        {digest && <CopyButton value={digest} label="Copy digest" />}
      </div>
      <ul className="mt-1 space-y-0.5 text-xs">
        {row.action_requirements.map((slot) => (
          <li key={`${slot.kind}:${slot.binding}:${slot.path ?? ''}`}>
            {slot.kind} · binding {slot.binding || 'any name'}
            {slot.path ? ` · kit path ${slot.path}` : ''} · {slot.required ? 'required' : 'optional'}
          </li>
        ))}
        {row.inputs.map((input) => (
          <li key={input.name} className="text-muted">
            input {input.name} ({input.type}
            {input.required ? ', required' : ''}
            {input.sensitive ? ', sensitive' : ''})
          </li>
        ))}
      </ul>
    </div>
  )
}

function ActionEditor({
  slot,
  draft,
  disabled,
  onChange,
}: {
  slot: VerificationActionRequirement
  draft?: Draft
  disabled: boolean
  onChange: (next: Draft) => void
}) {
  const id = useId()
  if (!draft) return null
  const interaction = slot.kind === 'operator_interaction'
  const placeholder =
    slot.kind === 'network'
      ? 'https://host:port'
      : slot.kind === 'credential'
        ? 'Credential handle name, e.g. CONVEYOR_KIT_SECRET_API'
        : 'Absolute root on the executing machine'
  return (
    <fieldset className="grid gap-1.5 rounded border border-border/70 p-2 sm:grid-cols-[10rem_1fr]">
      <legend className="px-1 text-xs font-medium">
        {slot.kind} {slot.required ? '(required)' : '(optional)'}
        {slot.path ? ` · kit path ${slot.path}` : ''}
      </legend>
      <label htmlFor={`${id}-binding`} className="self-center text-xs text-muted">
        Binding
      </label>
      <Input
        id={`${id}-binding`}
        value={draft.binding}
        disabled={disabled || slot.binding !== ''}
        onChange={(event) => onChange({ ...draft, binding: event.target.value })}
      />
      {interaction ? (
        <label className="flex items-center gap-2 text-xs sm:col-span-2">
          <input
            type="checkbox"
            checked={draft.include}
            disabled={disabled}
            onChange={(event) => onChange({ ...draft, include: event.target.checked })}
          />
          Allow operator interaction (no target)
        </label>
      ) : (
        <>
          <label htmlFor={`${id}-target`} className="self-center text-xs text-muted">
            Target
          </label>
          <div className="space-y-1">
            <Input
              id={`${id}-target`}
              value={draft.target}
              placeholder={placeholder}
              autoComplete="off"
              disabled={disabled}
              onChange={(event) => onChange({ ...draft, target: event.target.value })}
            />
            {!draft.target.trim() && <Badge variant="default">Unresolved</Badge>}
          </div>
        </>
      )}
    </fieldset>
  )
}

function RequestKeyField({
  value,
  disabled,
  onChange,
}: {
  value: string
  disabled: boolean
  onChange: (value: string) => void
}) {
  const id = useId()
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-xs text-muted">
        Request key (reuse it to retry an uncertain response)
      </label>
      <Input id={id} value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)} />
    </div>
  )
}

function RefusalText({ error }: { error: Error }) {
  return (
    <div role="alert" className="rounded-md border border-border p-2 text-xs">
      <p className="font-medium text-attention">
        {error instanceof VerificationGrantRefusal && error.reason ? `${error.reason}: ` : ''}
        {error.message}
      </p>
      {error instanceof VerificationGrantRefusal && error.recovery && <p className="text-muted">{error.recovery}</p>}
    </div>
  )
}

function Receipt({ grant }: { grant: VerificationPermissionGrant }) {
  return (
    <div role="status" aria-label="Grant receipt" className="rounded-md border border-positive/40 p-2.5 text-xs">
      <p className="font-medium">Grant issued and read back</p>
      <GrantFacts grant={grant} />
    </div>
  )
}

function GrantFacts({ grant }: { grant: VerificationPermissionGrant }) {
  return (
    <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
      <dt className="text-muted">Grant</dt>
      <dd className="break-all font-mono">{grant.id}</dd>
      <dt className="text-muted">Subject</dt>
      <dd className="break-all font-mono">{subjectRef(grant.subject)}</dd>
      <dt className="text-muted">Actor</dt>
      <dd className="break-all">
        {grant.actor} · <time dateTime={grant.created_at}>{absoluteTime(grant.created_at)}</time>
      </dd>
      <dt className="text-muted">Attempt</dt>
      <dd className="break-all font-mono">{grant.work_order_attempt_id}</dd>
      <dt className="text-muted">Actions</dt>
      <dd className="break-all">
        {grant.actions.length === 0
          ? 'none'
          : grant.actions.map((a) => `${a.kind} ${a.binding}${a.target ? ` → ${a.target}` : ''}`).join('; ')}
      </dd>
      {grant.revisions.map((revision) => (
        <div key={`${revision.repository}:${revision.sha}`} className="contents">
          <dt className="text-muted">Revision</dt>
          <dd className="break-all font-mono">
            {revision.repository} @ {revision.sha}
          </dd>
        </div>
      ))}
      {grant.revocation && (
        <>
          <dt className="text-muted">Revoked</dt>
          <dd className="break-words">
            {grant.revocation.actor} · {absoluteTime(grant.revocation.created_at)}: {grant.revocation.reason}
          </dd>
        </>
      )}
    </dl>
  )
}

function GrantRow({
  grant,
  workspace,
  orderId,
  eligible,
  onDone,
}: {
  grant: VerificationPermissionGrant
  workspace: string
  orderId: string
  eligible: boolean
  onDone: () => void
}) {
  const id = useId()
  const [revoking, setRevoking] = useState(false)
  const [reason, setReason] = useState('')
  const [key] = useState(() => crypto.randomUUID())
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error>()
  const revoke = async () => {
    setPending(true)
    setError(undefined)
    try {
      await sendVerificationPermissionRequest(workspace, orderId, {
        context_id: grant.context_id,
        request_key: key,
        revoke_grant_id: grant.id,
        reason: reason.trim(),
      })
      setRevoking(false)
    } catch (cause) {
      setError(cause instanceof Error ? cause : new Error(String(cause)))
    } finally {
      setPending(false)
      onDone()
    }
  }
  return (
    <div className="rounded-md border border-border p-2.5 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={grant.revocation ? 'default' : 'positive'}>{grant.revocation ? 'Revoked' : 'Active'}</Badge>
        {!grant.revocation && (
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            aria-expanded={revoking}
            aria-controls={id}
            disabled={!eligible}
            onClick={() => setRevoking(!revoking)}
          >
            Revoke
          </Button>
        )}
      </div>
      <GrantFacts grant={grant} />
      {revoking && (
        <div id={id} className="mt-2 space-y-1.5">
          <label htmlFor={`${id}-reason`} className="block text-xs text-muted">
            Revocation reason
          </label>
          <Textarea
            id={`${id}-reason`}
            value={reason}
            disabled={pending}
            onChange={(event) => setReason(event.target.value)}
          />
          <Button size="sm" variant="destructive" disabled={pending || !reason.trim()} onClick={() => void revoke()}>
            {pending ? 'Revoking…' : 'Confirm revocation'}
          </Button>
        </div>
      )}
      {error && <RefusalText error={error} />}
    </div>
  )
}
