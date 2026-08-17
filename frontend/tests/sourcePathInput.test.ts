import { describe, expect, it } from "vitest";
import {
  hasSourcePathInput,
  isBoundedSourcePathInput,
  SOURCE_PATH_INPUT_MAX_CODE_UNITS,
} from "../src/sourcePathInput";

describe("source path input", () => {
  it("rejects only the empty string and preserves whitespace as meaningful path data", () => {
    expect(hasSourcePathInput("")).toBe(false);
    expect(hasSourcePathInput(" ")).toBe(true);
    expect(hasSourcePathInput("  /tmp/file  ")).toBe(true);
  });

  it("bounds values before they enter path-bearing UI and bridge state", () => {
    const maximum = "x".repeat(SOURCE_PATH_INPUT_MAX_CODE_UNITS);
    expect(isBoundedSourcePathInput(maximum)).toBe(true);
    expect(isBoundedSourcePathInput(maximum + "x")).toBe(false);
    expect(isBoundedSourcePathInput("")).toBe(false);
  });
});
