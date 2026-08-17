import { Compartment, EditorState } from "@codemirror/state";
import { DecorationSet, EditorView, ViewPlugin } from "@codemirror/view";
import { afterEach, describe, expect, it } from "vitest";
import {
  EDITOR_SYNTAX_BACKGROUNDS,
  EDITOR_SYNTAX_PALETTES,
  highlightFor,
  syntaxThemeFor,
} from "../src/editor/highlight";
import { MAX_EDITOR_HIGHLIGHT_RANGES } from "../src/editor/highlightLimits";

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

describe("editor syntax themes", () => {
  let view: EditorView | null = null;

  afterEach(() => {
    view?.destroy();
    view = null;
    document.body.replaceChildren();
  });

  it.each(["dark", "light"] as const)("keeps every %s token color at WCAG AA contrast", (theme) => {
    const palette = EDITOR_SYNTAX_PALETTES[theme];
    const foregrounds = [
      ...Object.values(palette.generic),
      ...Object.values(palette.sql),
      ...palette.csv,
    ];

    for (const color of foregrounds) {
      expect(
        contrast(color, EDITOR_SYNTAX_BACKGROUNDS[theme]),
        `${theme} syntax color ${color}`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("reconfigures existing SQL token colors without rebuilding the language plugin", () => {
    const host = document.createElement("div");
    document.body.append(host);
    const theme = new Compartment();
    const language = highlightFor("sql", "dump.sql");
    const state = EditorState.create({
      doc: "SELECT `orders`, 42 FROM `archive`",
      extensions: [language, theme.of(syntaxThemeFor("dark"))],
    });
    view = new EditorView({ state, parent: host });

    const keyword = host.querySelector<HTMLElement>(".q-sql-keyword");
    expect(keyword).not.toBeNull();
    expect(getComputedStyle(keyword!).color).toBe("rgb(86, 156, 214)");

    view.dispatch({ effects: theme.reconfigure(syntaxThemeFor("light")) });

    expect(host.querySelector(".q-sql-keyword")).toBe(keyword);
    expect(getComputedStyle(keyword!).color).toBe("rgb(23, 78, 166)");
  });

  it.each([
    { detected: "javascript", path: "dense.js", token: "const ", selector: ".q-hl-keyword" },
    { detected: "sql", path: "dense.sql", token: "SELECT ", selector: ".q-sql-keyword" },
  ])("caps $detected decoration materialization across one visible render", ({ detected, path, token, selector }) => {
    const host = document.createElement("div");
    document.body.append(host);
    const language = highlightFor(detected, path);
    const state = EditorState.create({
      doc: token.repeat(MAX_EDITOR_HIGHLIGHT_RANGES + 128),
      extensions: [language],
    });
    view = new EditorView({ state, parent: host });

    const plugin = language[0] as ViewPlugin<{ decorations: DecorationSet }>;
    expect(view.plugin(plugin)?.decorations.size).toBe(MAX_EDITOR_HIGHLIGHT_RANGES);
    // CodeMirror horizontally virtualizes ultra-wide lines, but the materialized
    // DOM must still retain at least one normally highlighted token.
    expect(host.querySelector(selector)).not.toBeNull();
  });

  it("preserves normal generic token highlighting below the shared budget", () => {
    const host = document.createElement("div");
    document.body.append(host);
    const state = EditorState.create({
      doc: "const answer = 42; // retained comment",
      extensions: [highlightFor("javascript", "normal.js")],
    });
    view = new EditorView({ state, parent: host });

    expect(host.querySelectorAll(".q-hl-keyword")).toHaveLength(1);
    expect(host.querySelectorAll(".q-hl-number")).toHaveLength(1);
    expect(host.querySelectorAll(".q-hl-comment")).toHaveLength(1);
  });
});
