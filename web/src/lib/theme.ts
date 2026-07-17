export const THEME_STORAGE_KEY = 'conveyor-theme'
export const DARK_MEDIA_QUERY = '(prefers-color-scheme: dark)'

export const THEME_COLORS = {
  light: '#ffffff',
  dark: '#0f1115',
} as const

export type ThemeChoice = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

export function isThemeChoice(value: unknown): value is ThemeChoice {
  return value === 'light' || value === 'dark' || value === 'system'
}

export function readThemeChoice(storage?: Pick<Storage, 'getItem'>): ThemeChoice {
  try {
    const saved = (storage ?? window.localStorage).getItem(THEME_STORAGE_KEY)
    return isThemeChoice(saved) ? saved : 'system'
  } catch {
    return 'system'
  }
}

export function persistThemeChoice(choice: ThemeChoice, storage?: Pick<Storage, 'setItem'>) {
  try {
    (storage ?? window.localStorage).setItem(THEME_STORAGE_KEY, choice)
  } catch {
    // Local preferences are best-effort; an unavailable store must not break the dashboard.
  }
}

export function resolveTheme(choice: ThemeChoice, systemPrefersDark: boolean): ResolvedTheme {
  return choice === 'system' ? (systemPrefersDark ? 'dark' : 'light') : choice
}

export function applyResolvedTheme(theme: ResolvedTheme, documentTarget: Document = document) {
  const root = documentTarget.documentElement
  root.dataset.theme = theme
  root.style.colorScheme = theme

  const themeColor = documentTarget.querySelector<HTMLMetaElement>('meta[name="theme-color"]')
  if (themeColor) themeColor.content = THEME_COLORS[theme]
}
