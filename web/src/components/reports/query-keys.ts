import type { FeedbackReportFilter } from '../../lib/types'

/**
 * Every report read starts `['feedback-reports', workspace]`, so one prefix
 * refreshes each list and each single report: after an act, and on a frame of
 * the workspace stream (component-feedback-reports invariant 27).
 */
export function feedbackReportsQueryKey(workspace: string) {
  return ['feedback-reports', workspace] as const
}

export function feedbackReportListQueryKey(workspace: string, filter: FeedbackReportFilter) {
  return [
    'feedback-reports',
    workspace,
    'list',
    {
      state: filter.state ?? 'open',
      environment_id: filter.environment_id ?? '',
      task_id: filter.task_id ?? '',
      crate_id: filter.crate_id ?? '',
      no_crate: filter.no_crate === true,
    },
  ] as const
}

export function feedbackReportQueryKey(workspace: string, reportId: string) {
  return ['feedback-reports', workspace, 'report', reportId] as const
}
