import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { boundedPaletteTables, MAX_PALETTE_TABLES } from "../src/productLimits";

describe("bounded product convenience surfaces", () => {
  it("caps analyzed-table palette commands without changing source order", () => {
    const tables = Array.from({ length: MAX_PALETTE_TABLES + 25 }, (_, index) => ({ index }));
    const bounded = boundedPaletteTables(tables);

    expect(bounded).toHaveLength(MAX_PALETTE_TABLES);
    expect(bounded[0]).toBe(tables[0]);
    expect(bounded[MAX_PALETTE_TABLES - 1]).toBe(tables[MAX_PALETTE_TABLES - 1]);
    expect(tables).toHaveLength(MAX_PALETTE_TABLES + 25);
  });

  it("keeps the published feature contract aligned with the source cap", () => {
    const features = readFileSync(resolve(process.cwd(), "../../docs/features.md"), "utf8");
    expect(features).toContain(`at most the first ${MAX_PALETTE_TABLES} tables`);
  });
});
