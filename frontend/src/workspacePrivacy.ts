export type QuarryTheme = "dark" | "light";

export interface WorkspacePreferences {
  rememberPaths: boolean;
  restoreSession: boolean;
}

export interface StoredBookmark {
  offset: number;
  label: string;
}

export type StoredBookmarks = Record<string, StoredBookmark[]>;

export interface LocalSettingsSnapshot {
  theme: QuarryTheme;
  showByteOffsets: boolean;
  workspace: WorkspacePreferences;
  recentPaths: string[];
  sessionPaths: string[];
  bookmarks: StoredBookmarks;
}

export interface PreferenceCommitResult {
  preferences: WorkspacePreferences;
  persisted: boolean;
}

type StorageAccess = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export const LOCAL_STORAGE_KEYS = {
  theme: "quarry.theme",
  byteOffsets: "quarry.byteOffsets",
  workspacePreferences: "quarry.workspacePrivacy",
  recentPaths: "quarry.recent",
  sessionPaths: "quarry.session",
  bookmarks: "quarry.bookmarks",
} as const;

export const WORKSPACE_STORAGE_LIMITS = {
  preferenceBytes: 160,
  themeBytes: 16,
  byteOffsetBytes: 1,
  pathCodeUnits: 32_767,
  recentCount: 12,
  recentBytes: 512 * 1024,
  sessionCount: 16,
  sessionBytes: 1024 * 1024,
  bookmarkFileCount: 64,
  bookmarksPerFile: 256,
  bookmarkTotalCount: 1024,
  bookmarkLabelCodeUnits: 160,
  bookmarkBytes: 2 * 1024 * 1024,
} as const;

const OFF: WorkspacePreferences = { rememberPaths: false, restoreSession: false };

const cloneOff = (): WorkspacePreferences => ({ ...OFF });
const emptyBookmarks = (): StoredBookmarks => Object.create(null) as StoredBookmarks;

/** Returns null when browser storage is unavailable or denied. */
export function browserLocalStorage(): Storage | null {
  try {
    return typeof window === "undefined" ? null : window.localStorage;
  } catch {
    return null;
  }
}

// Count UTF-8 bytes without allocating a second copy of an untrusted payload.
function exceedsUTF8Limit(value: string, limit: number): boolean {
  let bytes = 0;
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code <= 0x7f) bytes += 1;
    else if (code <= 0x7ff) bytes += 2;
    else if (code >= 0xd800 && code <= 0xdbff
      && i + 1 < value.length
      && value.charCodeAt(i + 1) >= 0xdc00
      && value.charCodeAt(i + 1) <= 0xdfff) {
      bytes += 4;
      i++;
    } else bytes += 3;
    if (bytes > limit) return true;
  }
  return false;
}

function remove(storage: StorageAccess | null, key: string): boolean {
  if (storage == null) return true;
  try {
    storage.removeItem(key);
    return true;
  } catch {
    return false;
  }
}

function readBounded(storage: StorageAccess | null, key: string, maxBytes: number): string | null {
  if (storage == null) return null;
  try {
    const raw = storage.getItem(key);
    if (raw == null) return null;
    if (exceedsUTF8Limit(raw, maxBytes)) {
      remove(storage, key);
      return null;
    }
    return raw;
  } catch {
    return null;
  }
}

function writeBounded(storage: StorageAccess | null, key: string, value: string, maxBytes: number): boolean {
  if (storage == null) return false;
  if (exceedsUTF8Limit(value, maxBytes)) {
    remove(storage, key);
    return false;
  }
  try {
    storage.setItem(key, value);
    return true;
  } catch {
    return false;
  }
}

function parseJSON<T>(
  storage: StorageAccess | null,
  key: string,
  maxBytes: number,
  validate: (value: unknown) => T | null,
): T | null {
  const raw = readBounded(storage, key, maxBytes);
  if (raw == null) return null;
  try {
    const value = validate(JSON.parse(raw) as unknown);
    if (value != null) return value;
  } catch {
    // Invalid JSON is treated the same as a schema violation.
  }
  remove(storage, key);
  return null;
}

function writeJSON(storage: StorageAccess | null, key: string, maxBytes: number, value: unknown): boolean {
  try {
    return writeBounded(storage, key, JSON.stringify(value), maxBytes);
  } catch {
    remove(storage, key);
    return false;
  }
}

