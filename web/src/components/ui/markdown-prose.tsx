import { useEffect, useId, useState } from 'react'
import Markdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'

type Mermaid = typeof import('mermaid')['default']

// Mermaid loads only when a diagram block mounts, so it stays out of the
// dashboard's entry bundle and its preload links (component-web-dashboard).
// Every block shares one import, which initializes Mermaid once. A failed
// import is forgotten, so a later block can try again.
let mermaidLoad: Promise<Mermaid> | undefined

function loadMermaid() {
  mermaidLoad ??= import('mermaid').then(
    ({ default: mermaid }) => {
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' })
      return mermaid
    },
    (error: unknown) => {
      mermaidLoad = undefined
      throw error
    },
  )
  return mermaidLoad
}

// The code fence stays visible while Mermaid loads or renders, and after a
// load or parse failure. A result that arrives after the source changes or
// the block unmounts is dropped.
function MermaidBlock({ source }: { source: string }) {
  const id = `mermaid-${useId().replace(/:/g, '')}`
  const [svg, setSvg] = useState<string>()
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    setSvg(undefined)
    setFailed(false)
    loadMermaid()
      .then((mermaid) => mermaid.render(id, source))
      .then(({ svg: rendered }) => {
        if (active) setSvg(rendered)
      })
      .catch(() => {
        if (active) setFailed(true)
      })
    return () => {
      active = false
    }
  }, [id, source])

  if (!svg || failed)
    return (
      <pre>
        <code className="language-mermaid">{source}</code>
      </pre>
    )
  return <div data-mermaid dangerouslySetInnerHTML={{ __html: svg }} />
}

const sharedComponents: Components = {
  code({ className, children, ...props }) {
    const source = String(children).replace(/\n$/, '')
    if (className === 'language-mermaid') return <MermaidBlock source={source} />
    return (
      <code className={className} {...props}>
        {children}
      </code>
    )
  },
}

// Shared safe GFM presentation for human-authored prose. React Markdown keeps
// raw HTML disabled unless rehype-raw is added; do not add it here.
export function MarkdownProse({
  children,
  className = '',
  components,
}: {
  children: string
  className?: string
  components?: Components
}) {
  return (
    <div className={`markdown ${className}`.trim()}>
      <Markdown remarkPlugins={[remarkGfm]} components={{ ...sharedComponents, ...components }}>
        {children}
      </Markdown>
    </div>
  )
}

// Phrasing-only Markdown for a one-line preview inside a link, such as a
// Decisions list row: code spans, emphasis, and strikethrough render, and
// every other element, including paragraphs, lists, headings, and links,
// unwraps to its text, so the preview adds no block and no nested link. It
// uses the same GFM parser with raw HTML disabled; do not add rehype-raw here.
const inlineElements = ['code', 'em', 'strong', 'del']

export function InlineMarkdown({ children, className = '' }: { children: string; className?: string }) {
  return (
    <span className={`markdown markdown-inherit ${className}`.trim()}>
      <Markdown remarkPlugins={[remarkGfm]} allowedElements={inlineElements} unwrapDisallowed>
        {children}
      </Markdown>
    </span>
  )
}

// A document page already shows its title in the header, so a body that opens
// by repeating it starts at its first real content instead.
export function withoutLeadingTitle(content: string, title?: string) {
  if (!title) return content
  const match = /^\s*#{1,2}\s+(.+?)\s*#*\s*(?:\n|$)/.exec(content)
  if (!match || match[1].trim().toLowerCase() !== title.trim().toLowerCase()) return content
  return content.slice(match[0].length).trimStart()
}
