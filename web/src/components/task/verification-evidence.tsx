import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useState } from 'react'
import { fetchVerificationArtifact, fetchVerificationEvidence, recordVerificationObservation } from '../../lib/api'
import type { VerificationEvidence } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

const disclosureQuery = {
  retry: false,
  staleTime: Infinity,
  gcTime: 0,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
  refetchInterval: false,
} as const

// One retained evidence envelope, read on demand. Nothing here is fetched
// until a component that shows it mounts (component-web-dashboard).
export function useVerificationEvidence(taskId: string, contextId: string, evidenceId: string) {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['verification-evidence', workspace, taskId, contextId, evidenceId],
    queryFn: ({ signal }) => fetchVerificationEvidence(workspace, taskId, contextId, evidenceId, signal),
    enabled: Boolean(workspace && taskId && contextId && evidenceId),
    ...disclosureQuery,
  })
}

// A retained artifact as an object URL (and its text when it is readable).
// The URL is revoked when the component that asked for it goes away.
export function useVerificationArtifact(
  taskId: string,
  contextId: string,
  evidenceId: string,
  artifactId: string,
  mediaType: string,
) {
  const { workspace } = useWorkspaceSelection()
  const [url, setURL] = useState('')
  const [text, setText] = useState('')
  const query = useQuery({
    queryKey: ['verification-artifact', workspace, taskId, contextId, evidenceId, artifactId],
    queryFn: ({ signal }) => fetchVerificationArtifact(workspace, taskId, contextId, evidenceId, artifactId, signal),
    enabled: Boolean(workspace && taskId && contextId && evidenceId && artifactId),
    ...disclosureQuery,
  })
  useEffect(() => {
    if (!query.data) return
    const objectURL = URL.createObjectURL(query.data)
    let active = true
    setURL(objectURL)
    if (mediaType === 'application/json' || mediaType === 'text/plain')
      void query.data.text().then((value) => {
        if (active) setText(value)
      })
    return () => {
      active = false
      URL.revokeObjectURL(objectURL)
    }
  }, [query.data, mediaType])
  return { url, text, isPending: query.isPending, error: query.error }
}

export function isImageMediaType(mediaType?: string) {
  return Boolean(mediaType?.startsWith('image/'))
}

// The first image artifact an envelope carries, whether it is declared on
// the envelope or only referenced from a visual_capture payload.
export function imageArtifactOf(evidence: VerificationEvidence | undefined) {
  if (!evidence) return undefined
  const envelope = evidence.Envelope
  const declared = (envelope.artifacts ?? []).find((artifact) => isImageMediaType(artifact.media_type))
  if (declared) return declared
  const payload = envelope.payload as { artifact?: { artifact_id?: string }; media_type?: string } | null
  if (payload?.artifact?.artifact_id && isImageMediaType(payload.media_type))
    return { artifact_id: payload.artifact.artifact_id, sha256: '', media_type: payload.media_type ?? '' }
  return undefined
}

export function EvidenceImage({
  taskId,
  contextId,
  evidenceId,
  artifactId,
  mediaType,
  alt,
  className,
}: {
  taskId: string
  contextId: string
  evidenceId: string
  artifactId: string
  mediaType: string
  alt: string
  className?: string
}) {
  const artifact = useVerificationArtifact(taskId, contextId, evidenceId, artifactId, mediaType)
  if (artifact.error)
    return (
      <p role="alert" className="text-xs text-attention">
        Capture could not be loaded: {artifact.error.message}
      </p>
    )
  if (!artifact.url)
    return (
      <span
        role="status"
        aria-label="Loading capture"
        className={cn('block animate-pulse rounded bg-surface', className)}
      />
    )
  return <img src={artifact.url} alt={alt} className={cn('max-w-full object-contain', className)} />
}

export function VerificationEvidenceDisclosure({
  taskId,
  contextId,
  evidenceId,
}: {
  taskId: string
  contextId: string
  evidenceId: string
}) {
  const [open, setOpen] = useState(false)
  return (
    <div className="min-w-0">
      <Button variant="ghost" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        {open ? 'Close evidence' : 'Open evidence'} <span className="max-w-40 truncate font-mono">{evidenceId}</span>
      </Button>
      {open && <EvidenceContent taskId={taskId} contextId={contextId} evidenceId={evidenceId} />}
    </div>
  )
}

