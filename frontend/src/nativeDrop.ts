import { MAX_OPEN_FILE_SESSIONS } from "./productLimits";
import { isBoundedSourcePathInput } from "./sourcePathInput";

export interface NormalizedNativeDrop {
  paths: string[];
  omitted: number;
}

interface NativeDropEnvelope {
  paths: unknown[];
  omitted: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readEnvelope(value: unknown): NativeDropEnvelope | null {
  if (!isRecord(value) || !Array.isArray(value.paths)) return null;
  if (!Number.isSafeInteger(value.omitted) || (value.omitted as number) < 0) return null;
  return { paths: value.paths, omitted: value.omitted as number };
}

/**
 * Treat native event data as untrusted bridge input. Work is bounded before
 * validating individual entries, and both malformed and over-cap entries are
 * represented by one aggregate omission count for the caller to report.
 */
export function normalizeNativeDrop(value: unknown, remainingSlots: number): NormalizedNativeDrop {
  const envelope = readEnvelope(value);
  if (!envelope) return { paths: [], omitted: 1 };

  const slots = Number.isFinite(remainingSlots)
    ? Math.max(0, Math.min(MAX_OPEN_FILE_SESSIONS, Math.trunc(remainingSlots)))
    : 0;
  const inspected = envelope.paths.slice(0, MAX_OPEN_FILE_SESSIONS);
  let omitted = envelope.omitted + Math.max(0, envelope.paths.length - inspected.length);
  // Keep all counts exactly representable even if a hostile renderer payload
  // supplies an implausibly large upstream omission value.
  omitted = Math.min(Number.MAX_SAFE_INTEGER, omitted);

  const valid: string[] = [];
  for (const candidate of inspected) {
    if (typeof candidate !== "string" || !isBoundedSourcePathInput(candidate)) {
      omitted = Math.min(Number.MAX_SAFE_INTEGER, omitted + 1);
      continue;
    }
    valid.push(candidate);
  }

  if (valid.length > slots) {
    omitted = Math.min(Number.MAX_SAFE_INTEGER, omitted + valid.length - slots);
  }
  return { paths: valid.slice(0, slots), omitted };
}
