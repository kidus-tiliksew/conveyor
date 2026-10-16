// The five headings `submit_plan` requires of every execution plan
// (component-mcp-protocol), in the order the plan panel shows them
// (feature-operator-next-action NA-6).
export const planHeadings = ['Approach', 'Files touched', 'Ordering', 'Risks', 'Done criteria'] as const

export type PlanHeading = (typeof planHeadings)[number]

// Splits plan markdown at its five required `##` headings. A plan missing any
// of them returns null, so the caller renders it whole and drops nothing.
// Text before the first required heading, and any other heading, stays with
// the section it falls in.
export function splitPlanSections(markdown: string): Record<PlanHeading, string> | null {
  const starts: { heading: PlanHeading; at: number; body: number }[] = []
  const pattern = /^##[ \t]+(.+?)[ \t]*#*[ \t]*$/gm
  for (let match = pattern.exec(markdown); match; match = pattern.exec(markdown)) {
    const heading = planHeadings.find((name) => name.toLowerCase() === match?.[1].trim().toLowerCase())
    if (heading && !starts.some((start) => start.heading === heading))
      starts.push({ heading, at: match.index, body: match.index + match[0].length })
  }
  if (starts.length !== planHeadings.length) return null
  const sections = {} as Record<PlanHeading, string>
  starts.forEach((start, index) => {
    sections[start.heading] = markdown.slice(start.body, starts[index + 1]?.at ?? markdown.length).trim()
  })
  return sections
}

export interface PlanFile {
  path: string
  note: string
}

// The Files touched bullets as paths with their notes: the bullet's first code
// span when it has one, else its first word. Lines that are not bullets are
// continuation text and join the previous bullet's note.
export function planFiles(section: string): PlanFile[] {
  const files: PlanFile[] = []
  for (const line of section.split('\n')) {
    const bullet = /^\s*[-*+]\s+(.*)$/.exec(line)
    if (!bullet) {
      const last = files.at(-1)
      if (last && line.trim()) last.note = `${last.note} ${line.trim()}`.trim()
      continue
    }
    const text = bullet[1].trim()
    const code = /`([^`]+)`/.exec(text)
    const path = code ? code[1] : (text.split(/\s+/)[0] ?? '')
    const rest = code ? text.replace(code[0], '') : text.slice(path.length)
    files.push({ path, note: rest.replace(/^[\s:—–-]+/, '').trim() })
  }
  return files.filter((file) => file.path)
}
