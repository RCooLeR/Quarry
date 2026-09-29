import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

describe("visible source text", () => {
  it("keeps the SQL find-and-replace menu ellipsis correctly encoded", () => {
    const appSource = readFileSync(resolve(process.cwd(), "src/App.tsx"), "utf8");
    expect(appSource).toContain('label: "Find & replace…"');
    expect(appSource).not.toContain("вЂ¦");
  });
});
