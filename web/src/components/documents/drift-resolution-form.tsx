import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button } from '../ui/button'
import { Select } from '../ui/input'
import { resolveMonitorDrift } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { MonitorDriftOutcome, RepositoryDrift, RequirementView } from '../../lib/types'

type DriftResolutionSurface = 'monitor' | 'requirement' | 'system_design'

const driftOutcomes: Record<DriftResolutionSurface, Array<{ value: MonitorDriftOutcome; label: string }>> = {
  monitor: [
    { value: 'conflict_resolved', label: 'Conflict resolved' },
    { value: 'requirements_amended', label: 'Requirements amended' },
    { value: 'design_document_updated', label: 'Design document updated' },
    { value: 'change_reverted', label: 'Change reverted' },
  ],
  requirement: [
    { value: 'conflict_resolved', label: 'Conflict resolved' },
    { value: 'requirements_amended', label: 'Requirements amended' },
    { value: 'change_reverted', label: 'Change reverted' },
  ],
  system_design: [
    { value: 'conflict_resolved', label: 'Conflict resolved' },
    { value: 'design_document_updated', label: 'Design document updated' },
    { value: 'change_reverted', label: 'Change reverted' },
  ],
}

export function DriftResolutionForm({
  drift,
  surface,
  token,
  workspace,
  requirementID: fixedRequirementID,
  requirements = [],
  requirementsPending = false,
  onResolved,
}: {
  drift: RepositoryDrift
  surface: DriftResolutionSurface
  token: string
  workspace: string
  requirementID?: string
  requirements?: RequirementView[]
  requirementsPending?: boolean
  onResolved?: () => Promise<unknown> | unknown
}) {
  const queryClient = useQueryClient()
  const [outcome, setOutcome] = useState<MonitorDriftOutcome>('conflict_resolved')
  const [selectedRequirementID, setSelectedRequirementID] = useState(drift.requirement_id ?? '')
  const requirementID = fixedRequirementID ?? selectedRequirementID
  const mutation = useMutation({
    mutationFn: () =>
      resolveMonitorDrift(token, drift.id, outcome, outcome === 'requirements_amended' ? requirementID : undefined),
    onSuccess: async () => {
      await Promise.all([queryClient.invalidateQueries({ queryKey: ['monitor', workspace] }), onResolved?.()])
    },
  })
  const needsRequirement = outcome === 'requirements_amended'
  const showRequirementPicker = needsRequirement && surface === 'monitor'
  const outcomeSelectID = `resolution-outcome-${drift.id}`
  const requirementSelectID = `resolution-requirement-${drift.id}`
  return (
    <form
      className="flex flex-wrap items-end gap-2"
      aria-label={`Resolve drift ${drift.id}`}
      onSubmit={(event) => {
        event.preventDefault()
        mutation.mutate()
      }}
    >
      <label className="min-w-48 text-xs text-muted" htmlFor={outcomeSelectID}>
        Resolution
        <Select
          id={outcomeSelectID}
          aria-label={`Resolution outcome for ${drift.id}`}
          value={outcome}
          onChange={(event) => setOutcome(event.target.value as MonitorDriftOutcome)}
        >
          {driftOutcomes[surface].map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Select>
      </label>
      {showRequirementPicker && (
        <label className="min-w-56 flex-1 text-xs text-muted" htmlFor={requirementSelectID}>
          Confirmed requirement
          <Select
            id={requirementSelectID}
            aria-label={`Confirmed requirement for ${drift.id}`}
            value={selectedRequirementID}
            onChange={(event) => setSelectedRequirementID(event.target.value)}
            disabled={requirementsPending}
          >
            <option value="">{requirementsPending ? 'Loading confirmed requirements…' : 'Select a requirement'}</option>
            {requirements.map((item) => (
              <option key={item.requirement.id} value={item.requirement.id}>
                {item.requirement.title}
              </option>
            ))}
          </Select>
          {!requirementsPending && requirements.length === 0 && (
            <span className="mt-1 block text-faint">No confirmed requirements are available.</span>
          )}
        </label>
      )}
      <Button type="submit" size="sm" disabled={!token || mutation.isPending || (needsRequirement && !requirementID)}>
        {mutation.isPending ? 'Resolving…' : 'Resolve'}
      </Button>
      {mutation.error && (
        <p className="basis-full text-xs text-failure">
          {errorMessage(mutation.error, 'Could not resolve this repository change.')}
        </p>
      )}
    </form>
  )
}
