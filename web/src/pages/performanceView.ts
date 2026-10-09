import type { PerfPoint, PerfRow, PerformanceReport } from '../lib/api'

/** The ranking API returns the busiest 50 rows per dimension, not fleet totals. */
export function performanceSummary(rows: PerfRow[]) {
  const requests = rows.reduce((total, row) => total + row.requests, 0)
  const failed = rows.reduce((total, row) => total + row.failed, 0)
  const slowest = rows.reduce<PerfRow | null>((worst, row) =>
    row.requests > 0 && (!worst || row.p95_ms > worst.p95_ms) ? row : worst, null)
  return { requests, failed, successPct: requests ? (1 - failed / requests) * 100 : null, slowest }
}

export function problemRows(rows: PerfRow[], onlyProblems: boolean): PerfRow[] {
  return onlyProblems ? rows.filter(row => row.failed > 0 || row.uptime_pct < 99) : rows
}

/** Request logs accept Go durations or RFC3339, but not report shorthand such as 7d. */
export function performanceLogsUrl(report: Pick<PerformanceReport, 'from'>, model?: string): string {
  const params = new URLSearchParams({ since: report.from })
  if (model) params.set('model', model)
  return `/logs?${params.toString()}`
}

/** Ignore malformed/missing bucket keys so one bad response cannot blank all charts. */
export function performancePoints(points: PerfPoint[]): PerfPoint[] {
  return points.filter(point => typeof point.day === 'string' && point.day.length > 0)
}
