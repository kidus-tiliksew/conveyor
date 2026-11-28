/**
 * The Monitor page's address (component-feedback-reports invariant 26;
 * req-production-feedback AC-6.3): the report filters live in it, so a reload
 * or a shared link shows the same list.
 *
 * - `environment`: one environment ID. A value that is not one non-blank
 *   string (missing, empty, repeated, a number, or an object) is dropped.
 *   An ID that names no environment of the workspace is kept: the server
 *   answers it with an empty list, and the page says that the environment is
 *   not in the list.
 * - `no_crate`: `true` keeps the filter for reports that belong to no crate.
 *   Any other value is dropped.
 * - `show`: `closed` lists closed reports. Open is the default and is never
 *   written. Any other value is dropped.
 *
 * The page writes the cleaned address back in place, so the address always
 * holds what the page shows.
 */
export interface MonitorSearch {
  environment?: string
  no_crate?: true
  show?: 'closed'
}

export function validateMonitorSearch(search: Record<string, unknown>): MonitorSearch {
  const environment =
    typeof search.environment === 'string' && search.environment.trim() !== '' ? search.environment : undefined
  return {
    environment,
    no_crate: search.no_crate === true || search.no_crate === 'true' ? true : undefined,
    show: search.show === 'closed' ? 'closed' : undefined,
  }
}

/** True when the parsed address holds exactly the cleaned search, in any order. */
export function sameSearch(raw: Record<string, unknown>, search: MonitorSearch) {
  const defined = (value: Record<string, unknown>) =>
    Object.keys(value)
      .filter((key) => value[key] !== undefined)
      .sort()
  const rawKeys = defined(raw)
  const searchKeys = defined(search as Record<string, unknown>)
  return (
    rawKeys.length === searchKeys.length &&
    rawKeys.every((key, index) => key === searchKeys[index] && raw[key] === (search as Record<string, unknown>)[key])
  )
}
