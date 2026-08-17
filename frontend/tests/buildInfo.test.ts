import { describe, expect, it } from "vitest";
import { formatBuildInfo } from "../src/buildInfo";

describe("formatBuildInfo", () => {
  it("presents canonical release diagnostics", () => {
    expect(formatBuildInfo({
      applicationName: "Quarry Editor",
      version: "1.2.3-rc.1",
      commit: "abc123",
      buildDate: "2026-07-12T10:20:30Z",
    })).toContain("Quarry Editor 1.2.3-rc.1\nCommit: abc123\nBuilt: 2026-07-12T10:20:30Z");
  });

  it("never renders empty or undefined bridge values", () => {
    const text = formatBuildInfo({ applicationName: "", version: "" });
    expect(text).toContain("Quarry Editor unknown");
    expect(text).not.toContain("undefined");
  });
});
