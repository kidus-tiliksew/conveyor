import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from 'react'
import {
  applyResolvedTheme,
  DARK_MEDIA_QUERY,
  persistThemeChoice,
  readThemeChoice,
  resolveTheme,
  type ResolvedTheme,
  type ThemeChoice,
} from '../lib/theme'

type ThemeContextValue = {
  choice: ThemeChoice
  resolvedTheme: ResolvedTheme
  setChoice: (choice: ThemeChoice) => void
}

const ThemeContext = createContext<ThemeContextValue | undefined>(undefined)

function systemPrefersDark() {
  return window.matchMedia(DARK_MEDIA_QUERY).matches
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [choice, setChoiceState] = useState<ThemeChoice>(readThemeChoice)
  const [prefersDark, setPrefersDark] = useState(systemPrefersDark)
  const resolvedTheme = resolveTheme(choice, prefersDark)

  useEffect(() => {
    const media = window.matchMedia(DARK_MEDIA_QUERY)
    const handleChange = (event: MediaQueryListEvent) => setPrefersDark(event.matches)
    setPrefersDark(media.matches)
    media.addEventListener('change', handleChange)
    return () => media.removeEventListener('change', handleChange)
  }, [])

  useLayoutEffect(() => {
    applyResolvedTheme(resolvedTheme)
  }, [resolvedTheme])

  const value = useMemo<ThemeContextValue>(() => ({
    choice,
    resolvedTheme,
    setChoice: (nextChoice) => {
      persistThemeChoice(nextChoice)
      setChoiceState(nextChoice)
    },
  }), [choice, resolvedTheme])

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}

export function useTheme() {
  const value = useContext(ThemeContext)
  if (!value) throw new Error('useTheme must be used within ThemeProvider')
  return value
}
