import mermaid from 'mermaid'
import { useEffect, useId, useState } from 'react'
import Markdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'

mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' })

function MermaidBlock({ source }: { source: string }) {
  const id = `mermaid-${useId().replace(/:/g, '')}`
  const [svg, setSvg] = useState<string>()
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    setSvg(undefined)
    setFailed(false)
    mermaid
      .render(id, source)
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
  // The wrapper scrolls a table too wide for its column, and carries the border
  // so the rounded corners stay intact while it scrolls.
  table({ node: _node, ...props }) {
    return (
      <div className="markdown-table">
        <table {...props} />
      </div>
    )
  },
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

// A document's first heading usually repeats the title the page already shows.
export function withoutRepeatedTitle(content: string, title: string) {
  const match = content.match(/^\s*#{1,2}[ \t]+(.+?)[ \t]*\n+/)
  return match && match[1].trim().toLowerCase() === title.trim().toLowerCase()
    ? content.slice(match[0].length)
    : content
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
