import { dateTimeLabel } from '../../lib/utils'
import { useMutation } from '@tanstack/react-query'
import { ExternalLink } from 'lucide-react'
import { useId, useState } from 'react'
import { createWorkspaceGitHubWebhookSecret } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { WorkspaceGitHubWebhookStatus } from '../../lib/types'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'

// GitHub's App settings name each subscribable event by these labels.
const eventLabels: Record<string, string> = {
  pull_request: 'Pull request',
  check_suite: 'Check suite',
  check_run: 'Check run',
  status: 'Status',
  push: 'Push',
  issues: 'Issues',
}

function LocalTime({ value }: { value: string }) {
  return <time dateTime={value}>{dateTimeLabel(value)}</time>
}

/**
 * Webhook delivery setup for the workspace GitHub App (component-web-dashboard,
 * Webhook panel; req-quiet-idle-runtime AC-3.6; DEC-99(5)). GitHub lets only an
 * App's owner change its webhook, so for an App created before webhook
 * delivery the panel lists what to enter in the App's GitHub settings.
 */
export function WorkspaceGitHubWebhookPanel({
  workspace,
  appSlug,
  installationAccount,
  webhook,
  onSecretGenerated,
}: {
  workspace: string
  appSlug: string
  installationAccount: string
  webhook: WorkspaceGitHubWebhookStatus
  onSecretGenerated: () => void
}) {
  const receiverID = useId()
  const [generating, setGenerating] = useState(false)
  // The status names the App's slug but not its owner, so both settings
  // addresses are offered when an installation account is known.
  const slug = encodeURIComponent(appSlug)
  const settingsLinks = [
    { label: 'App settings on a personal account', href: `https://github.com/settings/apps/${slug}` },
  ]
  if (installationAccount)
    settingsLinks.push({
      label: `App settings on organization ${installationAccount}`,
      href: `https://github.com/organizations/${encodeURIComponent(installationAccount)}/settings/apps/${slug}`,
    })
  return (
    <section aria-label="Webhook delivery" className="space-y-4 border-t border-border pt-4">
      <div className="space-y-1">
        <h3 className="text-sm font-medium">Webhook delivery</h3>
        <p className={webhook.configured ? 'text-sm text-positive' : 'text-sm text-attention'}>
          {webhook.configured ? 'Webhook delivery is configured' : 'Webhook delivery is not configured'}
        </p>
      </div>
      <dl className="grid gap-x-4 gap-y-2 text-sm sm:grid-cols-[max-content_1fr]">
        <dt className="text-muted">Last accepted delivery</dt>
        <dd data-testid="webhook-last-delivery">
          {webhook.last_delivery_at ? <LocalTime value={webhook.last_delivery_at} /> : 'No delivery accepted yet'}
        </dd>
        <dt className="text-muted">Secret generated</dt>
        <dd data-testid="webhook-secret-set">
          {webhook.secret_configured && webhook.secret_set_at ? (
            <LocalTime value={webhook.secret_set_at} />
          ) : (
            'No secret generated yet'
          )}
        </dd>
      </dl>
      {webhook.receiver_url ? (
        <div className="space-y-1 text-sm">
          <label htmlFor={receiverID} className="text-muted">
            Receiver URL
          </label>
          <div className="flex items-center gap-2">
            <Input
              id={receiverID}
              readOnly
              value={webhook.receiver_url}
              autoComplete="off"
              spellCheck={false}
              className="max-w-xl"
            />
            <CopyButton value={webhook.receiver_url} label="Copy receiver URL" />
          </div>
        </div>
      ) : (
        <p className="text-sm text-attention" role="status">
          This deployment needs <code>CONVEYOR_PUBLIC_URL</code> before GitHub can deliver webhooks.
        </p>
      )}
      <div className="space-y-1 text-sm">
        <p className="text-muted">Events to subscribe</p>
        <ul className="flex flex-wrap gap-2" aria-label="Webhook events">
          {webhook.events.map((event) => (
            <li key={event} className="rounded-md border border-border px-2 py-1">
              {eventLabels[event] ?? event} <code className="text-faint">{event}</code>
            </li>
          ))}
        </ul>
      </div>
      <div className="space-y-2 text-sm">
        <p className="text-muted">Set up webhook delivery for an existing App</p>
        <ol className="list-decimal space-y-1 pl-5 leading-6" aria-label="Webhook setup steps">
          <li>
            Open the App’s GitHub settings:{' '}
            {settingsLinks.map((link, index) => (
              <span key={link.href}>
                {index > 0 && ' or '}
                <a
                  className="inline-flex items-center gap-1 text-primary underline"
                  href={link.href}
                  target="_blank"
                  rel="noreferrer"
                >
                  {link.label}
                  <ExternalLink className="size-3" aria-hidden />
                </a>
              </span>
            ))}
            .
          </li>
          <li>Under Webhook, turn on Active.</li>
          <li>Enter the receiver URL as the Webhook URL and the generated secret as the Webhook secret, then save.</li>
          <li>
            Under Permissions &amp; events, grant Checks read-only access if the App lacks it, then subscribe the events
            listed above.
          </li>
        </ol>
      </div>
      <Button type="button" variant="outline" onClick={() => setGenerating(true)}>
        Generate secret
      </Button>
      {generating && (
        <WebhookSecretDialog
          workspace={workspace}
          replacesSecret={webhook.secret_configured}
          onGenerated={onSecretGenerated}
          onClose={() => setGenerating(false)}
        />
      )}
    </section>
  )
}

