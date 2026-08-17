import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  GetCsvGrid: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import CsvGrid, {
  MAX_RENDERED_GRID_COLUMNS,
  MAX_RENDERED_GRID_ROWS,
  csvGridRenderWindow,
} from "../src/CsvGrid";

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

describe("CSV grid two-axis DOM bounds", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    vi.clearAllMocks();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("never exceeds the hard render caps, including pathological viewport values", () => {
    const first = csvGridRenderWindow(500, 256, {
      scrollTop: 0,
      scrollLeft: 0,
      width: Number.POSITIVE_INFINITY,
      height: Number.POSITIVE_INFINITY,
    });
    expect(first.rowEnd - first.rowStart).toBeLessThanOrEqual(MAX_RENDERED_GRID_ROWS);
    expect(first.columnEnd - first.columnStart).toBeLessThanOrEqual(MAX_RENDERED_GRID_COLUMNS);

    const last = csvGridRenderWindow(500, 256, {
      scrollTop: Number.MAX_SAFE_INTEGER,
      scrollLeft: Number.MAX_SAFE_INTEGER,
      width: 8_000,
      height: 8_000,
    });
    expect(last.rowEnd).toBe(500);
    expect(last.columnEnd).toBe(256);
    expect(last.rowEnd - last.rowStart).toBeLessThanOrEqual(MAX_RENDERED_GRID_ROWS);
    expect(last.columnEnd - last.columnStart).toBeLessThanOrEqual(MAX_RENDERED_GRID_COLUMNS);
  });

  it("renders only a viewport-sized rectangle while exposing full sample dimensions", async () => {
    const rows = Array.from({ length: 500 }, (_, row) => (
      Array.from({ length: 20 }, (_, column) => `r${row + 1}c${column + 1}`)
    ));
    fileService.CsvInspect.mockResolvedValue({ generation: 7, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid.mockResolvedValue({
      fileId: "wide",
      generation: 7,
      startByte: 0,
      nextByte: 128 * 1024,
      startRow: 1,
      rows,
      columns: 20,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "wide", onError: vi.fn() }));
      await settle();
    });

    const table = host.querySelector<HTMLTableElement>("table");
    expect(table?.getAttribute("aria-rowcount")).toBe("501");
    expect(table?.getAttribute("aria-colcount")).toBe("21");
    expect(host.textContent).toContain("Bounded sample");

    const renderedHeaders = host.querySelectorAll("thead th[aria-colindex]").length - 1;
    const renderedCells = host.querySelectorAll("tbody td[aria-label^='Inspect row']").length;
    expect(renderedHeaders).toBeGreaterThan(0);
    expect(renderedHeaders).toBeLessThanOrEqual(MAX_RENDERED_GRID_COLUMNS);
    expect(renderedCells).toBeGreaterThan(0);
    expect(renderedCells).toBeLessThanOrEqual(MAX_RENDERED_GRID_ROWS * MAX_RENDERED_GRID_COLUMNS);
    expect(host.querySelector('[aria-label="Inspect row 1, column 1"]')).not.toBeNull();
    expect(host.querySelector('[aria-label="Inspect row 500, column 20"]')).toBeNull();

    const grid = host.querySelector<HTMLDivElement>(".q-grid")!;
    Object.defineProperties(grid, {
      clientWidth: { configurable: true, value: 360 },
      clientHeight: { configurable: true, value: 290 },
    });
    grid.scrollLeft = 20 * 180;
    grid.scrollTop = 499 * 29;
    await act(async () => {
      grid.dispatchEvent(new Event("scroll", { bubbles: true }));
      await settle();
    });

    expect(host.querySelector('[aria-label="Inspect row 1, column 1"]')).toBeNull();
    expect(host.querySelector('[aria-label="Inspect row 500, column 20"]')).not.toBeNull();
    expect(host.textContent).toContain("Rows 483–500 of 500");
    expect(host.textContent).toContain("columns 15–20 of 20");
  });

  it("keeps sparse record windows reachable without relying on a scrollbar", async () => {
    fileService.CsvInspect.mockResolvedValue({ generation: 3, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid
      .mockResolvedValueOnce({
        fileId: "sparse",
        generation: 3,
        startByte: 0,
        nextByte: 4096,
        startRow: 1,
        rows: [["first large record"]],
        columns: 1,
        atBof: true,
        atEof: false,
      })
      .mockResolvedValueOnce({
        fileId: "sparse",
        generation: 3,
        startByte: 4096,
        nextByte: 8192,
        startRow: 2,
        rows: [["second large record"]],
        columns: 1,
        atBof: false,
        atEof: true,
      })
      .mockResolvedValueOnce({
        fileId: "sparse",
        generation: 3,
        startByte: 0,
        nextByte: 4096,
        startRow: 1,
        rows: [["first large record"]],
        columns: 1,
        atBof: true,
        atEof: false,
      })
      .mockResolvedValueOnce({
        fileId: "sparse",
        generation: 3,
        startByte: 4096,
        nextByte: 8192,
        startRow: 2,
        rows: [["second large record"]],
        columns: 1,
        atBof: false,
        atEof: true,
      })
      .mockResolvedValueOnce({
        fileId: "sparse",
        generation: 3,
        startByte: 0,
        nextByte: 4096,
        startRow: 1,
        rows: [["first large record"]],
        columns: 1,
        atBof: true,
        atEof: false,
      });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "sparse", onError: vi.fn() }));
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 100));
    });

    const button = (label: string) => Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((candidate) => candidate.textContent?.trim() === label)!;
    expect(button("First sample").disabled).toBe(true);
    expect(button("Previous sample").disabled).toBe(true);
    expect(button("Next sample").disabled).toBe(false);

    await act(async () => {
      button("Next sample").click();
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("sparse", ",", 4096, 128 * 1024);
    expect(host.textContent).toContain("second large record");
    expect(button("First sample").disabled).toBe(false);
    expect(button("Previous sample").disabled).toBe(false);
    expect(button("Next sample").disabled).toBe(true);

    await act(async () => {
      button("First sample").click();
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("sparse", ",", 0, 128 * 1024);
    expect(host.textContent).toContain("first large record");
    expect(button("First sample").disabled).toBe(true);
    expect(button("Previous sample").disabled).toBe(true);

    await act(async () => {
      button("Next sample").click();
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("sparse", ",", 4096, 128 * 1024);

    await act(async () => {
      button("Previous sample").click();
      await settle();
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("sparse", ",", 0, 128 * 1024);
    expect(host.textContent).toContain("first large record");
  });

  it("rejects a next-page response from a different source generation", async () => {
    const onError = vi.fn();
    fileService.CsvInspect.mockResolvedValue({ generation: 11, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid
      .mockResolvedValueOnce({
        fileId: "changing",
        generation: 11,
        startByte: 0,
        nextByte: 4096,
        startRow: 1,
        rows: [["owned generation"]],
        columns: 1,
        atBof: true,
        atEof: false,
      })
      .mockResolvedValueOnce({
        fileId: "changing",
        generation: 12,
        startByte: 4096,
        nextByte: 8192,
        startRow: 2,
        rows: [["stale replacement"]],
        columns: 1,
        atBof: false,
        atEof: true,
      });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "changing", onError }));
      await settle();
    });
    const next = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((candidate) => candidate.textContent?.trim() === "Next sample")!;
    await act(async () => {
      next.click();
      await settle();
    });

    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("changing", ",", 4096, 128 * 1024);
    expect(onError).toHaveBeenCalledWith(expect.stringContaining("stale source generation"));
    expect(host.textContent).toContain("owned generation");
    expect(host.textContent).not.toContain("stale replacement");
    expect(next.disabled).toBe(false);
  });

  it("rejects an initial grid window from a different generation than inspection", async () => {
    const onError = vi.fn();
    fileService.CsvInspect.mockResolvedValue({ generation: 30, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid.mockResolvedValue({
      fileId: "changed-before-grid",
      generation: 31,
      startByte: 0,
      nextByte: 128,
      startRow: 1,
      rows: [["new generation"]],
      columns: 1,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "changed-before-grid", onError }));
      await settle();
    });

    expect(onError).toHaveBeenCalledWith(expect.stringContaining("stale source generation"));
    expect(host.textContent).not.toContain("new generation");
  });

  it("keeps the current page after a load error and permits an explicit retry", async () => {
    const onError = vi.fn();
    fileService.CsvInspect.mockResolvedValue({ generation: 21, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid
      .mockResolvedValueOnce({
        fileId: "retry",
        generation: 21,
        startByte: 0,
        nextByte: 2048,
        startRow: 1,
        rows: [["safe current page"]],
        columns: 1,
        atBof: true,
        atEof: false,
      })
      .mockRejectedValueOnce(new Error("temporary read failure"))
      .mockResolvedValueOnce({
        fileId: "retry",
        generation: 21,
        startByte: 2048,
        nextByte: 4096,
        startRow: 2,
        rows: [["retried page"]],
        columns: 1,
        atBof: false,
        atEof: true,
      });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "retry", onError }));
      await settle();
    });
    const next = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((candidate) => candidate.textContent?.trim() === "Next sample")!;
    await act(async () => {
      next.click();
      await settle();
    });
    expect(onError).toHaveBeenCalledWith("temporary read failure");
    expect(host.textContent).toContain("safe current page");
    expect(next.disabled).toBe(false);

    await act(async () => {
      next.click();
      await settle();
    });
    expect(fileService.GetCsvGrid).toHaveBeenLastCalledWith("retry", ",", 2048, 128 * 1024);
    expect(host.textContent).toContain("retried page");
  });
});
