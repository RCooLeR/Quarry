import { describe, expect, it } from "vitest";
import {
  MAX_DIFF_TEXT_CODE_UNITS,
  normalizeDiffWindow,
} from "../src/DiffView";
import {
  MAX_HEX_WINDOW_LINES,
  normalizeHexWindow,
} from "../src/HexView";

describe("bounded renderer bridge payloads", () => {
  it("accepts a well-formed bounded hex window", () => {
    expect(normalizeHexWindow({
      startByte: 16,
      nextByte: 32,
      atBof: false,
      atEof: true,
      lines: [{ offset: 16, hex: "41 42", ascii: "AB" }],
    })).toEqual({
      startByte: 16,
      nextByte: 32,
      atBof: false,
      atEof: true,
      lines: [{ offset: 16, hex: "41 42", ascii: "AB" }],
    });
  });

  it("rejects hex responses before an oversized row collection is normalized", () => {
    const oversized = new Array(MAX_HEX_WINDOW_LINES + 1).fill({ offset: 0, hex: "", ascii: "" });
    expect(() => normalizeHexWindow({
      startByte: 0,
      nextByte: 0,
      atBof: true,
      atEof: true,
      lines: oversized,
    })).toThrow(`exceeds ${MAX_HEX_WINDOW_LINES} rows`);
  });

  it("rejects oversized or malformed text inside a hex row", () => {
    expect(() => normalizeHexWindow({
      startByte: 0,
      nextByte: 16,
      atBof: true,
      atEof: false,
      lines: [{ offset: 0, hex: "x".repeat(65), ascii: "" }],
    })).toThrow("oversized hex-window lines[0].hex");
    expect(() => normalizeHexWindow({
      startByte: 0,
      nextByte: 16,
      atBof: true,
      atEof: false,
      lines: [{ offset: 0, hex: "00", ascii: "x".repeat(17) }],
    })).toThrow("oversized hex-window lines[0].ascii");
  });

  it("accepts a well-formed bounded diff window", () => {
    expect(normalizeDiffWindow({
      startByte: 0,
      nextByte: 4,
      original: "old\n",
      edited: "new\n",
      atBof: true,
      atEof: true,
    })).toMatchObject({ original: "old\n", edited: "new\n" });
  });

  it("rejects diff text that could construct an oversized MergeView", () => {
    expect(() => normalizeDiffWindow({
      startByte: 0,
      nextByte: 1,
      original: "x".repeat(MAX_DIFF_TEXT_CODE_UNITS + 1),
      edited: "",
      atBof: true,
      atEof: true,
    })).toThrow("oversized diff-window original");
  });
});
