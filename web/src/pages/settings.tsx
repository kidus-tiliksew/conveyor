import { useQuery } from '@tanstack/react-query'
import { useLocation, useNavigate, useSearch } from '@tanstack/react-router'
import { CheckCircle2 } from 'lucide-react'
import { useEffect } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { BoardPreferencesCard } from '../components/settings/board-preferences-card'
import { ConnectAgentRow } from '../components/settings/connect-agent-row'
import { PasswordCard } from '../components/settings/password-card'
import { PersonalTokensCard } from '../components/settings/personal-tokens-card'
import { ProfileCard } from '../components/settings/profile-card'
import { SettingsGroup } from '../components/settings/settings-group'
import { ThemeRow } from '../components/settings/theme-row'
import { WorkspaceGitHubAppCard } from '../components/settings/workspace-github-app-card'
import { StagePreferencesCard } from '../components/stage-preferences/stage-preferences-card'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { CopyButton } from '../components/ui/copy-button'
import { PageHeader } from '../components/ui/page-header'
import { fetchWorkspaces } from '../lib/api'
import { mcpConnectionConfig, mcpEndpoint } from '../lib/mcp'

export function SettingsPage() {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const searchStr = useLocation({ select: (location) => location.searchStr })
  const returnedWorkspace = new URLSearchParams(searchStr).get('workspace')
  const workspaces = useQuery({ queryKey: ['workspaces'], queryFn: fetchWorkspaces })
  const returnWorkspaceKnown = workspaces.data?.some((item) => item.id === returnedWorkspace)
  const { welcome } = useSearch({ from: '/w/$workspaceId/settings' })
  // A GitHub App return from before workspace addresses names its workspace
  // in `?workspace=`; open that workspace's Settings in place of this one.
  useEffect(() => {
    if (returnedWorkspace && returnWorkspaceKnown && returnedWorkspace !== workspace) {
      void navigate({
        to: '/w/$workspaceId/settings',
        params: { workspaceId: returnedWorkspace },
        search: { welcome },
        replace: true,
      })
    }
  }, [returnedWorkspace, returnWorkspaceKnown, workspace, navigate, welcome])
  const cliCommand = 'export CONVEYOR_API_TOKEN="<paste-your-token>"'
  const endpoint = mcpEndpoint(window.location.origin)
  const canManage = useWorkspaceCapability('manage_workspace')
  const workspaceName = workspaces.data?.find((item) => item.id === workspace)?.name || workspace
  const mcpConfig = mcpConnectionConfig(endpoint).replace('<CONVEYOR_API_TOKEN>', '<paste-your-token>')
  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader title="Settings" description="Your access and connection settings." />
      <div className="min-h-0 flex-1 overflow-y-auto" data-testid="settings-scroll">
        <div className="mx-auto max-w-4xl px-4 py-6 sm:px-6 sm:py-8">
          {welcome && (
            <Card className="mb-8 border-primary/30 bg-primary-soft/30">
              <CardHeader>
                <CardTitle>Welcome to Conveyor</CardTitle>
                <CheckCircle2 className="size-4 text-positive" />
              </CardHeader>
              <CardContent className="space-y-4 text-sm leading-6 text-muted">
                <p>
                  You’re signed in. Create your first access token below, copy it when it appears, then use these
                  details to connect your command line or coding agent.
                </p>
                <div className="space-y-2">
                  <p className="font-medium text-foreground">Command line</p>
                  <div className="flex items-center gap-2 rounded-md border border-border bg-card p-2">
                    <code className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">
                      {cliCommand}
                    </code>
                    <CopyButton value={cliCommand} label="Copy command" />
                  </div>
                </div>
                <div className="space-y-2">
                  <p className="font-medium text-foreground">Coding-agent connection</p>
                  <div className="flex items-start gap-2 rounded-md border border-border bg-card p-2">
                    <pre className="min-w-0 flex-1 overflow-x-auto font-mono text-xs text-foreground">{mcpConfig}</pre>
                    <CopyButton value={mcpConfig} label="Copy connection settings" />
                  </div>
                </div>
              </CardContent>
            </Card>
          )}

          <div className="space-y-8 lg:space-y-10">
            <SettingsGroup title="Account" description="How you sign in and how teammates see you.">
              <ProfileCard />
              <PasswordCard />
              <PersonalTokensCard defaultOpen={Boolean(welcome)} />
            </SettingsGroup>
            <SettingsGroup
              title="Agents"
              description="How your coding-agent sessions reach Conveyor and what they start."
            >
              <ConnectAgentRow endpoint={endpoint} />
              <StagePreferencesCard />
            </SettingsGroup>
            <SettingsGroup title="This browser" description="Display choices saved on this device only.">
              <ThemeRow />
              <BoardPreferencesCard />
            </SettingsGroup>
            {canManage && (!returnWorkspaceKnown || returnedWorkspace === workspace) && (
              <SettingsGroup
                title={`Workspace: ${workspaceName}`}
                description="Shared by everyone in this workspace. Only people who manage the workspace see this."
              >
                <WorkspaceGitHubAppCard key={workspace} />
              </SettingsGroup>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
