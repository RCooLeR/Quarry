import { describe, expect, it } from "vitest";

import { presentSearchPage } from "../src/searchPresentation";

describe("whole-file search presentation", () => {
  it("keeps an empty timeout distinct from a successful zero-match result", () => {
    expect(presentSearchPage({ timedOut: true, hits: null })).toEqual({
      hits: null,
      info: "Timed out — narrow the query",
    });
  });

  it("does not render partial timeout hits as a complete result", () => {
    const hit = { offset: 42 };
    expect(presentSearchPage({ timedOut: true, hits: [hit] })).toEqual({
      hits: null,
      info: "Timed out — 1 partial matches were not shown",
    });
  });

  it("normalizes nullable successful hits and preserves truncation", () => {
    expect(presentSearchPage({ hits: null })).toEqual({ hits: [], info: "0 matches" });
    expect(presentSearchPage({ hits: [{ offset: 1 }], truncated: true })).toEqual({
      hits: [{ offset: 1 }],
      info: "1+ matches",
    });
  });
});
