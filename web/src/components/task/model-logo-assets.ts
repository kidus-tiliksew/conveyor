import claudeIcon from '@lobehub/icons-static-svg/icons/claude-color.svg?raw'
import claudeCodeIcon from '@lobehub/icons-static-svg/icons/claudecode-color.svg?raw'
import codexIcon from '@lobehub/icons-static-svg/icons/codex-color.svg?raw'
import cursorIcon from '@lobehub/icons-static-svg/icons/cursor.svg?raw'
import geminiIcon from '@lobehub/icons-static-svg/icons/gemini-color.svg?raw'
import githubCopilotIcon from '@lobehub/icons-static-svg/icons/githubcopilot.svg?raw'
import grokIcon from '@lobehub/icons-static-svg/icons/grok.svg?raw'
import openaiIcon from '@lobehub/icons-static-svg/icons/openai.svg?raw'
import opencodeIcon from '@lobehub/icons-static-svg/icons/opencode.svg?raw'
import windsurfIcon from '@lobehub/icons-static-svg/icons/windsurf.svg?raw'
import zhipuIcon from '@lobehub/icons-static-svg/icons/zhipu-color.svg?raw'
import type { HarnessLogo, ProviderLogo } from '../../lib/model-logo'

// Bundled logo SVGs (no network fetch), shared by the Timeline's model chip
// and the next-action banner's model logo (DEC-129(2)). Monochrome icons
// inherit the foreground color; selection order lives in lib/model-logo.
export const logoAssets: Record<ProviderLogo | HarnessLogo, { svg: string; className?: string }> = {
  openai: { svg: openaiIcon, className: 'text-foreground' },
  claude: { svg: claudeIcon },
  gemini: { svg: geminiIcon },
  grok: { svg: grokIcon, className: 'text-foreground' },
  zhipu: { svg: zhipuIcon },
  cursor: { svg: cursorIcon, className: 'text-foreground' },
  'claude-code': { svg: claudeCodeIcon },
  codex: { svg: codexIcon },
  opencode: { svg: opencodeIcon, className: 'text-foreground' },
  windsurf: { svg: windsurfIcon, className: 'text-foreground' },
  'github-copilot': { svg: githubCopilotIcon, className: 'text-foreground' },
}
