export interface ActiveSurfaceState {
  gridView: boolean;
  hexView: boolean;
  toolsOpen: boolean;
  diffOpen: boolean;
}

export function supportsDataTools(detected: string): boolean {
  const value = detected.toLowerCase();
  return value === "csv" || value === "tsv" || value === "sql";
}

/**
 * Normalize global workbench flags at an active-file boundary. Alternate
 * primary views do not follow a user to another tab; incompatible panels are
 * closed instead of silently changing meaning for the new file.
 */
export function normalizeSurfaceForFile(
  detected: string,
  hasEdits: boolean,
  current: ActiveSurfaceState,
  fileChanged: boolean,
): ActiveSurfaceState {
  const csv = detected.toLowerCase() === "csv" || detected.toLowerCase() === "tsv";
  return {
    gridView: fileChanged ? false : current.gridView && csv,
    hexView: fileChanged ? false : current.hexView,
    toolsOpen: current.toolsOpen && supportsDataTools(detected),
    diffOpen: current.diffOpen && hasEdits,
  };
}