function EvidenceContent({ taskId, contextId, evidenceId }: { taskId: string; contextId: string; evidenceId: string }) {
  const query = useVerificationEvidence(taskId, contextId, evidenceId)
  if (query.isPending) return <p role="status">Loading evidence…</p>
  if (query.error) return <p role="alert">Evidence could not be loaded: {query.error.message}</p>
  const evidence = query.data.Envelope
  return (
    <div className="min-w-0 space-y-2 rounded border border-border bg-background p-3 text-xs">
      <p className="break-all">
        {evidence.type} · Captured by {evidence.captured_by.identity} · {evidence.captured_at}
      </p>
      <p className="break-all">
        Attribution: {evidence.captured_by.attribution} · Submitted by {evidence.submitted_by}
      </p>
      <section aria-label="Structured evidence">
        <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-all">
          {JSON.stringify(evidence.payload, null, 2)}
        </pre>
      </section>
      {(evidence.artifacts ?? []).map((artifact) => (
        <ArtifactDisclosure
          key={artifact.artifact_id}
          taskId={taskId}
          contextId={contextId}
          evidenceId={evidenceId}
          artifactId={artifact.artifact_id}
          mediaType={artifact.media_type}
        />
      ))}
    </div>
  )
}

function ArtifactDisclosure(props: {
  taskId: string
  contextId: string
  evidenceId: string
  artifactId: string
  mediaType: string
}) {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <Button variant="outline" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        {open ? 'Close artifact' : 'Open artifact'} ({props.mediaType})
      </Button>
      {open && <ArtifactContent {...props} />}
    </div>
  )
}

function ArtifactContent({
  taskId,
  contextId,
  evidenceId,
  artifactId,
  mediaType,
}: {
  taskId: string
  contextId: string
  evidenceId: string
  artifactId: string
  mediaType: string
}) {
  const { url, text, isPending, error } = useVerificationArtifact(taskId, contextId, evidenceId, artifactId, mediaType)
  if (isPending) return <p role="status">Loading artifact…</p>
  if (error) return <p role="alert">Artifact could not be loaded: {error.message}</p>
  return (
    <div className="space-y-2 py-2">
      {url && mediaType.startsWith('image/') && (
        <img src={url} alt="Verification capture" className="max-h-96 max-w-full object-contain" />
      )}
      {url && mediaType.startsWith('video/') && (
        <video src={url} controls className="max-h-96 max-w-full">
          <track kind="captions" />
        </video>
      )}
      {text && (
        <section aria-label="Artifact content">
          <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-all">{text}</pre>
        </section>
      )}
      {url && (
        <a href={url} download={artifactId} className="text-accent underline">
          Download artifact
        </a>
      )}
    </div>
  )
}

export function VerificationObservationForm({
  taskId,
  contextId,
  runId,
}: {
  taskId: string
  contextId: string
  runId: string
}) {
  const allowed = useWorkspaceCapability('operate_gates')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const id = useId()
  const [open, setOpen] = useState(false)
  const [fact, setFact] = useState('')
  const [supporting, setSupporting] = useState('')
  const [key, setKey] = useState(() => crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () =>
      recordVerificationObservation(workspace, taskId, {
        context_id: contextId,
        run_id: runId,
        idempotency_key: key,
        fact,
        supporting: supporting
          .split(',')
          .map((value) => value.trim())
          .filter(Boolean)
          .map((evidence_id) => ({ evidence_id })),
      }),
    onSuccess: () => {
      setFact('')
      setSupporting('')
      setKey(crypto.randomUUID())
      void queryClient.invalidateQueries({ queryKey: ['verification-summary', workspace, taskId] })
      void queryClient.invalidateQueries({ queryKey: ['verification-page', workspace, taskId, contextId] })
    },
  })
  if (!allowed) return null
  return (
    <div className="space-y-2">
      <Button variant="outline" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        Record observation
      </Button>
      {open && (
        <form
          className="space-y-2"
          onSubmit={(event) => {
            event.preventDefault()
            mutation.mutate()
          }}
        >
          <label htmlFor={`${id}-fact`} className="block text-xs">
            Observed fact
          </label>
          <Textarea
            id={`${id}-fact`}
            value={fact}
            maxLength={16384}
            required
            disabled={mutation.isPending}
            onChange={(event) => {
              setFact(event.target.value)
              mutation.reset()
            }}
          />
          <label htmlFor={`${id}-support`} className="block text-xs">
            Supporting evidence IDs (comma separated)
          </label>
          <input
            id={`${id}-support`}
            className="w-full rounded border border-border bg-background p-2 text-xs"
            value={supporting}
            required
            disabled={mutation.isPending}
            onChange={(event) => setSupporting(event.target.value)}
          />
          <Button size="sm" type="submit" disabled={mutation.isPending || !fact.trim()}>
            {mutation.isPending ? 'Recording…' : 'Save observation'}
          </Button>
          {mutation.error && <p role="alert">{mutation.error.message}</p>}
          {mutation.isSuccess && <p role="status">Observation recorded.</p>}
        </form>
      )}
    </div>
  )
}
