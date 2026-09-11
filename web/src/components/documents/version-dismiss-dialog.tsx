import { X } from 'lucide-react'
import { useId, useState } from 'react'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'

export function VersionDismissDialog({
  documentTitle,
  version,
  pending,
  error,
  onCancel,
  onConfirm,
}: {
  documentTitle: string
  version: number
  pending: boolean
  error?: string
  onCancel: () => void
  onConfirm: (note: string) => void
}) {
  const [note, setNote] = useState('')
  const noteCounterId = useId()
  return (
    <Dialog label={`Dismiss version ${version} of ${documentTitle}`} onClose={() => !pending && onCancel()}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">Dismiss version {version}?</h2>
        <p className="mt-1 text-sm text-muted">{documentTitle}</p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <p className="text-sm leading-6 text-muted">
          This version's content will stay in version history, but it cannot be confirmed later.
        </p>
        <label className="block text-sm font-medium">
          Why are you dismissing this?
          <span className="ml-1 font-normal text-muted">(optional)</span>
          <textarea
            className="mt-2 min-h-24 w-full rounded-md border border-border bg-card p-3 text-sm"
            value={note}
            onChange={(event) => setNote(Array.from(event.target.value).slice(0, 2000).join(''))}
            disabled={pending}
            aria-describedby={noteCounterId}
          />
        </label>
        <p id={noteCounterId} className="text-xs text-muted">
          {Array.from(note).length} / 2000 characters
        </p>
        {error && <p className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" disabled={pending} onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={pending} onClick={() => onConfirm(note)}>
            <X /> {pending ? 'Dismissing…' : `Dismiss version ${version}`}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
