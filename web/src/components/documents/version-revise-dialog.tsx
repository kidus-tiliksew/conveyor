import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId, useState } from 'react'
import {
  confirmRequirementVersion,
  confirmSystemDesignVersion,
  fetchRequirement,
  fetchSystemDesign,
  proposeRequirementVersion,
  proposeSystemDesignVersion,
} from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { VersionDiff } from './version-diff'

export type ReviseTarget = { id: string; title: string; tier: 'requirement' | 'system_design'; version: number }

// component-document-corpus v4; req-260810-23b69f AC-1.2 and AC-1.3.
export function VersionReviseDialog({ target, onClose }: { target: ReviseTarget; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const view = useQuery({
    queryKey: ['revise-version', workspace, target.tier, target.id, target.version],
    queryFn: async () => {
      const document = await (target.tier === 'requirement'
        ? fetchRequirement(target.id)
        : fetchSystemDesign(target.id))
      const pending = document.pending_versions.find((version) => version.version === target.version)
      if (!pending)
        throw new Error('This version is no longer pending. Close this dialog and review the document again.')
      return { pending, current: document.current_version }
    },
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  if (!view.data)
    return (
      <Dialog label={`Revise version ${target.version} of ${target.title}`} onClose={onClose}>
        <div className="space-y-4 p-5">
          <p>{view.error ? errorMessage(view.error) : 'Loading proposal…'}</p>
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        </div>
      </Dialog>
    )
  return <ReviseEditor target={target} initial={view.data} onClose={onClose} />
}

type Revision = {
  version: number
  content: string
  origin: string
  origin_task_id?: string
  origin_session_id?: string
}

function ReviseEditor({
  target,
  initial,
  onClose,
}: {
  target: ReviseTarget
  initial: { pending: Revision; current?: Revision }
  onClose: () => void
}) {
  // Keep the reviewed base and edits stable across query refreshes.
  const [snapshot] = useState(initial)
  const [note, setNote] = useState('')
  const noteCounterId = useId()
  const [content, setContent] = useState(snapshot.pending.content)
  const [proposedVersion, setProposedVersion] = useState<number | null>(null)
  const [phase, setPhase] = useState('Proposing…')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const submit = useMutation({
    mutationFn: async () => {
      setPhase('Proposing…')
      const proposed = await (target.tier === 'requirement'
        ? proposeRequirementVersion(target.id, content)
        : proposeSystemDesignVersion(target.id, content))
      setProposedVersion(proposed.version)
      setPhase('Confirming…')
      try {
        const expected = snapshot.current?.version ?? 0
        await (target.tier === 'requirement'
          ? confirmRequirementVersion(target.id, proposed.version, expected, note)
          : confirmSystemDesignVersion(target.id, proposed.version, expected, note))
      } catch (error) {
        throw new Error(
          `Version ${proposed.version} was proposed, but confirmation failed. It remains pending. Close this dialog and review that version on the document or queue before confirming again. ${errorMessage(error)}`,
        )
      }
    },
    onSuccess: onClose,
    onSettled: async () => {
      await Promise.all(
        [
          'revise-version',
          'pending-proposals',
          'activity',
          'task',
          'task-operations',
          'document-events',
          'workspace',
          'requirements',
          'requirement',
          'requirement-versions',
          'system-designs',
          'system-design',
          'system-design-versions',
        ].map((key) => client.invalidateQueries({ queryKey: [key, workspace] })),
      )
    },
  })
  const origin = snapshot.pending.origin_task_id
    ? `task ${snapshot.pending.origin_task_id}`
    : snapshot.pending.origin_session_id
      ? `planning session ${snapshot.pending.origin_session_id}`
      : snapshot.pending.origin
  return (
    <Dialog
      label={`Revise version ${target.version} of ${target.title}`}
      className="max-w-4xl"
      onClose={() => !submit.isPending && onClose()}
    >
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">
          Revise version {target.version} of {target.title}
        </h2>
        <p className="mt-1 text-sm text-muted">
          {target.tier === 'requirement' ? 'Requirement' : 'System Design'} · Origin: {origin}
        </p>
      </div>
      <form
        className="space-y-4 p-5"
        onSubmit={(event) => {
          event.preventDefault()
          if (!submit.isPending && proposedVersion === null) submit.mutate()
        }}
      >
        <label className="block text-sm font-medium">
          Proposal content
          <textarea
            className="mt-2 min-h-64 w-full rounded-md border border-border bg-card p-3 font-mono text-sm"
            value={content}
            onChange={(event) => setContent(event.target.value)}
            readOnly={submit.isPending || proposedVersion !== null}
          />
        </label>
        <VersionDiff
          bounded
          left={{
            content: snapshot.current?.content ?? '',
            label: snapshot.current ? `Confirmed v${snapshot.current.version}` : 'No confirmed version',
          }}
          right={{ content, label: 'Edited proposal' }}
        />
        <p className="text-sm text-muted">
          Submitting proposes a new version and confirms it. Earlier pending versions will be dismissed.
        </p>
        <label className="block text-sm font-medium">
          What was wrong with the original?
          <span className="ml-1 font-normal text-muted">(optional)</span>
          <textarea
            className="mt-2 min-h-24 w-full rounded-md border border-border bg-card p-3 text-sm"
            value={note}
            onChange={(event) => setNote(Array.from(event.target.value).slice(0, 2000).join(''))}
            disabled={submit.isPending || proposedVersion !== null}
            aria-describedby={noteCounterId}
          />
        </label>
        <p id={noteCounterId} className="text-xs text-muted">
          {Array.from(note).length} / 2000 characters
        </p>
        {submit.error && (
          <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(submit.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" disabled={submit.isPending} onClick={onClose}>
            {proposedVersion === null ? 'Cancel' : 'Close'}
          </Button>
          <Button type="submit" disabled={submit.isPending || proposedVersion !== null || !content.trim()}>
            {submit.isPending ? phase : 'Propose and confirm'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
