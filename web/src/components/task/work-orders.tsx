import type { WorkOrder } from '../../lib/types'
import { Badge } from '../ui/badge'

// MCP work orders (spec §21.4): implementation and review delegate to
// operator-owned agents; the card narrates claim/lease/deadline state.
export function WorkOrders({ orders }: { orders: WorkOrder[] }) {
  if (orders.length === 0) return null
  return (
    <>
      {orders.map((order) => (
        <div key={order.id} className="rounded-lg border border-border bg-surface p-3">
          <div className="flex items-center gap-2">
            <Badge variant="accent">{order.stage} work order</Badge>
            <Badge variant={order.state === 'claimed' ? 'positive' : order.state === 'timed_out' ? 'failure' : order.state === 'stale' ? 'attention' : 'mono'}>{order.state.replaceAll('_', ' ')}</Badge>
            <Badge variant="outline">{order.claimable ? 'claimable' : 'not claimable'}</Badge>
          </div>
          <p className="mt-2 text-xs text-muted">
            {order.state === 'stale'
              ? 'Queue retention elapsed; redispatch is required.'
              : order.state === 'timed_out'
                ? 'Execution deadline elapsed; use the existing retry policy.'
                : order.claimed_by
                  ? `Claimed by ${order.claimed_by}${order.agent ? ` · ${order.agent}` : ''}${order.model ? ` · ${order.model}` : ''}`
                  : 'Waiting for an operator-owned agent over MCP.'}
          </p>
          <p className="mt-1 text-[11px] text-faint">Queued {new Date(order.queue_entered_at).toLocaleString()} · queue deadline {new Date(order.queue_deadline).toLocaleString()}</p>
          {order.execution_deadline && <p className="mt-1 text-[11px] text-faint">Execution deadline {new Date(order.execution_deadline).toLocaleString()}</p>}
          {order.lease_expires_at && <p className="mt-1 text-[11px] text-faint">Lease expires {new Date(order.lease_expires_at).toLocaleString()} · usage self-reported</p>}
          {order.progress && <p className="mt-2 text-xs">{order.progress}</p>}
        </div>
      ))}
    </>
  )
}
