import { Link } from '@tanstack/react-router'
import { useWorkspaceSelection } from '../app-shell'

export function SuccessorLinks({ ids, compact = false }: { ids?: string[]; compact?: boolean }) {
  const { workspace } = useWorkspaceSelection()
  if (!ids?.length) return null
  return (
    <span className={compact ? 'text-[10px] text-muted' : 'text-sm text-muted'}>
      Superseded by{' '}
      {ids.map((id, index) => (
        <span key={id}>
          {index > 0 && ', '}
          {id.startsWith('req-') ? (
            <Link
              to="/w/$workspaceId/requirements"
              params={{ workspaceId: workspace }}
              search={{ requirement: id }}
              className="font-mono text-primary hover:underline"
            >
              {id}
            </Link>
          ) : (
            <Link
              to="/w/$workspaceId/system-design"
              params={{ workspaceId: workspace }}
              search={{ document: id }}
              className="font-mono text-primary hover:underline"
            >
              {id}
            </Link>
          )}
        </span>
      ))}
    </span>
  )
}
