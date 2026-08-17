import { describe, expect, it } from "vitest";
import { normalizeSurfaceForFile, supportsDataTools } from "../src/activeSurface";

describe("active-file surface normalization", () => {
  it("exposes data tools only for CSV, TSV, and SQL", () => {
    expect(["csv", "TSV", "Sql"].every(supportsDataTools)).toBe(true);
    expect(["text", "json", "binary", ""].some(supportsDataTools)).toBe(false);
  });

  it("closes incompatible and tab-local surfaces on a file switch", () => {
    expect(normalizeSurfaceForFile("text", false, {
      gridView: true,
      hexView: true,
      toolsOpen: true,
      diffOpen: true,
    }, true)).toEqual({
      gridView: false,
      hexView: false,
      toolsOpen: false,
      diffOpen: false,
    });
  });

  it("preserves compatible same-file surfaces but closes a grid after a type change", () => {
    const current = { gridView: true, hexView: false, toolsOpen: true, diffOpen: true };
    expect(normalizeSurfaceForFile("csv", true, current, false)).toEqual(current);
    expect(normalizeSurfaceForFile("sql", true, current, false)).toEqual({
      gridView: false,
      hexView: false,
      toolsOpen: true,
      diffOpen: true,
    });
  });
});
