import { describe, expect, it } from "vitest";

import {
  createCsvGridCursorHistory,
  csvGridCursorHistoryExhausted,
  MAX_CSV_GRID_CURSOR_HISTORY,
  previousCsvGridCursor,
  updateCsvGridCursorHistory,
} from "../src/CsvGrid";

describe("CSV grid parser cursor history", () => {
  it("reuses exact emitted predecessor cursors instead of subtracting bytes", () => {
    const history = createCsvGridCursorHistory();
    updateCsvGridCursorHistory(history, "reset", 0);
    updateCsvGridCursorHistory(history, "forward", 0);
    updateCsvGridCursorHistory(history, "forward", 200_000);

    expect(previousCsvGridCursor(history)).toBe(200_000);
    updateCsvGridCursorHistory(history, "back", 400_000);
    expect(previousCsvGridCursor(history)).toBe(0);
    expect(previousCsvGridCursor(history)).not.toBe(400_000 - 128 * 1024);
  });

  it("mutates one fixed history object instead of cloning it per window", () => {
    const history = createCsvGridCursorHistory();

    expect(updateCsvGridCursorHistory(history, "forward", 0)).toBe(history);
    expect(updateCsvGridCursorHistory(history, "forward", 100)).toBe(history);
    expect(updateCsvGridCursorHistory(history, "back", 200)).toBe(history);
    expect(updateCsvGridCursorHistory(history, "reset", 300)).toBe(history);
  });

  it("bounds retained cursors and reports when older exact history is exhausted", () => {
    const history = createCsvGridCursorHistory();
    const extraCursors = 17;
    const cursorCount = MAX_CSV_GRID_CURSOR_HISTORY + extraCursors;

    for (let cursor = 0; cursor < cursorCount; cursor++) {
      updateCsvGridCursorHistory(history, "forward", cursor * 10_000);
    }

    expect(history.values).toHaveLength(MAX_CSV_GRID_CURSOR_HISTORY);
    expect(history.size).toBe(MAX_CSV_GRID_CURSOR_HISTORY);
    expect(history.truncated).toBe(true);
    expect(csvGridCursorHistoryExhausted(history, false)).toBe(false);

    for (let cursor = cursorCount - 1; cursor >= extraCursors; cursor--) {
      expect(previousCsvGridCursor(history)).toBe(cursor * 10_000);
      updateCsvGridCursorHistory(history, "back", 0);
    }

    expect(previousCsvGridCursor(history)).toBeUndefined();
    expect(history.size).toBe(0);
    expect(history.values.length).toBeLessThanOrEqual(MAX_CSV_GRID_CURSOR_HISTORY);
    expect(csvGridCursorHistoryExhausted(history, false)).toBe(true);
    expect(csvGridCursorHistoryExhausted(history, true)).toBe(false);
  });

  it("clears retained and truncated state when delimiter/header navigation resets the grid", () => {
    const history = createCsvGridCursorHistory();
    for (let cursor = 0; cursor <= MAX_CSV_GRID_CURSOR_HISTORY; cursor++) {
      updateCsvGridCursorHistory(history, "forward", cursor);
    }
    expect(history.truncated).toBe(true);

    updateCsvGridCursorHistory(history, "reset", 300);

    expect(history).toEqual({ values: [], head: 0, size: 0, truncated: false });
    expect(previousCsvGridCursor(history)).toBeUndefined();
    expect(csvGridCursorHistoryExhausted(history, false)).toBe(false);
  });
});
