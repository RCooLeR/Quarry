export type NotificationSeverity = "info" | "success" | "warning" | "error";

export interface NotificationOwner {
  operationId: string;
  sequence: number;
  operation: string;
  fileId?: string;
  path?: string;
}

export interface OperationNotification extends NotificationOwner {
  severity: NotificationSeverity;
  message: string;
  details?: string;
  timestamp: number;
  dismissible: boolean;
  expiresAt?: number;
}

interface SuppressedOwner {
  operationId: string;
  sequence: number;
}

export interface NotificationState {
  current: OperationNotification | null;
  lastSequence: number;
  latestOwner: NotificationOwner | null;
  suppressed: SuppressedOwner | null;
}

export interface NotificationInput {
  severity: NotificationSeverity;
  message: unknown;
  details?: unknown;
  timestamp?: number;
  dismissible?: boolean;
  expiresAfterMs?: number | null;
}

export type NotificationAction =
  | { type: "reserve"; owner: NotificationOwner }
  | { type: "publish"; owner: NotificationOwner; input: NotificationInput }
  | { type: "dismiss"; operationId: string; sequence: number }
  | { type: "expire"; operationId: string; sequence: number; now: number }
  | { type: "active-file"; fileId: string | null };

export const initialNotificationState: NotificationState = {
  current: null,
  lastSequence: 0,
  latestOwner: null,
  suppressed: null,
};

export const INFO_EXPIRY_MS = 8_000;
export const SUCCESS_EXPIRY_MS = 10_000;

const MAX_OPERATION_ID = 160;
const MAX_OPERATION = 100;
const MAX_FILE_ID = 160;
const MAX_PATH = 1_024;
const MAX_MESSAGE = 600;
const MAX_DETAILS = 4_096;
const MAX_DATE_TIMESTAMP = 8_640_000_000_000_000;

function boundedText(value: unknown, max: number, fallback = ""): string {
  const text = typeof value === "string"
    ? value
    : value instanceof Error
      ? value.message
      : value == null
        ? fallback
        : String(value);
  return text.replace(/\u0000/g, "").slice(0, max);
}

export function normalizeNotificationOwner(owner: NotificationOwner): NotificationOwner | null {
  const operationId = boundedText(owner.operationId, MAX_OPERATION_ID).trim();
  const operation = boundedText(owner.operation, MAX_OPERATION, "Operation").trim() || "Operation";
  if (!operationId || !Number.isSafeInteger(owner.sequence) || owner.sequence <= 0) return null;
  const fileId = boundedText(owner.fileId, MAX_FILE_ID).trim();
  const path = boundedText(owner.path, MAX_PATH);
  return {
    operationId,
    sequence: owner.sequence,
    operation,
    ...(fileId ? { fileId } : {}),
    ...(path ? { path } : {}),
  };
}

function defaultExpiry(severity: NotificationSeverity): number | null {
  if (severity === "success") return SUCCESS_EXPIRY_MS;
  if (severity === "info") return INFO_EXPIRY_MS;
  return null;
}

export function createNotification(owner: NotificationOwner, input: NotificationInput): OperationNotification | null {
  const normalizedOwner = normalizeNotificationOwner(owner);
  if (!normalizedOwner) return null;
  const rawTimestamp = Number.isFinite(input.timestamp) ? input.timestamp! : Date.now();
  const timestamp = Math.min(MAX_DATE_TIMESTAMP, Math.max(0, Math.trunc(rawTimestamp)));
  const message = boundedText(input.message, MAX_MESSAGE).trim() || `${normalizedOwner.operation} status changed`;
  const details = boundedText(input.details, MAX_DETAILS).trim();
  const expiry = input.expiresAfterMs === undefined ? defaultExpiry(input.severity) : input.expiresAfterMs;
  const expiresAt = typeof expiry === "number" && Number.isFinite(expiry) && expiry >= 0
    ? timestamp + Math.trunc(expiry)
    : undefined;
  return {
    ...normalizedOwner,
    severity: input.severity,
    message,
    ...(details ? { details } : {}),
    timestamp,
    dismissible: input.dismissible ?? true,
    ...(expiresAt != null ? { expiresAt } : {}),
  };
}

function sameOwner(left: SuppressedOwner | null, right: NotificationOwner): boolean {
  return left?.operationId === right.operationId && left.sequence === right.sequence;
}

export function notificationReducer(state: NotificationState, action: NotificationAction): NotificationState {
  if (action.type === "reserve") {
    const owner = normalizeNotificationOwner(action.owner);
    if (!owner || owner.sequence <= state.lastSequence) return state;
    return {
      current: null,
      lastSequence: owner.sequence,
      latestOwner: owner,
      suppressed: null,
    };
  }

  if (action.type === "publish") {
    const notification = createNotification(action.owner, action.input);
    if (!notification || notification.sequence < state.lastSequence) return state;
    if (sameOwner(state.suppressed, notification)) return state;
    if (notification.sequence === state.lastSequence && state.latestOwner && !sameOwner(state.latestOwner, notification)) {
      return state;
    }
    return {
      current: notification,
      lastSequence: Math.max(state.lastSequence, notification.sequence),
      latestOwner: notification.sequence > state.lastSequence ? notification : state.latestOwner,
      suppressed: notification.sequence > state.lastSequence ? null : state.suppressed,
    };
  }

  if (action.type === "dismiss") {
    const current = state.current;
    if (!current || !current.dismissible || current.operationId !== action.operationId || current.sequence !== action.sequence) {
      return state;
    }
    return {
      ...state,
      current: null,
      suppressed: { operationId: current.operationId, sequence: current.sequence },
    };
  }

  if (action.type === "expire") {
    const current = state.current;
    if (
      !current
      || current.operationId !== action.operationId
      || current.sequence !== action.sequence
      || current.expiresAt == null
      || action.now < current.expiresAt
    ) return state;
    return {
      ...state,
      current: null,
      // Expiry owns the exact notification revision just like an explicit
      // dismissal. A delayed completion for that owner cannot resurrect it;
      // genuinely later phases (for example a job terminal event) reserve a
      // new monotonic owner.
      suppressed: { operationId: current.operationId, sequence: current.sequence },
    };
  }

  const current = state.current;
  if (current?.fileId && current.fileId !== action.fileId) {
    return {
      ...state,
      current: null,
      suppressed: { operationId: current.operationId, sequence: current.sequence },
    };
  }
  const latest = state.latestOwner;
  if (!current && latest?.fileId && latest.fileId !== action.fileId) {
    return {
      ...state,
      suppressed: { operationId: latest.operationId, sequence: latest.sequence },
    };
  }
  return state;
}

export function notificationDetailsText(notification: OperationNotification): string {
  const lines = [
    `${notification.operation}: ${notification.message}`,
    `Severity: ${notification.severity}`,
    `Operation ID: ${notification.operationId}`,
    `Sequence: ${notification.sequence}`,
    `Timestamp: ${new Date(notification.timestamp).toISOString()}`,
  ];
  if (notification.fileId) lines.push(`File ID: ${notification.fileId}`);
  if (notification.path) lines.push(`Path: ${notification.path}`);
  if (notification.details) lines.push("", notification.details);
  return lines.join("\n").slice(0, MAX_DETAILS + MAX_PATH + 1_024);
}
