import { expect, test } from '@playwright/test'
import { documentMatchesQuery } from '../src/components/documents/document-search'

// The Requirements and System Design trees share this predicate: search
// matches a document entry's title or ID, ignoring letter case
// (component-web-document-surfaces, "The tree").
const requirement = { id: 'req-260805-3f9a04', title: 'Document archive' }
const design = { id: 'component-artifacts', title: 'Artifacts: media validation' }

test('document search matches a title substring', () => {
  expect(documentMatchesQuery(requirement, 'archive')).toBe(true)
  expect(documentMatchesQuery(design, 'media')).toBe(true)
})

test('document search matches a full or partial ID', () => {
  expect(documentMatchesQuery(requirement, 'req-260805-3f9a04')).toBe(true)
  expect(documentMatchesQuery(requirement, '260805')).toBe(true)
  expect(documentMatchesQuery(design, 'component-artifacts')).toBe(true)
  expect(documentMatchesQuery(design, 'component-art')).toBe(true)
})

test('document search ignores letter case in the title and the ID', () => {
  expect(documentMatchesQuery(requirement, 'REQ-260805')).toBe(true)
  expect(documentMatchesQuery(requirement, 'DOCUMENT ARCHIVE')).toBe(true)
  expect(documentMatchesQuery(design, 'Component-Artifacts')).toBe(true)
  expect(documentMatchesQuery({ id: 'REQ-UPPER', title: 'Upper' }, 'req-upper')).toBe(true)
})

test('document search trims the query and matches every entry for a blank query', () => {
  expect(documentMatchesQuery(requirement, '  req-260805  ')).toBe(true)
  expect(documentMatchesQuery(requirement, '')).toBe(true)
  expect(documentMatchesQuery(requirement, '   ')).toBe(true)
})

test('document search rejects a query that matches neither the title nor the ID', () => {
  expect(documentMatchesQuery(requirement, 'component-artifacts')).toBe(false)
  expect(documentMatchesQuery(design, 'req-260805')).toBe(false)
  expect(documentMatchesQuery(design, 'missing')).toBe(false)
})
