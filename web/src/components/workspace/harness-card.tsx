import { ChevronRight, Trash2 } from 'lucide-react'
import type { HarnessProbe, WorkerList, WorkspaceHarness } from '../../lib/types'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Input, Select } from '../ui/input'
import { cn } from '../../lib/utils'
import { ArgvInput } from './argv-input'
import { Field } from './field'

const TRANSPORT_LABELS: Record<WorkspaceHarness['mcp_transport'], string> = {
  json_file: 'JSON file',
  toml_override: 'TOML override',
  environment: 'Environment',
}

// Latest probe result for a harness across all enrolled workers.
export function latestProbe(workers: WorkerList | undefined, harness: string): HarnessProbe | undefined {
  let latest: HarnessProbe | undefined
  for (const worker of workers?.workers ?? []) {
    for (const probe of worker.probes ?? []) {
      if (probe.harness === harness && (!latest || probe.checked_at > latest.checked_at)) latest = probe
    }
  }
  return latest
}

export function HarnessCard({ harness, index, expanded, onToggle, probe, onChange, onEffortChange, onRemove }: {
  harness: WorkspaceHarness
  index: number
  expanded: boolean
  onToggle: () => void
  probe?: HarnessProbe
  onChange: (change: Partial<WorkspaceHarness>) => void
  onEffortChange: (effort: 'low' | 'medium' | 'high', value: string[]) => void
  onRemove: () => void
}) {
  const displayName = harness.name || `harness ${index + 1}`
  const environment = harness.mcp_transport === 'environment'
  const prompts = harness.command.filter((token) => token === '{prompt}').length
  const configs = harness.command.filter((token) => token === '{mcp_config}').length
  // Placeholder rules per spec §21.14 / §21.20; the server remains authoritative on save.
  const commandOk = prompts === 1 && (environment ? configs === 0 : configs === 1)
  const commandNote = environment
    ? 'Command argv requires one {prompt} and forbids {mcp_config}. The named registration must use child-environment URL and authorization references; literal credentials are rejected.'
    : 'Command argv requires exactly one {prompt} and one {mcp_config}.'
  return (
    <div className="rounded-md border border-border bg-card">
      <button
        type="button"
        aria-expanded={expanded}
        aria-label={`Toggle ${displayName}`}
        onClick={onToggle}
        className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-surface focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
      >
        <span className="font-mono text-sm font-semibold">{harness.name || <span className="font-sans font-normal italic text-faint">unnamed harness</span>}</span>
        <Badge>{TRANSPORT_LABELS[harness.mcp_transport ?? 'json_file']}</Badge>
        {probe ? (
          <Badge variant={probe.healthy ? 'positive' : 'attention'}>{probe.healthy ? 'Probe healthy' : `Probe failing${probe.message ? ` — ${probe.message}` : ''}`}</Badge>
        ) : (
          <span className="text-xs text-faint">No probe data yet</span>
        )}
        <ChevronRight className={cn('ml-auto size-4 shrink-0 text-faint transition-transform', expanded && 'rotate-90')} />
      </button>
      {expanded && (
        <div className="border-t border-border">
          <div className="grid gap-3 px-4 py-4 md:grid-cols-2">
            <Field label="Name"><Input aria-label={`Harness ${index + 1} name`} value={harness.name} onChange={(event) => onChange({ name: event.target.value })} /></Field>
            <Field label="MCP transport" hint="How the Conveyor work-order server is handed to the agent: Codex-style CLIs take a TOML --config override; Claude-style CLIs take a JSON file path (spec §21.20).">
              <Select
                aria-label="MCP transport"
                value={harness.mcp_transport ?? 'json_file'}
                onChange={(event) => {
                  const transport = event.target.value as WorkspaceHarness['mcp_transport']
                  onChange({ mcp_transport: transport, mcp_attachment: transport === 'environment' ? (harness.mcp_attachment || 'conveyor') : undefined })
                }}
              >
                <option value="json_file">JSON file</option>
                <option value="toml_override">TOML override</option>
                <option value="environment">Environment attachment</option>
              </Select>
            </Field>
            {environment && (
              <Field label="MCP attachment">
                <Input aria-label={`MCP attachment for ${displayName}`} value={harness.mcp_attachment ?? ''} placeholder="conveyor" onChange={(event) => onChange({ mcp_attachment: event.target.value })} />
              </Field>
            )}
            <div className="md:col-span-2">
              <Field label="Command argv" hint="Run as an argv array — no shell. Type or paste a command; it splits on spaces. Click an argument to edit it.">
                <ArgvInput label="Command argv" value={harness.command} onChange={(value) => onChange({ command: value })} />
              </Field>
              <p className={cn('mt-1.5 text-xs', commandOk ? 'text-faint' : 'text-failure')}>{commandNote}</p>
            </div>
            <Field label="Model argv" hint="Appended when a model is pinned; {model} is replaced with the model ID.">
              <ArgvInput label="Model argv" value={harness.model_args ?? []} onChange={(value) => onChange({ model_args: value })} placeholder="None" />
            </Field>
          </div>
          <details className="border-t border-border px-4 py-3">
            <summary className="cursor-pointer select-none text-sm font-medium text-muted hover:text-foreground">
              Advanced <span className="font-normal text-faint">— effort flags, model detection, health and stall supervision</span>
            </summary>
            <div className="grid gap-3 pb-1 pt-4 md:grid-cols-2">
              <Field label="Low effort argv"><ArgvInput label="Low effort argv" value={harness.effort_args?.low ?? []} onChange={(value) => onEffortChange('low', value)} placeholder="Not supported — effort Low is rejected for this harness" /></Field>
              <Field label="Medium effort argv"><ArgvInput label="Medium effort argv" value={harness.effort_args?.medium ?? []} onChange={(value) => onEffortChange('medium', value)} placeholder="Not supported" /></Field>
              <Field label="High effort argv"><ArgvInput label="High effort argv" value={harness.effort_args?.high ?? []} onChange={(value) => onEffortChange('high', value)} placeholder="Not supported" /></Field>
              <Field label="Default model sentinels" hint="Model names that mean “let the agent pick”. Leave empty unless the CLI has magic values.">
                <ArgvInput label="Default model sentinels" value={harness.default_model_sentinels ?? []} onChange={(value) => onChange({ default_model_sentinels: value })} placeholder="None" />
              </Field>
              <Field label="Probe argv" hint="Cheap health check the worker runs to confirm the CLI is installed and launchable.">
                <ArgvInput label="Probe argv" value={harness.probe_command} onChange={(value) => onChange({ probe_command: value })} />
              </Field>
              <Field label="Probe timeout"><Input className="max-w-28" value={harness.probe_timeout} onChange={(event) => onChange({ probe_timeout: event.target.value })} /></Field>
              <Field label="Stall timeout" hint="Stop a child after this long without stdout or stderr. Use 0 only for intentionally silent harnesses.">
                <Input className="max-w-28" value={harness.stall_timeout ?? '10m'} onChange={(event) => onChange({ stall_timeout: event.target.value })} />
              </Field>
            </div>
          </details>
          <div className="flex justify-end border-t border-border bg-surface px-4 py-2.5">
            <Button size="sm" variant="destructive" aria-label={`Remove ${displayName}`} onClick={onRemove}><Trash2 />Remove harness</Button>
          </div>
        </div>
      )}
    </div>
  )
}
