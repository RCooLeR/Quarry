import { describe, expect, it } from "vitest";
import {
  detectedCsvDialect,
  fallbackCsvDialect,
  isValidCsvDelimiter,
  mergeCsvDetection,
  overrideCsvDialect,
} from "../src/csvDialect";

describe("per-file CSV dialect", () => {
  it("uses type-specific fallbacks without sharing state", () => {
    expect(fallbackCsvDialect("csv")).toEqual({ delimiter: ",", hasHeader: false, origin: "fallback" });
    expect(fallbackCsvDialect("tsv")).toEqual({ delimiter: "\t", hasHeader: false, origin: "fallback" });
  });

  it("keeps an explicit override when a detector finishes later", () => {
    const explicit = overrideCsvDialect(";", true);
    const detected = detectedCsvDialect("csv", ",", false);
    expect(mergeCsvDetection(explicit, detected)).toBe(explicit);
    expect(mergeCsvDetection(undefined, detected)).toBe(detected);
  });

  it("accepts one Unicode separator and rejects ambiguous separators", () => {
    expect(isValidCsvDelimiter("¦")).toBe(true);
    for (const invalid of ["", "::", "\0", "\r", "\n"]) {
      expect(isValidCsvDelimiter(invalid)).toBe(false);
      expect(() => overrideCsvDialect(invalid, false)).toThrow(/separator/);
    }
  });
});
