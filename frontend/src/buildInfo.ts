export interface BuildInfoData {
  applicationName: string;
  version: string;
  commit: string;
  buildDate: string;
}

function value(input: unknown, fallback: string): string {
  return typeof input === "string" && input.trim() ? input.trim() : fallback;
}

export function formatBuildInfo(input: Partial<BuildInfoData> | null | undefined): string {
  const name = value(input?.applicationName, "Quarry Editor");
  const version = value(input?.version, "unknown");
  const commit = value(input?.commit, "unknown");
  const buildDate = value(input?.buildDate, "unknown");
  return `${name} ${version}\nCommit: ${commit}\nBuilt: ${buildDate}\n\nStreaming desktop editor for very large files. Source files are not silently overwritten.`;
}
