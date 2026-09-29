import type { NormalizedSqlSummaryResult } from "./bridgePayloads";

export const MAX_SQL_ANALYSIS_CACHE_ENTRIES = 4;

export function cacheSqlAnalysis(
  previous: Record<string, NormalizedSqlSummaryResult>,
  fileId: string,
  summary: NormalizedSqlSummaryResult,
): Record<string, NormalizedSqlSummaryResult> {
  const entries = Object.entries(previous).filter(([existingId]) => existingId !== fileId);
  entries.push([fileId, summary]);
  const bounded = entries.slice(Math.max(0, entries.length - MAX_SQL_ANALYSIS_CACHE_ENTRIES));
  return Object.fromEntries(bounded);
}
