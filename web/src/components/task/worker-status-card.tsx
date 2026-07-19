import { CircleCheck, TriangleAlert } from 'lucide-react'
import type { ActivityItem } from '../../lib/types'

export function hasWorkerAlert(item: ActivityItem) {
  return item.worker_status != null && !item.worker_status.available
}

export function WorkerStatusCard({ item }: { item: ActivityItem }) {
  const status = item.worker_status
  if (!status || status.available) return null
  const requiredHarnesses = status.required_harnesses ?? []
  return (
    <section className="rounded-lg border border-attention/40 bg-attention-soft px-3 py-3" aria-label="Auto worker unavailable">
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" />
        <div className="text-xs leading-5 text-muted">
          <p className="font-medium text-attention">No healthy worker can serve this Auto task</p>
          <p>{status.reason}.</p>
          <p>Required harnesses: {requiredHarnesses.length ? requiredHarnesses.join(', ') : 'not yet routed'}.</p>
          <p>{status.queue_context === 'interrupted' ? 'This work was interrupted and may require recovery.' : 'This queued work has never started.'}</p>
          <p className="mt-1 flex items-center gap-1"><CircleCheck className="size-3" /> Restarting an enrolled worker normally reuses its saved credential; no new pairing token is needed.</p>
        </div>
      </div>
    </section>
  )
}
