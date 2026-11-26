import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useId, useState } from 'react'
import { createEnvironment, EnvironmentRequestError, updateEnvironment } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { EnvironmentInput, EnvironmentRecord, EnvironmentVersionRuleKind } from '../../lib/types'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { Input, Select, Textarea } from '../ui/input'

// The fields a refusal can name. A refusal that names another field, or none,
// shows at the top of the form (component-environments invariants 3 to 7).
type FormField = 'name' | 'tracked_branches' | 'github_environments' | 'version_rule' | 'timing_window_minutes'
const formFields: readonly string[] = [
  'name',
  'tracked_branches',
  'github_environments',
  'version_rule',
  'timing_window_minutes',
]

/** Values that open the create form, from a suggested GitHub environment name. */
export interface EnvironmentFormSeed {
  name: string
  githubEnvironment: string
}

/**
 * Creates or edits one environment (req-environments AC-1.1 to AC-1.5;
 * component-environments invariant 18). Tracked branches offer one optional
 * branch field for each repository of the workspace configuration, and for a
 * repository the environment already tracks. A refusal keeps every value the
 * person typed and shows the server's message beside the field it names. The
 * request uses the workspace the dialog opened in, and success refreshes only
 * that workspace's environment reads. Nothing here changes the workspace
 * configuration draft.
 */
