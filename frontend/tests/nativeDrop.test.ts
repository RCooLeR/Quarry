import { describe, expect, it } from "vitest";
import { normalizeNativeDrop } from "../src/nativeDrop";
import { MAX_OPEN_FILE_SESSIONS } from "../src/productLimits";
import { SOURCE_PATH_INPUT_MAX_CODE_UNITS } from "../src/sourcePathInput";

describe("native file-drop normalization", () => {
  it("rejects legacy and malformed envelopes without traversing their contents", () => {
    expect(normalizeNativeDrop(["C:\\data\\one.sql"], 10)).toEqual({ paths: [], omitted: 1 });
    expect(normalizeNativeDrop({ paths: ["C:\\data\\one.sql"], omitted: -1 }, 10))
      .toEqual({ paths: [], omitted: 1 });
    expect(normalizeNativeDrop({ paths: "not-an-array", omitted: 0 }, 10))
      .toEqual({ paths: [], omitted: 1 });
  });

  it("bounds traversal and output to the product and remaining-session limits", () => {
    const paths = Array.from({ length: MAX_OPEN_FILE_SESSIONS + 72 }, (_, index) => `C:\\data\\${index}.sql`);
    const normalized = normalizeNativeDrop({ paths, omitted: 5 }, 3);
    expect(normalized.paths).toEqual(paths.slice(0, 3));
    expect(normalized.omitted).toBe(5 + 72 + MAX_OPEN_FILE_SESSIONS - 3);
  });

  it("counts invalid entries and preserves valid path text exactly", () => {
    expect(normalizeNativeDrop({
      paths: ["C:\\data\\one.sql", 42, "", " C:\\data\\two.sql "],
      omitted: 2,
    }, 10)).toEqual({
      paths: ["C:\\data\\one.sql", " C:\\data\\two.sql "],
      omitted: 4,
    });
  });

  it("omits an oversized path instead of retaining or sending it", () => {
    const oversized = "x".repeat(SOURCE_PATH_INPUT_MAX_CODE_UNITS + 1);
    expect(normalizeNativeDrop({ paths: [oversized, "C:\\data\\safe.sql"], omitted: 0 }, 2)).toEqual({
      paths: ["C:\\data\\safe.sql"],
      omitted: 1,
    });
  });
});
