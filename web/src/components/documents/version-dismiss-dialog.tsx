import { X } from 'lucide-react'
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
  onConfirm: () => void
}) {
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
        {error && <p className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" disabled={pending} onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={pending} onClick={onConfirm}>
            <X /> {pending ? 'Dismissing…' : `Dismiss version ${version}`}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