/**
 * Generates the webhook secret and shows it once. The secret lives only in
 * this dialog's state: the mutation stores it here and resolves with no value,
 * so neither the Query cache nor the mutation cache holds it, and unmounting
 * the dialog discards it (component-web-dashboard, Webhook panel).
 */
function WebhookSecretDialog({
  workspace,
  replacesSecret,
  onGenerated,
  onClose,
}: {
  workspace: string
  replacesSecret: boolean
  onGenerated: () => void
  onClose: () => void
}) {
  const secretID = useId()
  const [secret, setSecret] = useState('')
  const generate = useMutation({
    mutationFn: async (): Promise<void> => {
      const generated = await createWorkspaceGitHubWebhookSecret(workspace)
      setSecret(generated.secret)
    },
    onSuccess: () => onGenerated(),
    // A failed request shows the server's error and no secret.
    onError: () => setSecret(''),
  })
  const close = () => {
    if (!generate.isPending) onClose()
  }
  return (
    <Dialog label="Generate webhook secret" onClose={close}>
      <div className="space-y-4 p-5">
        <h2 className="text-base font-semibold">Generate webhook secret</h2>
        {!secret && replacesSecret && (
          <p className="text-sm leading-6 text-muted">
            A new secret replaces the current one. GitHub deliveries signed with the old secret are refused until the
            App’s GitHub settings hold the new one.
          </p>
        )}
        {!secret && !replacesSecret && (
          <p className="text-sm leading-6 text-muted">
            GitHub signs each webhook delivery with this secret. Enter it in the App’s GitHub settings.
          </p>
        )}
        {secret && (
          <div className="space-y-2 text-sm">
            <label htmlFor={secretID} className="font-medium">
              Webhook secret
            </label>
            <div className="flex items-center gap-2">
              <Input
                id={secretID}
                readOnly
                value={secret}
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
              />
              <CopyButton value={secret} label="Copy webhook secret" />
            </div>
            <p className="leading-6 text-muted">
              Conveyor will not show this secret again. Enter it in the App’s GitHub settings now.
            </p>
          </div>
        )}
        {generate.error && (
          <p className="text-sm text-failure" role="alert">
            {errorMessage(generate.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          {secret ? (
            <>
              <Button type="button" variant="outline" disabled={generate.isPending} onClick={() => generate.mutate()}>
                {generate.isPending ? 'Generating…' : 'Generate again'}
              </Button>
              <Button type="button" disabled={generate.isPending} onClick={close}>
                Done
              </Button>
            </>
          ) : (
            <>
              <Button type="button" variant="outline" disabled={generate.isPending} onClick={close}>
                Cancel
              </Button>
              <Button type="button" disabled={generate.isPending} onClick={() => generate.mutate()}>
                {generate.isPending ? 'Generating…' : 'Generate'}
              </Button>
            </>
          )}
        </div>
      </div>
    </Dialog>
  )
}
