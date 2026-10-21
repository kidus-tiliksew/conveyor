// Which sections of a proposed document its justification cites
// (feature-operator-next-action NA-6). The match runs in the browser over the
// section list `reviewBlocks` produces; the proposal stores no citation field.
//
// A section is cited when the justification contains its `REQ-n` or `AC-n.m`
// ID as a whole token, or contains its heading text case-insensitively as
// whole words, ignoring a leading heading number. An `AC-n.m` match also
// cites its parent `REQ-n` section.

export interface CitableSection {
  id: string
  title: string
  // Whether the title is a heading the author wrote. The synthetic
  // "Overview" title of a document's untitled preamble is not.
  heading: boolean
}

const statementId = /^(REQ-\d+|AC-(\d+)\.\d+)$/

function escapeRegExp(text: string) {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// An ID token: not glued to a letter, digit, or hyphen before it, and not
// continued by a digit or a further `.digit` after it, so REQ-1 never matches
// REQ-10 and AC-7.1 never matches AC-7.10.
function idPattern(id: string) {
  return new RegExp(`(?<![\\p{L}\\p{N}-])${escapeRegExp(id)}(?![\\p{N}]|\\.\\p{N})`, 'u')
}

// Heading words in order, separated by any run of whitespace, with no letter
// or digit touching either end.
function headingPattern(words: string[]) {
  return new RegExp(`(?<![\\p{L}\\p{N}])${words.map(escapeRegExp).join('\\s+')}(?![\\p{L}\\p{N}])`, 'iu')
}

// The heading's words without Markdown inline marks and without a leading
// number such as "3", "3.", or "3.2".
export function headingWords(title: string) {
  const plain = title
    .replace(/[`*_]/g, '')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .trim()
  const unnumbered = plain.replace(/^\d+(?:\.\d+)*\.?\s+/, '')
  return unnumbered.split(/\s+/).filter(Boolean)
}

export function citedSections(sections: CitableSection[], justification: string) {
  const cited = new Set<string>()
  const text = justification.trim()
  if (!text) return cited
  const byTitle = new Map(sections.map((section) => [section.title, section.id]))
  for (const section of sections) {
    const statement = section.title.match(statementId)
    if (statement) {
      if (!idPattern(section.title).test(text)) continue
      cited.add(section.id)
      // An acceptance criterion cites the requirement it belongs to.
      const parent = statement[2] ? byTitle.get(`REQ-${statement[2]}`) : undefined
      if (parent) cited.add(parent)
      continue
    }
    if (!section.heading) continue
    const words = headingWords(section.title)
    if (words.join(' ').length < 3) continue
    if (headingPattern(words).test(text)) cited.add(section.id)
  }
  return cited
}
