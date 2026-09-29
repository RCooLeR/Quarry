import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(process.cwd(), "src/quarry.css"), "utf8");
const mainGo = readFileSync(resolve(process.cwd(), "../main.go"), "utf8");

function themeBlock(selector: string): string {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`missing theme block ${selector}`);
  const bodyStart = css.indexOf("{", start) + 1;
  const end = css.indexOf("\n}", bodyStart);
  return css.slice(bodyStart, end);
}

function variable(block: string, name: string): string {
  const match = block.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`));
  if (!match) throw new Error(`missing --${name}`);
  return match[1];
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((index) => Number.parseInt(hex.slice(index, index + 2), 16) / 255);
  const [red, green, blue] = channels.map((channel) => (
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4
  ));
  return red * 0.2126 + green * 0.7152 + blue * 0.0722;
}

function contrast(first: string, second: string): number {
  const a = luminance(first);
  const b = luminance(second);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

describe("theme and compact-layout safety contracts", () => {
  it.each([":root", ':root[data-theme="light"]'])("keeps semantic foregrounds readable in %s", (selector) => {
    const block = themeBlock(selector);
    const panel = variable(block, "panel");
    for (const foreground of ["text", "muted", "accent-fg", "danger", "warning"]) {
      expect(contrast(variable(block, foreground), panel), `${selector} --${foreground}`).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("defines high-contrast, reduced-motion, forced-color, and narrow-window behavior", () => {
    expect(css).toContain("@media (prefers-contrast: more)");
    expect(css).toContain("@media (prefers-reduced-motion: reduce)");
    expect(css).toContain("@media (forced-colors: active)");
    expect(css).toContain("@media (max-width: 520px)");
    expect(css).toContain("width: min(160px, 42vw)");
    expect(css).toContain(".q-grid-virtual-spacer");
  });

  it("keeps native minimum dimensions and scrollable high-zoom close decisions", () => {
    expect(mainGo).toMatch(/MinWidth:\s*800\b/);
    expect(mainGo).toMatch(/MinHeight:\s*600\b/);
    const closeDialog = themeBlock(".q-close-dialog");
    expect(closeDialog).toContain("max-height: calc(100vh - 48px)");
    expect(closeDialog).toContain("overflow-y: auto");
  });
});