function exactKeys(value: Record<string, unknown>, expected: readonly string[]): boolean {
  const keys = Object.keys(value);
  return keys.length === expected.length && expected.every((key) => Object.prototype.hasOwnProperty.call(value, key));
}

function validPath(value: unknown): value is string {
  return typeof value === "string"
    && value.length > 0
    && value.length <= WORKSPACE_STORAGE_LIMITS.pathCodeUnits
    && !value.includes("\0");
}

function validatePreferences(value: unknown): WorkspacePreferences | null {
  if (typeof value !== "object" || value == null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  if (!exactKeys(record, ["version", "rememberPaths", "restoreSession"])) return null;
  if (record.version !== 1 || typeof record.rememberPaths !== "boolean" || typeof record.restoreSession !== "boolean") return null;
  if (record.restoreSession && !record.rememberPaths) return null;
  return { rememberPaths: record.rememberPaths, restoreSession: record.restoreSession };
}

function readWorkspacePreferences(storage: StorageAccess | null): WorkspacePreferences | null {
  return parseJSON(
    storage,
    LOCAL_STORAGE_KEYS.workspacePreferences,
    WORKSPACE_STORAGE_LIMITS.preferenceBytes,
    validatePreferences,
  );
}

function validatePathArray(value: unknown, maxCount: number): string[] | null {
  if (!Array.isArray(value) || value.length > maxCount) return null;
  const paths: string[] = [];
  const seen = new Set<string>();
  for (const entry of value) {
    if (!validPath(entry) || seen.has(entry)) return null;
    seen.add(entry);
    paths.push(entry);
  }
  return paths;
}

function validBookmark(value: unknown): value is StoredBookmark {
  if (typeof value !== "object" || value == null || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  return exactKeys(record, ["offset", "label"])
    && typeof record.offset === "number"
    && Number.isSafeInteger(record.offset)
    && record.offset >= 0
    && typeof record.label === "string"
    && record.label.length <= WORKSPACE_STORAGE_LIMITS.bookmarkLabelCodeUnits;
}

function validateBookmarks(value: unknown): StoredBookmarks | null {
  if (typeof value !== "object" || value == null || Array.isArray(value)) return null;
  const entries = Object.entries(value as Record<string, unknown>);
  if (entries.length > WORKSPACE_STORAGE_LIMITS.bookmarkFileCount) return null;

  const result = emptyBookmarks();
  let total = 0;
  for (const [path, rawBookmarks] of entries) {
    if (!validPath(path) || !Array.isArray(rawBookmarks)
      || rawBookmarks.length > WORKSPACE_STORAGE_LIMITS.bookmarksPerFile) return null;
    total += rawBookmarks.length;
    if (total > WORKSPACE_STORAGE_LIMITS.bookmarkTotalCount) return null;
    const offsets = new Set<number>();
    const bookmarks: StoredBookmark[] = [];
    for (const bookmark of rawBookmarks) {
      if (!validBookmark(bookmark) || offsets.has(bookmark.offset)) return null;
      offsets.add(bookmark.offset);
      bookmarks.push({ offset: bookmark.offset, label: bookmark.label });
    }
    bookmarks.sort((a, b) => a.offset - b.offset);
    if (bookmarks.length > 0) result[path] = bookmarks;
  }
  return result;
}

function loadTheme(storage: StorageAccess | null): QuarryTheme {
  const raw = readBounded(storage, LOCAL_STORAGE_KEYS.theme, WORKSPACE_STORAGE_LIMITS.themeBytes);
  if (raw === "dark" || raw === "light") return raw;
  if (raw != null) remove(storage, LOCAL_STORAGE_KEYS.theme);
  return "dark";
}

function loadByteOffsets(storage: StorageAccess | null): boolean {
  const raw = readBounded(storage, LOCAL_STORAGE_KEYS.byteOffsets, WORKSPACE_STORAGE_LIMITS.byteOffsetBytes);
  if (raw === "0" || raw === "1") return raw === "1";
  if (raw != null) remove(storage, LOCAL_STORAGE_KEYS.byteOffsets);
  return false;
}

export function saveTheme(storage: StorageAccess | null, theme: QuarryTheme): boolean {
  if (theme !== "dark" && theme !== "light") {
    remove(storage, LOCAL_STORAGE_KEYS.theme);
    return false;
  }
  return writeBounded(storage, LOCAL_STORAGE_KEYS.theme, theme, WORKSPACE_STORAGE_LIMITS.themeBytes);
}

export function saveByteOffsets(storage: StorageAccess | null, show: boolean): boolean {
  if (typeof show !== "boolean") {
    remove(storage, LOCAL_STORAGE_KEYS.byteOffsets);
    return false;
  }
  return writeBounded(storage, LOCAL_STORAGE_KEYS.byteOffsets, show ? "1" : "0", WORKSPACE_STORAGE_LIMITS.byteOffsetBytes);
}

export function clearWorkspacePathData(storage: StorageAccess | null): boolean {
  let cleared = true;
  for (const key of [LOCAL_STORAGE_KEYS.recentPaths, LOCAL_STORAGE_KEYS.sessionPaths, LOCAL_STORAGE_KEYS.bookmarks]) {
    if (!remove(storage, key)) cleared = false;
  }
  return cleared;
}

/**
 * Load every local payload through a strict, bounded schema. A missing or
 * invalid privacy preference is an opt-out and removes legacy path data.
 */
export function loadLocalSettings(storage: StorageAccess | null): LocalSettingsSnapshot {
  const theme = loadTheme(storage);
  const showByteOffsets = loadByteOffsets(storage);
  const workspace = readWorkspacePreferences(storage) ?? cloneOff();

  if (!workspace.rememberPaths) {
    clearWorkspacePathData(storage);
    return { theme, showByteOffsets, workspace, recentPaths: [], sessionPaths: [], bookmarks: emptyBookmarks() };
  }

  const recentPaths = parseJSON(
    storage,
    LOCAL_STORAGE_KEYS.recentPaths,
    WORKSPACE_STORAGE_LIMITS.recentBytes,
    (value) => validatePathArray(value, WORKSPACE_STORAGE_LIMITS.recentCount),
  ) ?? [];
  const bookmarks = parseJSON(
    storage,
    LOCAL_STORAGE_KEYS.bookmarks,
    WORKSPACE_STORAGE_LIMITS.bookmarkBytes,
    validateBookmarks,
  ) ?? emptyBookmarks();

  let sessionPaths: string[] = [];
  if (workspace.restoreSession) {
    sessionPaths = parseJSON(
      storage,
      LOCAL_STORAGE_KEYS.sessionPaths,
      WORKSPACE_STORAGE_LIMITS.sessionBytes,
      (value) => validatePathArray(value, WORKSPACE_STORAGE_LIMITS.sessionCount),
    ) ?? [];
  } else {
    remove(storage, LOCAL_STORAGE_KEYS.sessionPaths);
  }
  return { theme, showByteOffsets, workspace, recentPaths, sessionPaths, bookmarks };
}

/** Apply a preference change; a failed write falls back to path memory off. */
export function commitWorkspacePreferences(
  storage: StorageAccess | null,
  requested: WorkspacePreferences,
): PreferenceCommitResult {
  const valid = validatePreferences({ version: 1, ...requested });
  if (valid == null || !valid.rememberPaths) {
    const cleared = clearWorkspacePathData(storage);
    const preferenceRemoved = remove(storage, LOCAL_STORAGE_KEYS.workspacePreferences);
    return { preferences: cloneOff(), persisted: cleared && preferenceRemoved };
  }

  let sessionCleared = true;
  if (!valid.restoreSession) sessionCleared = remove(storage, LOCAL_STORAGE_KEYS.sessionPaths);
  const stored = sessionCleared && writeJSON(
    storage,
    LOCAL_STORAGE_KEYS.workspacePreferences,
    WORKSPACE_STORAGE_LIMITS.preferenceBytes,
    { version: 1, ...valid },
  );
  if (stored) return { preferences: valid, persisted: true };

  // Never leave an old opt-in authoritative after a failed privacy update.
  clearWorkspacePathData(storage);
  remove(storage, LOCAL_STORAGE_KEYS.workspacePreferences);
  return { preferences: cloneOff(), persisted: false };
}

export function nextRecentPaths(current: readonly string[], path: string): string[] {
  if (!validPath(path)) return normalizePathList(current, WORKSPACE_STORAGE_LIMITS.recentCount);
  return [path, ...current.filter((entry) => entry !== path && validPath(entry))]
    .slice(0, WORKSPACE_STORAGE_LIMITS.recentCount);
}

function normalizePathList(value: unknown, maxCount: number): string[] {
  if (!Array.isArray(value)) return [];
  const result: string[] = [];
  const seen = new Set<string>();
  for (const entry of value) {
    if (!validPath(entry) || seen.has(entry)) continue;
    seen.add(entry);
    result.push(entry);
    if (result.length === maxCount) break;
  }
  return result;
}

export function normalizeBookmarks(value: unknown): StoredBookmarks {
  if (typeof value !== "object" || value == null || Array.isArray(value)) return emptyBookmarks();
  const result = emptyBookmarks();
  let files = 0;
  let total = 0;
  for (const [path, rawBookmarks] of Object.entries(value as Record<string, unknown>)) {
    if (files >= WORKSPACE_STORAGE_LIMITS.bookmarkFileCount || !validPath(path) || !Array.isArray(rawBookmarks)) continue;
    const offsets = new Set<number>();
    const bookmarks: StoredBookmark[] = [];
    for (const bookmark of rawBookmarks) {
      if (total >= WORKSPACE_STORAGE_LIMITS.bookmarkTotalCount
        || bookmarks.length >= WORKSPACE_STORAGE_LIMITS.bookmarksPerFile) break;
      if (!validBookmark(bookmark) || offsets.has(bookmark.offset)) continue;
      offsets.add(bookmark.offset);
      bookmarks.push({ offset: bookmark.offset, label: bookmark.label });
      total++;
    }
    if (bookmarks.length > 0) {
      bookmarks.sort((a, b) => a.offset - b.offset);
      result[path] = bookmarks;
      files++;
    }
  }
  return result;
}

export function persistRecentPaths(
  storage: StorageAccess | null,
  preferences: WorkspacePreferences,
  paths: readonly string[],
): boolean {
  if (!preferences.rememberPaths) return remove(storage, LOCAL_STORAGE_KEYS.recentPaths);
  if (readWorkspacePreferences(storage)?.rememberPaths !== true) {
    remove(storage, LOCAL_STORAGE_KEYS.recentPaths);
    return false;
  }
  const normalized = normalizePathList(paths, WORKSPACE_STORAGE_LIMITS.recentCount);
  if (normalized.length === 0) return remove(storage, LOCAL_STORAGE_KEYS.recentPaths);
  return writeJSON(storage, LOCAL_STORAGE_KEYS.recentPaths, WORKSPACE_STORAGE_LIMITS.recentBytes, normalized);
}

export function persistSessionPaths(
  storage: StorageAccess | null,
  preferences: WorkspacePreferences,
  paths: readonly string[],
): boolean {
  if (!preferences.rememberPaths || !preferences.restoreSession) {
    return remove(storage, LOCAL_STORAGE_KEYS.sessionPaths);
  }
  const stored = readWorkspacePreferences(storage);
  if (stored?.rememberPaths !== true || stored.restoreSession !== true) {
    remove(storage, LOCAL_STORAGE_KEYS.sessionPaths);
    return false;
  }
  const normalized = normalizePathList(paths, WORKSPACE_STORAGE_LIMITS.sessionCount);
  if (normalized.length === 0) return remove(storage, LOCAL_STORAGE_KEYS.sessionPaths);
  return writeJSON(storage, LOCAL_STORAGE_KEYS.sessionPaths, WORKSPACE_STORAGE_LIMITS.sessionBytes, normalized);
}

export function persistBookmarks(
  storage: StorageAccess | null,
  preferences: WorkspacePreferences,
  bookmarks: StoredBookmarks,
): boolean {
  if (!preferences.rememberPaths) return remove(storage, LOCAL_STORAGE_KEYS.bookmarks);
  if (readWorkspacePreferences(storage)?.rememberPaths !== true) {
    remove(storage, LOCAL_STORAGE_KEYS.bookmarks);
    return false;
  }
  const normalized = normalizeBookmarks(bookmarks);
  if (Object.keys(normalized).length === 0) return remove(storage, LOCAL_STORAGE_KEYS.bookmarks);
  return writeJSON(storage, LOCAL_STORAGE_KEYS.bookmarks, WORKSPACE_STORAGE_LIMITS.bookmarkBytes, normalized);
}
