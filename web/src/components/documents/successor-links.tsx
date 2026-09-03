import { Link } from '@tanstack/react-router'

export function SuccessorLinks({ ids, compact = false }: { ids?: string[]; compact?: boolean }) {
  if (!ids?.length) return null
  return (
    <span className={compact ? 'text-[10px] text-muted' : 'text-sm text-muted'}>
      Superseded by{' '}
      {ids.map((id, index) => (
        <span key={id}>
          {index > 0 && ', '}
          {id.startsWith('req-') ? (
            <Link to="/requirements" search={{ requirement: id }} className="font-mono text-primary hover:underline">
              {id}
            </Link>
          ) : (
            <Link to="/system-design" search={{ document: id }} className="font-mono text-primary hover:underline">
              {id}
            </Link>
          )}
        </span>
      ))}
    </span>
  )
}
