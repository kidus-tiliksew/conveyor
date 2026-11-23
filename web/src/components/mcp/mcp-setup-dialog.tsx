import claudeIcon from '@lobehub/icons-static-svg/icons/claude-color.svg?raw'
import codexIcon from '@lobehub/icons-static-svg/icons/codex.svg?raw'
import cursorIcon from '@lobehub/icons-static-svg/icons/cursor.svg?raw'
import opencodeIcon from '@lobehub/icons-static-svg/icons/opencode.svg?raw'
import { Cable, CheckCircle2, Plug, X } from 'lucide-react'
import { useMemo, useState } from 'react'
import { type MCPAuthMode, type MCPClient, mcpClientSetups, mcpEndpoint } from '../../lib/mcp'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { SegmentedControl } from '../ui/segmented-control'

const clientLogos: Partial<Record<MCPClient, string>> = {
  cursor: cursorIcon,
  opencode: opencodeIcon,
  claude: claudeIcon,
  codex: codexIcon,
}

// The Board header shows the trigger as an icon with the same accessible
// name; the Tasks header keeps the labelled button.
export function MCPSetup({ iconOnly = false }: { iconOnly?: boolean }) {
  const [open, setOpen] = useState(false)

  return (
    <>
      {iconOnly ? (
        <Button
          type="button"
          size="icon"
          variant="secondary"
          aria-label="MCP"
          title="MCP"
          onClick={() => setOpen(true)}
        >
          <Plug />
        </Button>
      ) : (
        <Button type="button" size="sm" variant="secondary" onClick={() => setOpen(true)}>
          <Cable />
          MCP
        </Button>
      )}
      {open && <MCPSetupDialog onClose={() => setOpen(false)} />}
    </>
  )
}

export function MCPSetupDialog({ onClose }: { onClose: () => void }) {
  const endpoint = mcpEndpoint(window.location.origin)
  // Browser sign-in is the default. The access token setup is the fallback
  // for a client without OAuth sign-in (component-web-dashboard invariant 26).
  const [mode, setMode] = useState<MCPAuthMode>('oauth')
  const clients = useMemo(() => mcpClientSetups(endpoint, mode), [endpoint, mode])
  const [selected, setSelected] = useState<MCPClient>('cursor')
  const setup = clients.find((client) => client.id === selected) ?? clients[0]

  return (
    <Dialog label="MCP Setup" onClose={onClose} className="max-w-2xl overflow-x-hidden">
      <div className="flex items-start justify-between gap-4 border-b border-border px-5 py-4">
        <div>
          <div className="flex items-center gap-2">
            <Cable className="size-5 text-primary" aria-hidden="true" />
            <h2 className="text-base font-semibold">MCP Setup</h2>
          </div>
          <p className="mt-1 text-sm leading-5 text-muted">
            Connect your coding client to Conveyor’s Model Context Protocol server.
          </p>
        </div>
        <Button type="button" variant="ghost" size="icon" aria-label="Close MCP Setup" onClick={onClose}>
          <X />
        </Button>
      </div>

      <div className="min-w-0 px-5 py-4">
        <div className="mb-4 space-y-2">
          <div className="flex items-center gap-2 rounded-md border border-border bg-background p-2">
            <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground" data-testid="mcp-endpoint">
              {endpoint}
            </code>
            <CopyButton value={endpoint} label="Copy MCP endpoint" />
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <SegmentedControl
              label="How the client signs in"
              value={mode}
              onChange={setMode}
              options={[
                { value: 'oauth', label: 'Browser sign-in' },
                { value: 'pat', label: 'Access token' },
              ]}
            />
            <p className="text-xs leading-5 text-muted">
              {mode === 'oauth'
                ? 'The client opens Conveyor in your browser. You approve it once.'
                : 'Use this for a client without OAuth sign-in. It needs an access token.'}
            </p>
          </div>
        </div>
        <div className="rounded-lg bg-surface p-1" role="tablist" aria-label="MCP clients">
          <div className="grid grid-cols-2 gap-1 sm:grid-cols-3 md:grid-cols-5">
            {clients.map((client) => {
              const logo = clientLogos[client.id]
              return (
                <button
                  key={client.id}
                  type="button"
                  role="tab"
                  aria-selected={client.id === selected}
                  aria-controls="mcp-client-panel"
                  className={cn(
                    'flex h-9 items-center justify-center gap-2 rounded-md px-3 text-xs font-medium text-muted transition-colors focus-visible:outline-2 focus-visible:outline-primary',
                    client.id === selected && 'bg-background text-foreground shadow-sm',
                  )}
                  onClick={() => setSelected(client.id)}
                >
                  {logo ? (
                    <span
                      aria-hidden="true"
                      data-mcp-client-logo={client.id}
                      className="inline-flex size-4 shrink-0 [&_svg]:size-4"
                      dangerouslySetInnerHTML={{ __html: logo }}
                    />
                  ) : (
                    <Cable aria-hidden="true" data-mcp-client-fallback className="size-4 shrink-0" />
                  )}
                  {client.label}
                </button>
              )
            })}
          </div>
        </div>

        <div id="mcp-client-panel" role="tabpanel" className="mt-5 min-w-0">
          <p className="text-sm font-medium text-foreground">{setup.description}</p>
          <ol className="mt-3 list-decimal space-y-1 pl-5 text-sm leading-6 text-muted">
            {setup.steps.map((step) => (
              <li key={step}>{step}</li>
            ))}
          </ol>
          {setup.installerNote && (
            <p
              className="mt-3 rounded-md border border-border bg-surface px-3 py-2 text-xs leading-5 text-muted"
              data-testid="mcp-installer-note"
            >
              {setup.installerNote}
            </p>
          )}

          <div className="mt-5 flex items-center justify-between gap-3">
            <p className="text-xs font-medium text-muted">{setup.snippetLabel}</p>
            <CopyButton value={setup.snippet} label={`Copy ${setup.label} setup`} showLabel />
          </div>
          <pre className="mt-2 max-h-56 overflow-auto rounded-lg bg-surface p-4 font-mono text-xs leading-5 text-foreground">
            <code>{setup.snippet}</code>
          </pre>
        </div>
      </div>

      <div className="flex justify-end border-t border-border px-5 py-3">
        <Button type="button" onClick={onClose}>
          <CheckCircle2 />
          Done
        </Button>
      </div>
    </Dialog>
  )
}
