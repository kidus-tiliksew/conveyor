// Model chip logo selection. The model's provider wins; when the model names
// no known provider (for example Cursor Auto reports `auto`), the claim's
// harness identity supplies the logo; otherwise the caller renders a generic
// icon. The model text itself is never rewritten.

export type ProviderLogo = 'openai' | 'claude' | 'gemini' | 'grok' | 'zhipu'
export type HarnessLogo = 'cursor' | 'claude-code' | 'codex' | 'opencode' | 'windsurf' | 'github-copilot'

export type ModelLogo =
  | { source: 'provider'; logo: ProviderLogo }
  | { source: 'harness'; logo: HarnessLogo; harness: string }

export function providerLogoFor(model: string): ProviderLogo | undefined {
  const name = model.toLowerCase()
  if (/^(gpt|o\d|codex|davinci)/.test(name) || name.includes('openai')) return 'openai'
  if (/claude|fable|opus|sonnet|haiku|anthropic/.test(name)) return 'claude'
  if (/gemini|google/.test(name)) return 'gemini'
  if (/grok|xai|x\.ai/.test(name)) return 'grok'
  if (/(^|[/:])(?:chatglm|glm|zhipu)(?=$|[-/.:\d])/.test(name)) return 'zhipu'
  return undefined
}

const harnesses: Record<HarnessLogo, { name: string; aliases: string[] }> = {
  cursor: { name: 'Cursor', aliases: ['cursor', 'cursoragent', 'cursorcli'] },
  'claude-code': { name: 'Claude Code', aliases: ['claudecode', 'claude'] },
  codex: { name: 'Codex', aliases: ['codex', 'codexcli', 'openaicodex'] },
  opencode: { name: 'OpenCode', aliases: ['opencode'] },
  windsurf: { name: 'Windsurf', aliases: ['windsurf'] },
  'github-copilot': { name: 'GitHub Copilot', aliases: ['githubcopilot', 'copilot'] },
}

const harnessByAlias = new Map(
  (Object.entries(harnesses) as [HarnessLogo, { name: string; aliases: string[] }][]).flatMap(([logo, harness]) =>
    harness.aliases.map((alias) => [alias, { logo, name: harness.name }] as const),
  ),
)

// Agent names match case-insensitively and ignore separators, so
// `claude-code`, `Claude Code`, and `claude_code` name the same harness.
// Only the explicit alias table matches; unknown names fall through.
export function harnessFor(agent?: string): { logo: HarnessLogo; name: string } | undefined {
  if (!agent) return undefined
  return harnessByAlias.get(agent.toLowerCase().replace(/[^a-z0-9]/g, ''))
}

export function selectModelLogo(model: string, agent?: string): ModelLogo | undefined {
  const provider = providerLogoFor(model)
  if (provider) return { source: 'provider', logo: provider }
  const harness = harnessFor(agent)
  if (harness) return { source: 'harness', logo: harness.logo, harness: harness.name }
  return undefined
}
