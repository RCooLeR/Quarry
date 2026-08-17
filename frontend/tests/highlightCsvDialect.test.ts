import { describe, expect, it } from "vitest";
import {
  csvFieldRanges,
  detectCsvDelimiter,
  highlightFor,
  MAX_CSV_DELIMITER_DETECTION_CODE_UNITS,
  MAX_CSV_HIGHLIGHT_RANGES,
} from "../src/editor/highlight";

describe("rainbow CSV dialect isolation", () => {
  it("creates a fresh plugin configuration for every file", () => {
    const comma = highlightFor("csv", "first.csv", ",");
    const semicolon = highlightFor("csv", "second.csv", ";");
    const commaAgain = highlightFor("csv", "first.csv", ",");
    expect(comma).not.toBe(semicolon);
    expect(comma).not.toBe(commaAgain);
    expect(comma[0]).not.toBe(semicolon[0]);
  });

  it("uses the configured separator without splitting quoted separators", () => {
    expect(csvFieldRanges('a;"b;c";d', ";")).toEqual([[0, 1], [2, 7], [8, 9]]);
    expect(csvFieldRanges("a\tb\tc", "\t")).toEqual([[0, 1], [2, 3], [4, 5]]);
  });

  it("bounds field-range materialization for delimiter-dense editor windows", () => {
    const ranges = csvFieldRanges("x,".repeat(MAX_CSV_HIGHLIGHT_RANGES * 8), ",");

    expect(ranges).toHaveLength(MAX_CSV_HIGHLIGHT_RANGES);
    expect(ranges[0]).toEqual([0, 1]);
    expect(ranges.at(-1)).toEqual([
      (MAX_CSV_HIGHLIGHT_RANGES - 1) * 2,
      (MAX_CSV_HIGHLIGHT_RANGES - 1) * 2 + 1,
    ]);
  });

  it("detects separators with fixed counters over a bounded prefix", () => {
    expect(detectCsvDelimiter("a;b;c;d")).toBe(";");
    const boundedPrefix = ",".repeat(MAX_CSV_DELIMITER_DETECTION_CODE_UNITS);
    const ignoredTail = ";".repeat(MAX_CSV_DELIMITER_DETECTION_CODE_UNITS * 2);

    expect(detectCsvDelimiter(boundedPrefix + ignoredTail)).toBe(",");
  });
});
