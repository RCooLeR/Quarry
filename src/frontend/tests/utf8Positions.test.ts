import { describe, expect, it } from "vitest";
import { utf8ByteRangeToCodeUnits } from "../src/editor/utf8Positions";

describe("exact edit-window UTF-8 positions", () => {
  it("maps all byte boundaries and rejects partial scalars in mixed Unicode", () => {
    const text = "aé中😀\r\n\ud800z\udfff";
    const encoder = new TextEncoder();
    const boundaries = new Map<number, number>([[0, 0]]);
    let bytes = 0;
    let codeUnits = 0;
    for (const scalar of text) {
      bytes += encoder.encode(scalar).length;
      codeUnits += scalar.length;
      boundaries.set(bytes, codeUnits);
    }
    for (let from = 0; from <= bytes; from++) {
      for (let to = from; to <= bytes; to++) {
        const expected = boundaries.has(from) && boundaries.has(to)
          ? { from: boundaries.get(from), to: boundaries.get(to) }
          : null;
        expect(utf8ByteRangeToCodeUnits(text, from, to - from)).toEqual(expected);
      }
    }
  });

  it("maps the end of a megabyte ASCII window without per-character buffers", () => {
    const text = "x".repeat(1 << 20) + "😀";
    expect(utf8ByteRangeToCodeUnits(text, 1 << 20, 4)).toEqual({ from: 1 << 20, to: (1 << 20) + 2 });
  });

  it("rejects invalid, overflowing and out-of-window ranges", () => {
    for (const [offset, length] of [[-1, 0], [0, -1], [0.5, 1], [0, NaN], [Infinity, 0], [Number.MAX_SAFE_INTEGER, 1], [4, 0], [0, 4]]) {
      expect(utf8ByteRangeToCodeUnits("abc", offset, length)).toBeNull();
    }
    expect(utf8ByteRangeToCodeUnits("", 0, 0)).toEqual({ from: 0, to: 0 });
  });
});
