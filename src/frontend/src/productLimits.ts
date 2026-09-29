/** Maximum analyzed-table jump commands materialized in the shared palette. */
export const MAX_PALETTE_TABLES = 300;

/** Matches the backend registry's hard cap on simultaneously open files. */
export const MAX_OPEN_FILE_SESSIONS = 128;

/**
 * Keeps command construction independent of total schema size. The SQL tools
 * expose their own bounded paging/filtering surface for tables beyond this
 * convenience-command cap.
 */
export function boundedPaletteTables<T>(tables: readonly T[]): readonly T[] {
  return tables.slice(0, MAX_PALETTE_TABLES);
}
