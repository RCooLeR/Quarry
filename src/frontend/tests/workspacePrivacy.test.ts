import { beforeEach, describe, expect, it } from "vitest";

import {
  LOCAL_STORAGE_KEYS,
  WORKSPACE_STORAGE_LIMITS,
  clearWorkspacePathData,
  commitWorkspacePreferences,
  loadLocalSettings,
  nextRecentPaths,
  normalizeBookmarks,
  persistBookmarks,
  persistRecentPaths,
  persistSessionPaths,
  saveByteOffsets,
  saveTheme,
} from "../src/workspacePrivacy";

const optIn = (restoreSession = true) => JSON.stringify({
  version: 1,
  rememberPaths: true,
  restoreSession,
});

describe("workspace privacy storage", () => {
  beforeEach(() => localStorage.clear());

  it("migrates legacy path data to a privacy-conservative fresh default", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.theme, "light");
    localStorage.setItem(LOCAL_STORAGE_KEYS.byteOffsets, "1");
    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\private\\recent.sql"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, JSON.stringify(["C:\\private\\session.sql"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, JSON.stringify({
      "C:\\private\\bookmark.sql": [{ offset: 4, label: "line ~1" }],
    }));

    const loaded = loadLocalSettings(localStorage);

    expect(loaded).toMatchObject({
      theme: "light",
      showByteOffsets: true,
      workspace: { rememberPaths: false, restoreSession: false },
      recentPaths: [],
      sessionPaths: [],
    });
    expect(Object.keys(loaded.bookmarks)).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });

  it("loads only strictly valid opted-in recent, session, and bookmark records", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.workspacePreferences, optIn(true));
    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\one.sql", "C:\\two.sql"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, JSON.stringify(["C:\\two.sql"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, JSON.stringify({
      "C:\\two.sql": [
        { offset: 20, label: "line ~3" },
        { offset: 5, label: "line ~1" },
      ],
    }));

    const loaded = loadLocalSettings(localStorage);

    expect(loaded.workspace).toEqual({ rememberPaths: true, restoreSession: true });
    expect(loaded.recentPaths).toEqual(["C:\\one.sql", "C:\\two.sql"]);
    expect(loaded.sessionPaths).toEqual(["C:\\two.sql"]);
    expect(loaded.bookmarks["C:\\two.sql"]).toEqual([
      { offset: 5, label: "line ~1" },
      { offset: 20, label: "line ~3" },
    ]);
  });

  it("does not retain or load a session list when automatic restore is off", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.workspacePreferences, optIn(false));
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, JSON.stringify(["C:\\private.sql"]));

    const loaded = loadLocalSettings(localStorage);

    expect(loaded.workspace).toEqual({ rememberPaths: true, restoreSession: false });
    expect(loaded.sessionPaths).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
  });

  it("rejects invalid scalar settings and never writes invalid scalar values", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.theme, "sepia".repeat(8));
    localStorage.setItem(LOCAL_STORAGE_KEYS.byteOffsets, "true");

    const loaded = loadLocalSettings(localStorage);

    expect(loaded.theme).toBe("dark");
    expect(loaded.showByteOffsets).toBe(false);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.theme)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.byteOffsets)).toBeNull();
    expect(saveTheme(localStorage, "light")).toBe(true);
    expect(saveByteOffsets(localStorage, true)).toBe(true);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.theme)).toBe("light");
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.byteOffsets)).toBe("1");
  });

  it("rejects malformed, duplicate, oversized, and over-count path arrays", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.workspacePreferences, optIn(true));
    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\same", "C:\\same"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, JSON.stringify(
      Array.from({ length: WORKSPACE_STORAGE_LIMITS.sessionCount + 1 }, (_, i) => `C:\\${i}`),
    ));

    const loaded = loadLocalSettings(localStorage);

    expect(loaded.recentPaths).toEqual([]);
    expect(loaded.sessionPaths).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();

    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify([
      "x".repeat(WORKSPACE_STORAGE_LIMITS.pathCodeUnits + 1),
    ]));
    expect(loadLocalSettings(localStorage).recentPaths).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
  });

  it("rejects bookmark schema, count, and safe-integer violations as a whole", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.workspacePreferences, optIn(true));
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, JSON.stringify({
      "C:\\bad.sql": [{ offset: Number.MAX_SAFE_INTEGER + 1, label: "bad", extra: true }],
    }));
    expect(Object.keys(loadLocalSettings(localStorage).bookmarks)).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();

    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, JSON.stringify({
      "C:\\many.sql": Array.from(
        { length: WORKSPACE_STORAGE_LIMITS.bookmarksPerFile + 1 },
        (_, offset) => ({ offset, label: "x" }),
      ),
    }));
    expect(Object.keys(loadLocalSettings(localStorage).bookmarks)).toEqual([]);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });

  it("rejects malformed and oversized preference payloads and clears path data", () => {
    for (const invalid of [
      "not-json",
      JSON.stringify({ version: 1, rememberPaths: false, restoreSession: true }),
      JSON.stringify({ version: 1, rememberPaths: true, restoreSession: false, unknown: "x" }),
      " ".repeat(WORKSPACE_STORAGE_LIMITS.preferenceBytes + 1),
    ]) {
      localStorage.setItem(LOCAL_STORAGE_KEYS.workspacePreferences, invalid);
      localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\private.sql"]));
      const loaded = loadLocalSettings(localStorage);
      expect(loaded.workspace).toEqual({ rememberPaths: false, restoreSession: false });
      expect(localStorage.getItem(LOCAL_STORAGE_KEYS.workspacePreferences)).toBeNull();
      expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    }
  });

  it("never writes path payloads while memory is disabled", () => {
    const off = { rememberPaths: false, restoreSession: false };
    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, "legacy");
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, "legacy");
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, "legacy");

    expect(persistRecentPaths(localStorage, off, ["C:\\recent.sql"])).toBe(true);
    expect(persistSessionPaths(localStorage, off, ["C:\\session.sql"])).toBe(true);
    expect(persistBookmarks(localStorage, off, {
      "C:\\bookmark.sql": [{ offset: 0, label: "line ~1" }],
    })).toBe(true);

    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });

  it("requires the persisted opt-in at write time so stale callers cannot recreate paths", () => {
    const stale = { rememberPaths: true, restoreSession: true };
    expect(persistRecentPaths(localStorage, stale, ["C:\\recent.sql"])).toBe(false);
    expect(persistSessionPaths(localStorage, stale, ["C:\\session.sql"])).toBe(false);
    expect(persistBookmarks(localStorage, stale, {
      "C:\\bookmark.sql": [{ offset: 0, label: "line ~1" }],
    })).toBe(false);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });

  it("stores explicit opt-in, bounds persisted lists, and clears on opt-out", () => {
    const enabled = commitWorkspacePreferences(localStorage, { rememberPaths: true, restoreSession: true });
    expect(enabled).toEqual({
      preferences: { rememberPaths: true, restoreSession: true },
      persisted: true,
    });

    const paths = Array.from(
      { length: WORKSPACE_STORAGE_LIMITS.sessionCount + 10 },
      (_, i) => `C:\\file-${i}.sql`,
    );
    expect(persistSessionPaths(localStorage, enabled.preferences, paths)).toBe(true);
    expect(JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths) ?? "[]")).toHaveLength(
      WORKSPACE_STORAGE_LIMITS.sessionCount,
    );

    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\recent.sql"]));
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, JSON.stringify({
      "C:\\bookmark.sql": [{ offset: 1, label: "x" }],
    }));
    const disabled = commitWorkspacePreferences(localStorage, { rememberPaths: false, restoreSession: false });
    expect(disabled).toEqual({
      preferences: { rememberPaths: false, restoreSession: false },
      persisted: true,
    });
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.workspacePreferences)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });

  it("fails closed when an opt-in preference cannot be written", () => {
    const data = new Map<string, string>([
      [LOCAL_STORAGE_KEYS.recentPaths, JSON.stringify(["C:\\private.sql"])],
    ]);
    const failingStorage = {
      getItem: (key: string) => data.get(key) ?? null,
      setItem: () => { throw new Error("denied"); },
      removeItem: (key: string) => { data.delete(key); },
    };

    const result = commitWorkspacePreferences(failingStorage, { rememberPaths: true, restoreSession: false });

    expect(result).toEqual({
      preferences: { rememberPaths: false, restoreSession: false },
      persisted: false,
    });
    expect(data.size).toBe(0);
  });

  it("keeps transient recent and bookmark state deterministic and bounded", () => {
    let recent: string[] = [];
    for (let i = 0; i < WORKSPACE_STORAGE_LIMITS.recentCount + 5; i++) {
      recent = nextRecentPaths(recent, `C:\\${i}.sql`);
    }
    recent = nextRecentPaths(recent, "C:\\8.sql");
    expect(recent).toHaveLength(WORKSPACE_STORAGE_LIMITS.recentCount);
    expect(recent[0]).toBe("C:\\8.sql");
    expect(new Set(recent).size).toBe(recent.length);

    const bookmarks = normalizeBookmarks({
      "C:\\one.sql": [
        { offset: 10, label: "ten" },
        { offset: 3, label: "three" },
        { offset: 3, label: "duplicate" },
        { offset: -1, label: "invalid" },
      ],
    });
    expect(bookmarks["C:\\one.sql"]).toEqual([
      { offset: 3, label: "three" },
      { offset: 10, label: "ten" },
    ]);
  });

  it("clears every path-bearing key with one operation", () => {
    localStorage.setItem(LOCAL_STORAGE_KEYS.recentPaths, "[]");
    localStorage.setItem(LOCAL_STORAGE_KEYS.sessionPaths, "[]");
    localStorage.setItem(LOCAL_STORAGE_KEYS.bookmarks, "{}");
    expect(clearWorkspacePathData(localStorage)).toBe(true);
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.recentPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.sessionPaths)).toBeNull();
    expect(localStorage.getItem(LOCAL_STORAGE_KEYS.bookmarks)).toBeNull();
  });
});
