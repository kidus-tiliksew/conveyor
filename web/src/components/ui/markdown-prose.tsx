import { useEffect, useId, useState } from 'react'
import mermaid from 'mermaid'
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
