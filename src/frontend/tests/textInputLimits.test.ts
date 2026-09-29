import { describe, expect, it } from "vitest";
import {
  boundedUtf8Text,
  SEARCH_PLAIN_INPUT_MAX_BYTES,
  SEARCH_REGEX_INPUT_MAX_BYTES,
  SQL_FIND_INPUT_MAX_BYTES,
  SQL_REPLACEMENT_INPUT_MAX_BYTES,
} from "../src/textInputLimits";

describe("UTF-8 text input limits", () => {
  it("stays aligned with the backend request ceilings", () => {
    expect(SEARCH_PLAIN_INPUT_MAX_BYTES).toBe(1024 * 1024);
    expect(SEARCH_REGEX_INPUT_MAX_BYTES).toBe(64 * 1024);
    expect(SQL_FIND_INPUT_MAX_BYTES).toBe(64 * 1024);
    expect(SQL_REPLACEMENT_INPUT_MAX_BYTES).toBe(256 * 1024);
  });

  it("keeps the longest UTF-8 byte-bounded scalar prefix", () => {
    const exact = boundedUtf8Text("Aé🙂", 7);
    expect(exact).toEqual({ value: "Aé🙂", truncated: false });

    const bounded = boundedUtf8Text("Aé🙂Z", 6);
    expect(bounded).toEqual({ value: "Aé", truncated: true });
    expect(new TextEncoder().encode(bounded.value)).toHaveLength(3);
  });

  it("does not split surrogate pairs and accounts for lone surrogates like TextEncoder", () => {
    expect(boundedUtf8Text("🙂tail", 3)).toEqual({ value: "", truncated: true });
    expect(boundedUtf8Text("🙂tail", 4)).toEqual({ value: "🙂", truncated: true });
    expect(boundedUtf8Text("\ud83d", 2)).toEqual({ value: "", truncated: true });
    expect(boundedUtf8Text("\ud83d", 3)).toEqual({ value: "\ud83d", truncated: false });
  });

  it("rejects invalid ceilings", () => {
    expect(() => boundedUtf8Text("value", -1)).toThrow(RangeError);
    expect(() => boundedUtf8Text("value", 1.5)).toThrow(RangeError);
  });
});