export function EnvironmentFormDialog({
  workspace,
  repositories,
  environment,
  seed,
  onClose,
}: {
  workspace: string
  repositories: string[]
  environment?: EnvironmentRecord
  seed?: EnvironmentFormSeed
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const baseId = useId()
  const editing = environment !== undefined
  const [name, setName] = useState(environment?.name ?? seed?.name ?? '')
  const [branches, setBranches] = useState<Record<string, string>>(() => ({ ...(environment?.tracked_branches ?? {}) }))
  const [githubNames, setGitHubNames] = useState(
    (environment?.github_environments ?? (seed ? [seed.githubEnvironment] : [])).join('\n'),
  )
  const [ruleKind, setRuleKind] = useState<EnvironmentVersionRuleKind | ''>(environment?.version_rule?.kind ?? '')
  const [ruleText, setRuleText] = useState(environment?.version_rule?.text ?? '')
  const [windowText, setWindowText] = useState(environment ? String(environment.timing_window_minutes) : '')
  const repositoryNames = [...new Set([...repositories, ...Object.keys(environment?.tracked_branches ?? {})])]

  const body = (): EnvironmentInput => {
    const input: EnvironmentInput = {
      name,
      tracked_branches: Object.fromEntries(
        Object.entries(branches)
          .map(([repository, branch]) => [repository, branch.trim()] as const)
          .filter(([, branch]) => branch !== ''),
      ),
      github_environments: githubNames
        .split('\n')
        .map((line) => line.trim())
        .filter(Boolean),
    }
    if (ruleKind) input.version_rule = { kind: ruleKind, text: ruleText }
    else if (editing) input.version_rule = null
    // A blank window is left out: a new environment takes 60 minutes, and an
    // edit keeps the current window (req-environments AC-1.3). The server
    // judges every other value.
    if (windowText.trim() !== '') input.timing_window_minutes = Number(windowText.trim())
    return input
  }

  const save = useMutation({
    mutationFn: (input: EnvironmentInput) =>
      environment ? updateEnvironment(workspace, environment.id, input) : createEnvironment(workspace, input),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['environments', workspace] })
      onClose()
    },
  })
  const refusal = save.error instanceof EnvironmentRequestError ? save.error : undefined
  const refusedField = refusal && formFields.includes(refusal.field) ? (refusal.field as FormField) : undefined
  const fieldError = (field: FormField) =>
    refusedField === field ? (
      <p id={`${baseId}-${field}-error`} role="alert" className="mt-1 text-xs text-failure">
        {refusal?.message}
      </p>
    ) : null
  const describedBy = (field: FormField) => (refusedField === field ? `${baseId}-${field}-error` : undefined)
  const close = () => {
    if (!save.isPending) onClose()
  }

  return (
    <Dialog label={editing ? 'Edit environment' : 'Add environment'} onClose={close} className="max-w-lg">
      <form
        className="max-h-[85vh] space-y-4 overflow-y-auto p-5"
        aria-label={editing ? 'Edit environment' : 'Add environment'}
        onSubmit={(event) => {
          event.preventDefault()
          save.mutate(body())
        }}
      >
        <h2 className="text-base font-semibold">{editing ? `Edit ${environment.name}` : 'Add environment'}</h2>
        {save.error && !refusedField && (
          <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(save.error)}
          </p>
        )}
        <div>
          <label htmlFor={`${baseId}-name`} className="mb-1 block text-xs font-medium text-muted">
            Name
          </label>
          <Input
            id={`${baseId}-name`}
            value={name}
            onChange={(event) => setName(event.target.value)}
            maxLength={200}
            autoComplete="off"
            aria-invalid={refusedField === 'name' || undefined}
            aria-describedby={describedBy('name')}
          />
          {fieldError('name')}
        </div>

        <fieldset aria-describedby={describedBy('tracked_branches')}>
          <legend className="mb-1 text-xs font-medium text-muted">Tracked branches</legend>
          <p className="mb-2 text-xs text-faint">
            Each repository can name the branch it deploys from to this environment. Leave a field empty to track no
            branch for that repository.
          </p>
          {repositoryNames.length === 0 && <p className="text-xs text-muted">This workspace has no repositories.</p>}
          <div className="space-y-2">
            {repositoryNames.map((repository) => (
              <div key={repository} className="grid gap-1 sm:grid-cols-[10rem_minmax(0,1fr)] sm:items-center sm:gap-3">
                <label htmlFor={`${baseId}-branch-${repository}`} className="truncate text-sm">
                  {repository}
                </label>
                <Input
                  id={`${baseId}-branch-${repository}`}
                  value={branches[repository] ?? ''}
                  onChange={(event) => setBranches((current) => ({ ...current, [repository]: event.target.value }))}
                  placeholder="No branch"
                  autoComplete="off"
                  spellCheck={false}
                  className="font-mono"
                  aria-invalid={refusedField === 'tracked_branches' || undefined}
                />
              </div>
            ))}
          </div>
          {fieldError('tracked_branches')}
        </fieldset>

        <div>
          <label htmlFor={`${baseId}-github`} className="mb-1 block text-xs font-medium text-muted">
            GitHub environment names
          </label>
          <p className="mb-2 text-xs text-faint">
            Write one name on each line, at most ten. GitHub reports deployments under these names.
          </p>
          <Textarea
            id={`${baseId}-github`}
            value={githubNames}
            onChange={(event) => setGitHubNames(event.target.value)}
            rows={3}
            spellCheck={false}
            aria-invalid={refusedField === 'github_environments' || undefined}
            aria-describedby={describedBy('github_environments')}
          />
          {fieldError('github_environments')}
        </div>

        <fieldset aria-describedby={describedBy('version_rule')}>
          <legend className="mb-1 text-xs font-medium text-muted">Version rule</legend>
          <p className="mb-2 text-xs text-faint">
            A commit pattern holds one {'{commit}'}. A tag template holds one {'{version}'} and can hold one{' '}
            {'{service}'}.
          </p>
          <div className="grid gap-2 sm:grid-cols-[10rem_minmax(0,1fr)]">
            <Select
              aria-label="Version rule kind"
              value={ruleKind}
              onChange={(event) => setRuleKind(event.target.value as EnvironmentVersionRuleKind | '')}
            >
              <option value="">No rule</option>
              <option value="commit_pattern">Commit pattern</option>
              <option value="tag_template">Tag template</option>
            </Select>
            <Input
              aria-label="Version rule text"
              value={ruleText}
              onChange={(event) => setRuleText(event.target.value)}
              disabled={!ruleKind}
              placeholder={ruleKind === 'tag_template' ? 'v{version}' : 'build-{commit}'}
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              aria-invalid={refusedField === 'version_rule' || undefined}
            />
          </div>
          {fieldError('version_rule')}
        </fieldset>

        <div>
          <label htmlFor={`${baseId}-window`} className="mb-1 block text-xs font-medium text-muted">
            Timing window in minutes
          </label>
          <p className="mb-2 text-xs text-faint">
            How long after a deploy a problem still counts as following it. Use 1 to 1440.
            {editing ? '' : ' Leave it empty to use 60.'}
          </p>
          <Input
            id={`${baseId}-window`}
            value={windowText}
            onChange={(event) => setWindowText(event.target.value)}
            inputMode="numeric"
            autoComplete="off"
            className="w-32"
            aria-invalid={refusedField === 'timing_window_minutes' || undefined}
            aria-describedby={describedBy('timing_window_minutes')}
          />
          {fieldError('timing_window_minutes')}
        </div>

        <div className="flex justify-end gap-2 pt-1">
          <Button type="button" variant="outline" disabled={save.isPending} onClick={close}>
            Cancel
          </Button>
          <Button type="submit" disabled={save.isPending}>
            {save.isPending ? 'Saving…' : editing ? 'Save environment' : 'Add environment'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
