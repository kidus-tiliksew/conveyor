import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useRef, useState } from 'react'
import { EnvironmentRequestError, issueEnvironmentKey, revokeEnvironmentKey } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { EnvironmentKey, EnvironmentRecord } from '../../lib/types'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'

function keyLimitMessage(error: unknown) {
  return error instanceof EnvironmentRequestError && error.code === 'environment_key_limit'
    ? 'This environment already has five keys. Revoke a key before you issue another.'
    : errorMessage(error)
}

/**
 * Issues a key and shows its value once (req-environments AC-2.1, AC-2.3;
 * component-environments invariants 11, 12, and 18). The value lives only in
 * this dialog's state: the mutation stores it here and resolves with no value,
 * so neither the Query cache nor the mutation cache holds it, and nothing
 * writes it to browser storage, the address, or a log. Closing or unmounting
 * the dialog discards it. The panel is keyed by workspace, so a workspace
 * change unmounts the dialog, and an answer that arrives after that is
 * dropped. A refusal because another manager reached the five-key limit first
 * says so, and the list reads again.
 */
export function IssueKeyDialog({
  workspace,
  environment,
  onClose,
}: {
  workspace: string
  environment: EnvironmentRecord
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const labelId = useId()
  const valueId = useId()
  const [label, setLabel] = useState('')
  const [value, setValue] = useState('')
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const issue = useMutation({
    mutationFn: async (keyLabel: string): Promise<void> => {
      const issued = await issueEnvironmentKey(workspace, environment.id, keyLabel)
      if (mounted.current) setValue(issued.value)
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ['environments', workspace] }),
  })
  // Closing never waits for the request: an answer that arrives after the
  // dialog closed finds it unmounted and shows nothing.
  const close = () => {
    setValue('')
    onClose()
  }
  return (
    <Dialog label="Issue environment key" onClose={close}>
      <form
        className="space-y-4 p-5"
        aria-label="Issue environment key"
        onSubmit={(event) => {
          event.preventDefault()
          if (!value) issue.mutate(label)
        }}
      >
        <h2 className="text-base font-semibold">Issue a key for {environment.name}</h2>
        {!value && (
          <>
            <p className="text-sm leading-6 text-muted">
              A watcher sends signals for this environment with the key. Give the key a label that names the watcher.
            </p>
            <div>
              <label htmlFor={labelId} className="mb-1 block text-xs font-medium text-muted">
                Label
              </label>
              <Input
                id={labelId}
                value={label}
                onChange={(event) => setLabel(event.target.value)}
                maxLength={120}
                autoComplete="off"
              />
            </div>
          </>
        )}
        {value && (
          <div className="space-y-2 text-sm">
            <label htmlFor={valueId} className="font-medium">
              Key for {label.trim() || environment.name}
            </label>
            <div className="flex items-center gap-2">
              <Input
                id={valueId}
                readOnly
                value={value}
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
                data-environment-key-value
              />
              <CopyButton value={value} label="Copy key" />
            </div>
            <p className="leading-6 text-muted">
              Conveyor will not show this key again. Copy it now and give it to the watcher.
            </p>
          </div>
        )}
        {issue.error && (
          <p className="text-sm text-failure" role="alert">
            {keyLimitMessage(issue.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          {value ? (
            <Button type="button" onClick={close}>
              Done
            </Button>
          ) : (
            <>
              <Button type="button" variant="outline" onClick={close}>
                Cancel
              </Button>
              <Button type="submit" disabled={issue.isPending || !label.trim()}>
                {issue.isPending ? 'Issuing…' : 'Issue key'}
              </Button>
            </>
          )}
        </div>
      </form>
    </Dialog>
  )
}

/**
 * Revokes one key after a confirmation that names its label
 * (req-environments AC-2.2; component-environments invariant 13).
 */
export function RevokeKeyDialog({
  workspace,
  environment,
  environmentKey,
  onClose,
}: {
  workspace: string
  environment: EnvironmentRecord
  environmentKey: EnvironmentKey
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const revoke = useMutation({
    mutationFn: () => revokeEnvironmentKey(workspace, environment.id, environmentKey.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['environments', workspace] })
      onClose()
    },
  })
  const close = () => {
    if (!revoke.isPending) onClose()
  }
  return (
    <Dialog label={`Revoke key ${environmentKey.label}`} onClose={close}>
      <div className="space-y-4 p-5">
        <h2 className="text-base font-semibold">Revoke the key “{environmentKey.label}”?</h2>
        <p className="text-sm leading-6 text-muted">
          Conveyor will refuse this key the next time a watcher uses it for {environment.name}. Everything that the
          watcher sent before stays.
        </p>
        {revoke.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(revoke.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={revoke.isPending} onClick={close}>
            Cancel
          </Button>
          <Button type="button" variant="danger" disabled={revoke.isPending} onClick={() => revoke.mutate()}>
            {revoke.isPending ? 'Revoking…' : 'Revoke key'}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
