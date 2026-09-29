import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  GetCsvGrid: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import CsvGrid, { csvGridFocusDestination } from "../src/CsvGrid";

async function settle(): Promise<void> {
  for (let index = 0; index < 5; index++) await Promise.resolve();
}

async function press(target: Element, key: string, options: KeyboardEventInit = {}): Promise<void> {
  await act(async () => {
    target.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, ...options }));
    await settle();
  });
}

describe("CSV grid keyboard accessibility", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    vi.clearAllMocks();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    fileService.CsvInspect.mockResolvedValue({ generation: 1, delimiter: ",", hasHeader: false });
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("keeps one cell in the tab order and follows the ARIA grid arrow/Home/End model", async () => {
    fileService.GetCsvGrid.mockResolvedValue({
      fileId: "keyboard",
      generation: 1,
      startByte: 0,
      nextByte: 100,
      startRow: 1,
      rows: Array.from({ length: 4 }, (_, row) => (
        Array.from({ length: 4 }, (_, column) => `r${row + 1}c${column + 1}`)
      )),
      columns: 4,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "keyboard", onError: vi.fn() }));
      await settle();
    });

    const grid = host.querySelector<HTMLTableElement>('table[role="grid"]')!;
    const onlyTabStop = () => Array.from(grid.querySelectorAll<HTMLElement>('[tabindex="0"]'));
    const activeLabel = () => document.activeElement?.getAttribute("aria-label");
    const first = grid.querySelector<HTMLElement>('[aria-label="Inspect row 1, column 1"]')!;

    expect(onlyTabStop()).toEqual([first]);
    expect(grid.querySelector<HTMLElement>('[aria-label="Inspect header, column 1"]')?.tabIndex).toBe(-1);

    act(() => first.focus());
    await press(first, "ArrowRight");
    expect(activeLabel()).toBe("Inspect row 1, column 2");
    expect(onlyTabStop()).toEqual([document.activeElement]);

    await press(document.activeElement!, "ArrowDown");
    expect(activeLabel()).toBe("Inspect row 2, column 2");
    await press(document.activeElement!, "ArrowUp");
    await press(document.activeElement!, "ArrowUp");
    expect(activeLabel()).toBe("Inspect header, column 2");

    await press(document.activeElement!, "End");
    expect(activeLabel()).toBe("Inspect header, column 4");
    await press(document.activeElement!, "Home");
    expect(activeLabel()).toBe("Inspect header, column 1");
    await press(document.activeElement!, "End", { ctrlKey: true });
    expect(activeLabel()).toBe("Inspect row 4, column 4");
    await press(document.activeElement!, "Home", { ctrlKey: true });
    expect(activeLabel()).toBe("Inspect header, column 1");
    expect(onlyTabStop()).toEqual([document.activeElement]);

    await press(document.activeElement!, "Enter");
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Full bounded cell value");
    const close = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent === "Close inspector")!;
    await act(async () => {
      close.click();
      await settle();
    });
    expect(activeLabel()).toBe("Inspect header, column 1");
    expect(onlyTabStop()).toEqual([document.activeElement]);
  });

  it("pages by visible rows, virtualizes to a distant cell, and does not fetch another sample", async () => {
    fileService.GetCsvGrid.mockResolvedValue({
      fileId: "virtual-keyboard",
      generation: 1,
      startByte: 0,
      nextByte: 4096,
      startRow: 1,
      rows: Array.from({ length: 100 }, (_, row) => (
        Array.from({ length: 20 }, (_, column) => `r${row + 1}c${column + 1}`)
      )),
      columns: 20,
      atBof: true,
      atEof: false,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "virtual-keyboard", onError: vi.fn() }));
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 20));
    });

    const scroller = host.querySelector<HTMLDivElement>(".q-grid")!;
    Object.defineProperties(scroller, {
      clientWidth: { configurable: true, value: 360 },
      clientHeight: { configurable: true, value: 58 },
    });
    await act(async () => {
      window.dispatchEvent(new Event("resize"));
      await settle();
    });

    const first = host.querySelector<HTMLElement>('[aria-label="Inspect row 1, column 1"]')!;
    act(() => first.focus());
    await press(first, "PageDown");
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Inspect row 3, column 1");
    await press(document.activeElement!, "PageUp");
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Inspect row 1, column 1");

    await press(document.activeElement!, "End", { ctrlKey: true });
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Inspect row 100, column 20");
    expect(scroller.scrollTop).toBeGreaterThan(0);
    expect(scroller.scrollLeft).toBeGreaterThan(0);
    expect(host.querySelectorAll('table[role="grid"] [tabindex="0"]')).toHaveLength(1);
    expect(fileService.GetCsvGrid).toHaveBeenCalledTimes(1);

    await press(document.activeElement!, "PageDown");
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Inspect row 100, column 20");
    expect(fileService.GetCsvGrid).toHaveBeenCalledTimes(1);

    await press(document.activeElement!, "Home", { ctrlKey: true });
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Inspect header, column 1");
    expect(scroller.scrollTop).toBe(0);
    expect(scroller.scrollLeft).toBe(0);
  });

  it("bounds pure navigation for empty and malformed dimensions", () => {
    expect(csvGridFocusDestination({ row: 20, column: 20 }, "PageDown", 0, 0, Infinity, false))
      .toEqual({ row: 0, column: 0 });
    expect(csvGridFocusDestination({ row: 2, column: 1 }, "PageUp", 3, 2, 50, false))
      .toEqual({ row: 0, column: 1 });
    expect(csvGridFocusDestination({ row: 2, column: 1 }, "Escape", 3, 2, 1, false)).toBeNull();
  });
});
