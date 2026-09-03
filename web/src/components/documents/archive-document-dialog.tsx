import { Archive } from 'lucide-react'
import { useState } from 'react'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'

export interface SuccessorCandidate {
  id: string
  title: string
  kind: 'requirement' | 'system_design'
}

export function ArchiveDocumentDialog({
  documentTitle,
  candidates,
  pending,
  error,
  onCancel,
  onConfirm,
}: {
  documentTitle: string
  candidates: SuccessorCandidate[]
  pending: boolean
  error?: string
  onCancel: () => void
  onConfirm: (supersededBy: string[]) => void
}) {
  const [selected, setSelected] = useState<string[]>([])
  const toggle = (id: string) =>
    setSelected((current) =>
      current.includes(id) ? current.filter((candidate) => candidate !== id) : [...current, id],
    )

  return (
    <Dialog label={`Archive ${documentTitle}`} onClose={() => !pending && onCancel()}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">Archive {documentTitle}?</h2>
        <p className="mt-1 text-sm text-muted">
          Its history stays available, but agents stop treating it as live authority.
        </p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <fieldset>
          <legend className="text-sm font-medium">Documents that replace this one (optional)</legend>
          <p className="mt-1 text-xs text-muted">
            Select every live requirement or System Design that absorbed its claims.
          </p>
          <div className="mt-3 max-h-56 space-y-1 overflow-y-auto rounded-md border border-border p-2">
            {candidates.length === 0 && <p className="px-2 py-1 text-xs text-faint">No other live documents.</p>}
            {candidates.map((candidate) => (
              <label
                key={`${candidate.kind}:${candidate.id}`}
                className="flex cursor-pointer items-start gap-2 rounded px-2 py-1.5 hover:bg-surface"
              >
                <input
                  type="checkbox"
                  checked={selected.includes(candidate.id)}
                  disabled={pending}
                  onChange={() => toggle(candidate.id)}
                  className="mt-0.5"
                />
                <span className="min-w-0 text-sm">
                  <span className="block truncate">{candidate.title}</span>
                  <span className="block font-mono text-[10px] text-faint">
                    {candidate.kind === 'requirement' ? 'Requirement' : 'System Design'} · {candidate.id}
                  </span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>
        {error && <p className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" disabled={pending} onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={pending} onClick={() => onConfirm(selected)}>
            <Archive /> {pending ? 'Archiving…' : 'Archive'}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
