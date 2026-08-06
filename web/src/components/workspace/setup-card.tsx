import { ChevronRight, Copy, Plus, Trash2 } from 'lucide-react'
import type { ExecutionSetup, WorkspaceConfigDocument, WorkspaceReviewSeat } from '../../lib/types'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Input, Select } from '../ui/input'
import { cn } from '../../lib/utils'
import { Field } from './field'

// One named execution setup (spec §21.27): a collapsed pipeline summary that
// expands into the §21.18 contextual layout. Edits write into
// document.setups[index]; the top-level execution_settings/review projection
// is kept in sync while the setup is the workspace default.
export function SetupCard({
  document,
  setup,
  index,
  expanded,
  onToggle,
  workerReady,
  workerReason,
  setDraft,
  onDuplicate,
  onDelete,
}: {
  document: WorkspaceConfigDocument
  setup: ExecutionSetup
  index: number
  expanded: boolean
  onToggle: () => void
  workerReady: boolean
  workerReason?: string
  setDraft: (value: WorkspaceConfigDocument) => void
  onDuplicate: () => void
  onDelete: () => void
}) {
  const isDefault = document.default_setup === setup.name
  const settings = setup.execution_settings
  const planning = settings.control_plane.planning ?? {
    model: settings.control_plane.triage.model,
    effort: settings.control_plane.triage.effort,
    timeout: settings.control_plane.triage.timeout,
    exploration_output_tokens: 10_000,
    context: { depth: 5, nodes: 32, renderable_bytes: 262_144, artifact_refs: 64, authority_nodes: 256 },
  }
  const seats = setup.review.seats
  const planningContext = {
    depth: 5,
    nodes: 32,
    renderable_bytes: 262_144,
    artifact_refs: 64,
    authority_nodes: 256,
    ...planning.context,
  }

  const updateSetup = (change: Partial<ExecutionSetup>) => {
    const setups = [...document.setups]
    setups[index] = { ...setups[index], ...change }
    const updated = setups[index]
    setDraft({
      ...document,
      setups,
      ...(isDefault && change.name !== undefined ? { default_setup: updated.name } : {}),
      ...(isDefault ? { execution_settings: updated.execution_settings, review: updated.review } : {}),
    })
  }
  const updateTriage = (change: Partial<WorkspaceConfigDocument['execution_settings']['control_plane']['triage']>) =>
    updateSetup({
      execution_settings: {
        ...settings,
        control_plane: { ...settings.control_plane, triage: { ...settings.control_plane.triage, ...change } },
      },
    })
  const updatePlanning = (
    change: Partial<WorkspaceConfigDocument['execution_settings']['control_plane']['planning']>,
  ) =>
    updateSetup({
      execution_settings: {
        ...settings,
        control_plane: { ...settings.control_plane, planning: { ...planning, ...change } },
      },
    })
  const updatePlanningContext = (change: Partial<typeof planningContext>) =>
    updatePlanning({ context: { ...planningContext, ...change } })
  const updateSpec = (change: Partial<typeof settings.spec>) =>
    updateSetup({ execution_settings: { ...settings, spec: { ...settings.spec, ...change } } })
  const updateImplementation = (change: Partial<typeof settings.implementation>) =>
    updateSetup({ execution_settings: { ...settings, implementation: { ...settings.implementation, ...change } } })
  const updateReviewExecution = (change: Partial<typeof settings.review>) =>
    updateSetup({ execution_settings: { ...settings, review: { ...settings.review, ...change } } })
  const updateSeat = (seatIndex: number, change: Partial<WorkspaceReviewSeat>) => {
    const next = [...seats]
    next[seatIndex] = { ...next[seatIndex], ...change }
    updateSetup({ review: { seats: next } })
  }

  const implementationHarness = document.harnesses.find((harness) => harness.name === settings.implementation.harness)
  const specHarness = document.harnesses.find((harness) => harness.name === settings.spec.harness)
  const reviewNeedsFallback = seats.some((seat) => !seat.harness)
  const implementSummary = [
    settings.implementation.harness || 'no harness',
    settings.implementation.model_policy === 'explicit'
      ? settings.implementation.model || 'no model'
      : 'harness default',
    ...(settings.implementation.effort ? [`${settings.implementation.effort} effort`] : []),
  ].join(' · ')

  return (
    <div className="rounded-md border border-border bg-card">
      <button
        type="button"
        aria-expanded={expanded}
        aria-label={`Toggle ${setup.name} setup`}
        onClick={onToggle}
        className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-surface focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
      >
        <span className="text-sm font-semibold">{setup.name}</span>
        {isDefault && <Badge variant="accent">Default</Badge>}
        <span className="flex items-center gap-1.5 text-xs text-muted">
          <span className={cn('size-1.5 rounded-full', workerReady ? 'bg-positive' : 'bg-attention-dot')} />
          {workerReady ? 'Worker ready' : `Worker can't serve${workerReason ? ` — ${workerReason}` : ''}`}
        </span>
        <ChevronRight
          className={cn('ml-auto size-4 shrink-0 text-faint transition-transform', expanded && 'rotate-90')}
        />
      </button>

      <div className="flex items-stretch gap-4 overflow-x-auto px-4 pb-3">
        <Stage
          name="Triage"
          model={settings.control_plane.triage.model}
          meta={`${settings.control_plane.triage.timeout} limit${settings.control_plane.triage.effort ? ` · ${settings.control_plane.triage.effort} effort` : ''}`}
        />
        <Stage
          name="Planning"
          model={planning.model}
          meta={`${planning.exploration_output_tokens.toLocaleString()} tokens/call`}
          connected
        />
        <Stage
          name="Spec"
          model={`${settings.spec.harness || 'no harness'} · ${settings.spec.model_policy === 'explicit' ? settings.spec.model || 'no model' : 'harness default'}`}
          meta={`${settings.spec.timeout} limit`}
          connected
        />
        <Stage name="Implement" model={implementSummary} meta={`${settings.implementation.timeout} limit`} connected />
        <Stage
          name="Review"
          model={`${seats.length} ${seats.length === 1 ? 'seat' : 'seats'}`}
          meta={`all must approve · ${settings.review.timeout}`}
          connected
        />
      </div>

      {expanded && (
        <div className="border-t border-border">
          <div className="border-b border-border px-4 py-4">
            <GroupTitle title="Triage" note="runs inside Conveyor" />
            <div className="grid gap-3 md:grid-cols-3">
              <Field label="Triage model">
                <Input
                  aria-label="triage model"
                  className="font-mono"
                  value={settings.control_plane.triage.model}
                  onChange={(event) => updateTriage({ model: event.target.value })}
                />
              </Field>
              <Field
                label="Reasoning effort"
                hint="Passed to the provider's Responses API; leave unset for the provider default."
              >
                <Select
                  aria-label="triage reasoning effort"
                  value={settings.control_plane.triage.effort ?? ''}
                  onChange={(event) =>
                    updateTriage({
                      effort: (event.target.value || undefined) as typeof settings.control_plane.triage.effort,
                    })
                  }
                >
                  <option value="">Provider default</option>
                  <option value="minimal">minimal</option>
                  <option value="low">low</option>
                  <option value="medium">medium</option>
                  <option value="high">high</option>
                </Select>
              </Field>
              <Field label="Time limit">
                <Input
                  aria-label="triage timeout"
                  value={settings.control_plane.triage.timeout}
                  onChange={(event) => updateTriage({ timeout: event.target.value })}
                />
              </Field>
            </div>
          </div>

          <div className="border-b border-border px-4 py-4">
            <GroupTitle title="Planning" note="runs inside Conveyor over immutable repository snapshots" />
            <div className="grid gap-3 md:grid-cols-4">
              <Field label="Default model">
                <Input
                  aria-label="planning model"
                  className="font-mono"
                  value={planning.model}
                  onChange={(event) => updatePlanning({ model: event.target.value })}
                />
              </Field>
              <Field label="Reasoning effort">
                <Select
                  aria-label="planning reasoning effort"
                  value={planning.effort ?? ''}
                  onChange={(event) =>
                    updatePlanning({ effort: (event.target.value || undefined) as typeof planning.effort })
                  }
                >
                  <option value="">Provider default</option>
                  <option value="minimal">minimal</option>
                  <option value="low">low</option>
                  <option value="medium">medium</option>
                  <option value="high">high</option>
                </Select>
              </Field>
              <Field label="Time limit">
                <Input
                  aria-label="planning timeout"
                  value={planning.timeout}
                  onChange={(event) => updatePlanning({ timeout: event.target.value })}
                />
              </Field>
              <Field label="Exploration output tokens">
                <Input
                  aria-label="planning exploration output tokens"
                  type="number"
                  min={1}
                  value={planning.exploration_output_tokens}
                  onChange={(event) => updatePlanning({ exploration_output_tokens: Number(event.target.value) })}
                />
              </Field>
              <Field label="Context depth">
                <Input
                  aria-label="planning context depth"
                  type="number"
                  min={1}
                  value={planningContext.depth}
                  onChange={(event) => updatePlanningContext({ depth: Number(event.target.value) })}
                />
              </Field>
              <Field label="Context nodes">
                <Input
                  aria-label="planning context nodes"
                  type="number"
                  min={1}
                  value={planningContext.nodes}
                  onChange={(event) => updatePlanningContext({ nodes: Number(event.target.value) })}
                />
              </Field>
              <Field label="Context bytes">
                <Input
                  aria-label="planning context renderable bytes"
                  type="number"
                  min={1}
                  value={planningContext.renderable_bytes}
                  onChange={(event) => updatePlanningContext({ renderable_bytes: Number(event.target.value) })}
                />
              </Field>
              <Field label="Artifact references">
                <Input
                  aria-label="planning context artifact references"
                  type="number"
                  min={1}
                  value={planningContext.artifact_refs}
                  onChange={(event) => updatePlanningContext({ artifact_refs: Number(event.target.value) })}
                />
              </Field>
              <Field
                label="Authority nodes"
                hint="Raise this bounded requirement-authority limit when a task pauses with authority_budget_exceeded."
              >
                <Input
                  aria-label="served requirement authority nodes"
                  type="number"
                  min={8}
                  value={planningContext.authority_nodes}
                  onChange={(event) => updatePlanningContext({ authority_nodes: Number(event.target.value) })}
                />
              </Field>
            </div>
          </div>

          <div className="border-b border-border px-4 py-4">
            <GroupTitle title="Plan" note="produces the execution plan over MCP without git changes" />
            <div className="grid gap-3 md:grid-cols-5">
              <Field label="Harness">
                <Select
                  aria-label="Spec harness"
                  value={settings.spec.harness}
                  onChange={(event) => updateSpec({ harness: event.target.value, model: '' })}
                >
                  <option value="">Select harness</option>
                  {document.harnesses.map((harness) => (
                    <option key={harness.name} value={harness.name}>
                      {harness.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="Model policy">
                <Select
                  aria-label="Spec model policy"
                  value={settings.spec.model_policy}
                  onChange={(event) =>
                    updateSpec({ model_policy: event.target.value as 'explicit' | 'harness_default', model: '' })
                  }
                >
                  <option value="explicit">Explicit model</option>
                  <option value="harness_default">Harness default</option>
                </Select>
              </Field>
              {settings.spec.model_policy === 'explicit' ? (
                <Field label="Model">
                  <Input
                    aria-label="Spec model"
                    className="font-mono"
                    value={settings.spec.model ?? ''}
                    onChange={(event) => updateSpec({ model: event.target.value })}
                  />
                </Field>
              ) : (
                <Field label="Default sentinel">
                  <Select
                    aria-label="Spec default sentinel"
                    value={settings.spec.model ?? ''}
                    onChange={(event) => updateSpec({ model: event.target.value })}
                  >
                    <option value="">Omit model arguments</option>
                    {(specHarness?.default_model_sentinels ?? []).map((sentinel) => (
                      <option key={sentinel} value={sentinel}>
                        {sentinel}
                      </option>
                    ))}
                  </Select>
                </Field>
              )}
              <Field label="Reasoning effort">
                <Select
                  aria-label="spec reasoning effort"
                  value={settings.spec.effort ?? ''}
                  onChange={(event) =>
                    updateSpec({ effort: (event.target.value || undefined) as typeof settings.spec.effort })
                  }
                >
                  <option value="">Harness default</option>
                  <option value="low">Low</option>
                  <option value="medium">Medium</option>
                  <option value="high">High</option>
                </Select>
              </Field>
              <Field label="Time limit">
                <Input
                  aria-label="spec timeout"
                  value={settings.spec.timeout}
                  onChange={(event) => updateSpec({ timeout: event.target.value })}
                />
              </Field>
            </div>
          </div>

          <div className="border-b border-border px-4 py-4">
            <GroupTitle title="Implementation" note="runs on your worker over MCP" />
            <div className="grid gap-3 md:grid-cols-4">
              <Field label="Harness">
                <Select
                  aria-label="Implementation harness"
                  value={settings.implementation.harness}
                  onChange={(event) => updateImplementation({ harness: event.target.value, model: '' })}
                >
                  <option value="">Select harness</option>
                  {document.harnesses.map((harness) => (
                    <option key={harness.name} value={harness.name}>
                      {harness.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="Model policy">
                <Select
                  aria-label="Implementation model policy"
                  value={settings.implementation.model_policy}
                  onChange={(event) =>
                    updateImplementation({
                      model_policy: event.target.value as 'explicit' | 'harness_default',
                      model: '',
                    })
                  }
                >
                  <option value="explicit">Explicit model</option>
                  <option value="harness_default">Harness default</option>
                </Select>
              </Field>
              {settings.implementation.model_policy === 'explicit' ? (
                <Field label="Model">
                  <Input
                    aria-label="Implementation model"
                    className="font-mono"
                    value={settings.implementation.model ?? ''}
                    placeholder="Provider model ID"
                    onChange={(event) => updateImplementation({ model: event.target.value })}
                  />
                </Field>
              ) : (
                <Field label="Default sentinel">
                  <Select
                    aria-label="Implementation default sentinel"
                    value={settings.implementation.model ?? ''}
                    onChange={(event) => updateImplementation({ model: event.target.value })}
                  >
                    <option value="">Omit model arguments</option>
                    {(implementationHarness?.default_model_sentinels ?? []).map((sentinel) => (
                      <option key={sentinel} value={sentinel}>
                        {sentinel}
                      </option>
                    ))}
                  </Select>
                </Field>
              )}
              <Field label="Reasoning effort">
                <Select
                  aria-label="Implementation reasoning effort"
                  value={settings.implementation.effort ?? ''}
                  onChange={(event) =>
                    updateImplementation({
                      effort: (event.target.value || undefined) as typeof settings.implementation.effort,
                    })
                  }
                >
                  <option value="">Harness default</option>
                  <option value="low">Low</option>
                  <option value="medium">Medium</option>
                  <option value="high">High</option>
                </Select>
              </Field>
              <Field label="Time limit">
                <Input
                  aria-label="Implementation timeout"
                  className="max-w-28"
                  value={settings.implementation.timeout}
                  onChange={(event) => updateImplementation({ timeout: event.target.value })}
                />
              </Field>
            </div>
          </div>

          <div className="border-b border-border px-4 py-4">
            <GroupTitle title="Review panel" note="every seat must approve" />
            <div className="grid gap-3 md:grid-cols-3">
              <Field label="Refresh review" hint="Scope used when the approved pull-request head changes.">
                <Select
                  aria-label="Refresh review"
                  value={setup.refresh_review}
                  onChange={(event) =>
                    updateSetup({ refresh_review: event.target.value as ExecutionSetup['refresh_review'] })
                  }
                >
                  <option value="delta">Delta since approval</option>
                  <option value="full">Full branch diff</option>
                  <option value="none">Skip clean refresh</option>
                </Select>
              </Field>
              <Field label="Execution">
                <Select
                  aria-label="Review execution"
                  value={settings.review.execution}
                  onChange={(event) =>
                    updateReviewExecution({
                      execution: event.target.value as 'mcp' | 'in_process',
                      ...(event.target.value === 'in_process' ? { fallback_harness: undefined } : {}),
                    })
                  }
                >
                  <option value="mcp">On your worker (MCP)</option>
                  <option value="in_process">In-process</option>
                </Select>
              </Field>
              <Field label="Time limit">
                <Input
                  aria-label="Review timeout"
                  value={settings.review.timeout}
                  onChange={(event) => updateReviewExecution({ timeout: event.target.value })}
                />
              </Field>
              {settings.review.execution === 'mcp' && reviewNeedsFallback && (
                <Field label="Fallback harness">
                  <Select
                    aria-label="Review fallback harness"
                    value={settings.review.fallback_harness ?? ''}
                    onChange={(event) => updateReviewExecution({ fallback_harness: event.target.value || undefined })}
                  >
                    <option value="">Select fallback</option>
                    {document.harnesses.map((harness) => (
                      <option key={harness.name} value={harness.name}>
                        {harness.name}
                      </option>
                    ))}
                  </Select>
                </Field>
              )}
            </div>
            <p className="mt-2 text-xs text-faint">
              {reviewNeedsFallback
                ? 'Seats without a harness use the review fallback; only that fallback is validated and health-gated.'
                : 'Every seat is explicitly routed.'}
            </p>
            <div className="mt-3 space-y-2">
              {seats.map((seat, seatIndex) => (
                <div
                  key={seatIndex}
                  className="grid items-end gap-3 rounded-md border border-border px-3 py-2.5 md:grid-cols-[auto_1fr_1fr_1fr_auto]"
                >
                  <span className="pb-2 text-xs text-faint">Seat {seatIndex + 1}</span>
                  <Field label="Pinned model">
                    <Input
                      aria-label="Pinned model"
                      className="font-mono"
                      value={seat.model}
                      onChange={(event) => updateSeat(seatIndex, { model: event.target.value })}
                    />
                  </Field>
                  <Field label="Harness">
                    <Select
                      aria-label={`Seat ${seatIndex + 1} harness`}
                      value={seat.harness ?? ''}
                      onChange={(event) => updateSeat(seatIndex, { harness: event.target.value || undefined })}
                    >
                      <option value="">Use review fallback</option>
                      {document.harnesses.map((harness) => (
                        <option key={harness.name} value={harness.name}>
                          {harness.name}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field label="Reasoning effort">
                    <Select
                      aria-label={`Seat ${seatIndex + 1} reasoning effort`}
                      value={seat.effort ?? ''}
                      onChange={(event) =>
                        updateSeat(seatIndex, {
                          effort: (event.target.value || undefined) as WorkspaceReviewSeat['effort'],
                        })
                      }
                    >
                      <option value="">Harness default</option>
                      <option value="low">Low</option>
                      <option value="medium">Medium</option>
                      <option value="high">High</option>
                    </Select>
                  </Field>
                  <Button
                    aria-label={`Remove review seat ${seatIndex + 1}`}
                    size="icon"
                    variant="ghost"
                    className="mb-0.5 hover:text-failure"
                    onClick={() => updateSetup({ review: { seats: seats.filter((_, i) => i !== seatIndex) } })}
                  >
                    <Trash2 />
                  </Button>
                </div>
              ))}
            </div>
            <Button
              size="sm"
              variant="secondary"
              className="mt-3"
              onClick={() =>
                updateSetup({
                  review: {
                    seats: [
                      ...seats,
                      { model: settings.review.fallback_model ?? '', harness: undefined, effort: undefined },
                    ],
                  },
                })
              }
            >
              <Plus />
              Add seat
            </Button>
          </div>

          <div className="flex flex-wrap items-end gap-3 bg-surface px-4 py-3">
            <Field label="Setup name">
              <Input
                aria-label="Setup name"
                value={setup.name}
                onChange={(event) => updateSetup({ name: event.target.value })}
              />
            </Field>
            <div className="ml-auto flex gap-2">
              <Button size="sm" variant="secondary" aria-label={`Duplicate ${setup.name}`} onClick={onDuplicate}>
                <Copy />
                Duplicate
              </Button>
              <Button
                size="sm"
                variant="secondary"
                aria-label={`Set ${setup.name} as default`}
                disabled={isDefault}
                onClick={() =>
                  setDraft({
                    ...document,
                    default_setup: setup.name,
                    execution_settings: setup.execution_settings,
                    review: setup.review,
                  })
                }
              >
                Set as default
              </Button>
              <Button
                size="sm"
                variant="destructive"
                aria-label={`Delete ${setup.name}`}
                disabled={document.setups.length <= 1}
                onClick={onDelete}
              >
                <Trash2 />
                Delete
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function Stage({ name, model, meta, connected }: { name: string; model: string; meta: string; connected?: boolean }) {
  return (
    <div
      className={cn(
        'relative min-w-40 flex-1 rounded-md border border-border bg-surface px-3 py-2',
        connected &&
          "before:absolute before:-left-4 before:top-1/2 before:h-px before:w-4 before:bg-edge before:content-['']",
      )}
    >
      <p className="text-[10px] font-semibold uppercase tracking-wider text-faint">{name}</p>
      <p className="truncate font-mono text-xs text-foreground" title={model}>
        {model}
      </p>
      <p className="mt-0.5 text-[11px] text-faint">{meta}</p>
    </div>
  )
}

function GroupTitle({ title, note }: { title: string; note: string }) {
  return (
    <h4 className="mb-3 text-sm font-semibold">
      {title} <span className="font-normal text-faint">— {note}</span>
    </h4>
  )
}
