import { expect, test } from '@playwright/test'
import { harnessFor, type ModelLogo, providerLogoFor, selectModelLogo } from '../src/lib/model-logo'

const providerModels = [
  ['gpt-5.5', 'openai'],
  ['o4-mini', 'openai'],
  ['codex-mini', 'openai'],
  ['davinci-002', 'openai'],
  ['azure/openai-model', 'openai'],
  ['claude-opus-5-5', 'claude'],
  ['claude-fable-5-1', 'claude'],
  ['sonnet', 'claude'],
  ['Haiku-4.5', 'claude'],
  ['anthropic/model', 'claude'],
  ['gemini-2.5-pro', 'gemini'],
  ['google/model', 'gemini'],
  ['grok-4', 'grok'],
  ['xai/model', 'grok'],
  ['opencode-go/glm-5.3-flash', 'zhipu'],
  ['OpenCode-Go/GLM-5.3-FLASH', 'zhipu'],
  ['chatglm3', 'zhipu'],
  ['zhipu/model', 'zhipu'],
] as const

const harnessAgents = [
  ['cursor', 'cursor', 'Cursor'],
  ['Cursor', 'cursor', 'Cursor'],
  ['cursor-agent', 'cursor', 'Cursor'],
  ['claude-code', 'claude-code', 'Claude Code'],
  ['Claude Code', 'claude-code', 'Claude Code'],
  ['claude_code', 'claude-code', 'Claude Code'],
  ['claude', 'claude-code', 'Claude Code'],
  ['codex', 'codex', 'Codex'],
  ['CODEX', 'codex', 'Codex'],
  ['codex-cli', 'codex', 'Codex'],
  ['opencode', 'opencode', 'OpenCode'],
  ['open-code', 'opencode', 'OpenCode'],
  ['OpenCode', 'opencode', 'OpenCode'],
  ['windsurf', 'windsurf', 'Windsurf'],
  ['github-copilot', 'github-copilot', 'GitHub Copilot'],
  ['githubcopilot', 'github-copilot', 'GitHub Copilot'],
  ['copilot', 'github-copilot', 'GitHub Copilot'],
] as const

test.describe('model chip logo selection', () => {
  for (const [model, provider] of providerModels) {
    test(`provider ${provider} wins for ${model} under every harness`, () => {
      expect(providerLogoFor(model)).toBe(provider)
      for (const agent of [undefined, '', 'unknown-harness', ...harnessAgents.map(([alias]) => alias)]) {
        expect(selectModelLogo(model, agent)).toEqual({ source: 'provider', logo: provider } satisfies ModelLogo)
      }
    })
  }

  for (const [agent, logo, harness] of harnessAgents) {
    test(`harness ${agent} supplies the ${logo} logo when the model names no provider`, () => {
      expect(harnessFor(agent)).toEqual({ logo, name: harness })
      for (const model of ['auto', 'Auto', 'unknown/model', '—']) {
        expect(selectModelLogo(model, agent)).toEqual({ source: 'harness', logo, harness } satisfies ModelLogo)
      }
    })
  }

  test('cursor with a Claude model keeps the Claude logo; cursor with auto shows Cursor', () => {
    expect(selectModelLogo('claude-opus-5-5', 'cursor')).toEqual({ source: 'provider', logo: 'claude' })
    expect(selectModelLogo('auto', 'cursor')).toEqual({ source: 'harness', logo: 'cursor', harness: 'Cursor' })
  })

  for (const agent of [undefined, '', '   ', 'unknown-harness', 'aider', 'cursorx', 'my-claude-fork']) {
    test(`unknown or absent harness ${JSON.stringify(agent)} with an unknown model selects no logo`, () => {
      expect(harnessFor(agent)).toBeUndefined()
      for (const model of ['auto', 'unknown/model', 'unknown/notglm-5', 'unknown/glmodel']) {
        expect(selectModelLogo(model, agent)).toBeUndefined()
      }
    })
  }
})
