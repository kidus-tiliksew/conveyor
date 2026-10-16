// The five headings `submit_plan` requires of every execution plan
// (component-mcp-protocol), in the order the plan panel shows them
// (feature-operator-next-action NA-6).
export const planHeadings = ['Approach', 'Files touched', 'Ordering', 'Risks', 'Done criteria'] as const

export type PlanHeading = (typeof planHeadings)[number]

export interface PlanSections {
  // Text before the first required heading, shown above the sections.
  preamble: string
  sections: Record<PlanHeading, string>
}

// Splits plan markdown at its five required `##` headings without dropping
// any text: whatever precedes the first required heading is the preamble,
// and any other heading stays inside the section it falls in. A plan missing
// any required heading returns null, so the caller renders it whole.
export function splitPlanSections(markdown: string): PlanSections | null {
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
  return { preamble: markdown.slice(0, starts[0].at).trim(), sections }
}

export interface PlanFile {
  path: string
  note: string
}

export interface PlanFiles {
  // Text before the first file bullet, shown above the list.
  intro: string
  files: PlanFile[]
}

// The Files touched section as its intro text and its top-level bullets, each
// a path with its note: the bullet's first code span when it has one, else its
// first word. Every other line after the first bullet, indented or not, joins
// the previous bullet's note, so no text is dropped.
export function planFiles(section: string): PlanFiles {
  const intro: string[] = []
  const files: PlanFile[] = []
  for (const line of section.split('\n')) {
    const bullet = /^[-*+]\s+(.*)$/.exec(line)
    if (!bullet) {
      const last = files.at(-1)
      if (last) {
        if (line.trim()) last.note = `${last.note} ${line.trim()}`.trim()
      } else intro.push(line)
      continue
    }
    const text = bullet[1].trim()
    if (!text) continue
    const code = /`([^`]+)`/.exec(text)
    const path = code ? code[1] : (text.split(/\s+/)[0] ?? '')
    const rest = code ? text.replace(code[0], '') : text.slice(path.length)
    files.push({ path, note: rest.replace(/^[\s:—–-]+/, '').trim() })
  }
  return { intro: intro.join('\n').trim(), files }
}
