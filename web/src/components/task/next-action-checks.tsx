import type { NextAction } from '../../lib/types'

// The checks a next action names, with the GitHub link that is how an operator
// reruns one: Conveyor has no rerun act, and a commit status has no rerun at
// all (component-next-action, The statement; req-operator-next-action AC-1.5).
export function CheckStatusList({ action }: { action: NextAction | null | undefined }) {
  const details = action?.details
  const checks = details?.checks ?? []
  if (checks.length === 0 && !details?.check_read_failure && !details?.check_remedy) return null
  return (
    <div data-check-status className="space-y-1.5 text-sm">
      {checks.length > 0 && (
        <ul aria-label="Checks" className="space-y-1">
          {checks.map((check) => (
            <li
              key={`${check.name}:${check.state}:${check.url ?? ''}`}
              className="flex flex-wrap items-baseline gap-x-2"
            >
              {check.url ? (
                <a
                  className="font-medium underline-offset-2 hover:underline"
                  href={check.url}
                  rel="noreferrer"
                  target="_blank"
                >
                  {check.name}
                </a>
              ) : (
                <span className="font-medium">{check.name}</span>
              )}
              <span className="text-xs text-muted">{check.state}</span>
            </li>
          ))}
        </ul>
      )}
      {details?.check_read_failure && <p className="text-muted">{details.check_read_failure}</p>}
      {details?.check_remedy && <p className="text-muted">{details.check_remedy}</p>}
    </div>
  )
}
