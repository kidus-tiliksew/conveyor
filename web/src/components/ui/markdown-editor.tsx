import { useId, useLayoutEffect, useRef, useState } from 'react'
import {
  Bold,
  Code,
  FileCode2,
  Heading,
  Italic,
  Link as LinkIcon,
  List,
  ListOrdered,
  ListTodo,
  Quote,
} from 'lucide-react'
import { cn } from '../../lib/utils'
import { MarkdownProse } from './markdown-prose'

type Selection = { start: number; end: number }
type InlineAction = 'bold' | 'italic' | 'inline-code' | 'code-block' | 'link'
type LineAction = 'heading' | 'quote' | 'bullet-list' | 'numbered-list' | 'task-list'

const linePrefixes: Record<Exclude<LineAction, 'numbered-list'>, string> = {
  heading: '## ',
  quote: '> ',
  'bullet-list': '- ',
  'task-list': '- [ ] ',
}

export function MarkdownEditor({
  value,
  onChange,
  placeholder,
}: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
}) {
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const pendingSelection = useRef<Selection | undefined>(undefined)
  const writeTabID = useId()
  const previewTabID = useId()
  const writePanelID = useId()
  const previewPanelID = useId()

  useLayoutEffect(() => {
    const selection = pendingSelection.current
    const textarea = textareaRef.current
    if (!selection || !textarea || tab !== 'write') return
    pendingSelection.current = undefined
    textarea.focus()
    textarea.setSelectionRange(selection.start, selection.end)
  }, [tab, value])

  const commit = (nextValue: string, selection: Selection) => {
    pendingSelection.current = selection
    onChange(nextValue)
    if (nextValue === value) {
      requestAnimationFrame(() => {
        const textarea = textareaRef.current
        if (!textarea) return
        pendingSelection.current = undefined
        textarea.focus()
        textarea.setSelectionRange(selection.start, selection.end)
      })
    }
  }

  const selectedRange = () => {
    const textarea = textareaRef.current
    return {
      start: textarea?.selectionStart ?? value.length,
      end: textarea?.selectionEnd ?? value.length,
    }
  }

  const applyInline = (action: InlineAction) => {
    const { start, end } = selectedRange()
    const selected = value.slice(start, end)

    if (action === 'link') {
      const completeLink = selected.match(/^\[([\s\S]*)\]\(([^\n]*)\)$/)
      if (completeLink) {
        const text = completeLink[1]
        commit(value.slice(0, start) + text + value.slice(end), { start, end: start + text.length })
        return
      }
      const label = selected || 'text'
      const replacement = `[${label}](url)`
      const linkStart = start + label.length + 3
      commit(value.slice(0, start) + replacement + value.slice(end), { start: linkStart, end: linkStart + 3 })
      return
    }

    const markers =
      action === 'bold'
        ? ['**', '**']
        : action === 'italic'
          ? ['_', '_']
          : action === 'inline-code'
            ? ['`', '`']
            : ['```\n', '\n```']
    const [open, close] = markers
    if (selected.startsWith(open) && selected.endsWith(close) && selected.length >= open.length + close.length) {
      const unwrapped = selected.slice(open.length, selected.length - close.length)
      commit(value.slice(0, start) + unwrapped + value.slice(end), { start, end: start + unwrapped.length })
      return
    }
    const alreadyWrapped =
      value.slice(Math.max(0, start - open.length), start) === open && value.slice(end, end + close.length) === close
    if (alreadyWrapped) {
      const next = value.slice(0, start - open.length) + selected + value.slice(end + close.length)
      commit(next, { start: start - open.length, end: end - open.length })
      return
    }
    const replacement = open + selected + close
    const selectionStart = start + open.length
    commit(value.slice(0, start) + replacement + value.slice(end), {
      start: selectionStart,
      end: selectionStart + selected.length,
    })
  }

  const applyLines = (action: LineAction) => {
    const { start, end } = selectedRange()
    const blockStart = value.lastIndexOf('\n', Math.max(0, start - 1)) + 1
    const nextNewline = value.indexOf('\n', end)
    const blockEnd = nextNewline === -1 ? value.length : nextNewline
    const lines = value.slice(blockStart, blockEnd).split('\n')
    const target = action === 'numbered-list' ? /^\d+\. / : new RegExp(`^${escapeRegExp(linePrefixes[action])}`)
    const allTarget = lines.every((line) => target.test(line))
    const knownPrefix = /^(?:#{1,6} |> |- \[[ xX]\] |- |\d+\. )/
    const transformed = lines.map((line, index) => {
      if (allTarget) return line.replace(target, '')
      const bare = line.replace(knownPrefix, '')
      const prefix = action === 'numbered-list' ? `${index + 1}. ` : linePrefixes[action]
      return prefix + bare
    })
    const replacement = transformed.join('\n')
    const deltaAtStart = transformed[0].length - lines[0].length
    const nextStart = Math.max(blockStart, start + deltaAtStart)
    commit(value.slice(0, blockStart) + replacement + value.slice(blockEnd), {
      start: nextStart,
      end: Math.max(nextStart, end + replacement.length - (blockEnd - blockStart)),
    })
  }

  const handleKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (!(event.metaKey || event.ctrlKey)) return
    const action =
      event.key.toLowerCase() === 'b'
        ? 'bold'
        : event.key.toLowerCase() === 'i'
          ? 'italic'
          : event.key.toLowerCase() === 'k'
            ? 'link'
            : undefined
    if (!action) return
    event.preventDefault()
    applyInline(action)
  }

  const tools: Array<{ label: string; Icon: typeof Bold; action: () => void }> = [
    { label: 'Heading', Icon: Heading, action: () => applyLines('heading') },
    { label: 'Bold', Icon: Bold, action: () => applyInline('bold') },
    { label: 'Italic', Icon: Italic, action: () => applyInline('italic') },
    { label: 'Quote', Icon: Quote, action: () => applyLines('quote') },
    { label: 'Inline code', Icon: Code, action: () => applyInline('inline-code') },
    { label: 'Code block', Icon: FileCode2, action: () => applyInline('code-block') },
    { label: 'Link', Icon: LinkIcon, action: () => applyInline('link') },
    { label: 'Bullet list', Icon: List, action: () => applyLines('bullet-list') },
    { label: 'Numbered list', Icon: ListOrdered, action: () => applyLines('numbered-list') },
    { label: 'Task list', Icon: ListTodo, action: () => applyLines('task-list') },
  ]

  return (
    <div className="overflow-hidden rounded-md border border-edge bg-input focus-within:border-primary/50 focus-within:ring-2 focus-within:ring-primary/10">
      <div className="flex min-h-10 flex-wrap items-center justify-between gap-1 border-b border-border bg-surface px-2">
        <div role="tablist" aria-label="Markdown editor mode" className="flex self-stretch">
          <button
            id={writeTabID}
            role="tab"
            type="button"
            aria-controls={writePanelID}
            aria-selected={tab === 'write'}
            onClick={() => setTab('write')}
            className={cn(
              'border-b-2 px-3 text-xs font-medium',
              tab === 'write'
                ? 'border-primary text-foreground'
                : 'border-transparent text-muted hover:text-foreground',
            )}
          >
            Write
          </button>
          <button
            id={previewTabID}
            role="tab"
            type="button"
            aria-controls={previewPanelID}
            aria-selected={tab === 'preview'}
            onClick={() => setTab('preview')}
            className={cn(
              'border-b-2 px-3 text-xs font-medium',
              tab === 'preview'
                ? 'border-primary text-foreground'
                : 'border-transparent text-muted hover:text-foreground',
            )}
          >
            Preview
          </button>
        </div>
        {tab === 'write' && (
          <div role="toolbar" aria-label="Markdown formatting" className="flex flex-wrap items-center py-1">
            {tools.map(({ label, Icon, action }) => (
              <button
                key={label}
                type="button"
                aria-label={label}
                title={label}
                onMouseDown={(event) => event.preventDefault()}
                onClick={action}
                className="rounded p-1.5 text-faint hover:bg-raised hover:text-foreground focus-visible:outline-2 focus-visible:outline-primary"
              >
                <Icon aria-hidden="true" className="size-3.5" />
              </button>
            ))}
          </div>
        )}
      </div>
      {tab === 'write' ? (
        <div id={writePanelID} role="tabpanel" aria-labelledby={writeTabID}>
          <textarea
            ref={textareaRef}
            aria-label="Task description"
            value={value}
            onChange={(event) => onChange(event.target.value)}
            onKeyDown={handleKeyDown}
            placeholder={placeholder}
            style={{ fontFamily: 'var(--font-mono)' }}
            className="block min-h-56 w-full resize-y bg-transparent px-3 py-2 text-sm leading-6 text-foreground outline-none placeholder:text-faint"
          />
        </div>
      ) : (
        <div id={previewPanelID} role="tabpanel" aria-labelledby={previewTabID} className="min-h-56 px-3 py-2">
          {value.trim() ? (
            <MarkdownProse>{value}</MarkdownProse>
          ) : (
            <p className="text-sm italic text-faint">Nothing to preview</p>
          )}
        </div>
      )}
    </div>
  )
}

function escapeRegExp(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}
