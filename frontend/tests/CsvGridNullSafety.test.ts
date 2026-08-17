import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  GetCsvGrid: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import CsvGrid from "../src/CsvGrid";

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

describe("CsvGrid nullable responses", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let onError: ReturnType<typeof vi.fn>;
  let writeClipboard: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    onError = vi.fn();
    writeClipboard = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: writeClipboard },
    });
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("renders an explicit empty state for an empty or whitespace-only CSV window", async () => {
    fileService.CsvInspect.mockResolvedValue({
      generation: 1,
      delimiter: ",",
      hasHeader: false,
      candidates: null,
      warnings: null,
    });
    fileService.GetCsvGrid.mockResolvedValue({
      generation: 1,
      startByte: 0,
      nextByte: 0,
      startRow: 0,
      rows: null,
      columns: 0,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "empty", onError }));
      await settle();
    });

    expect(host.textContent).toContain("No data rows in this window.");
    expect(host.querySelector("table")).not.toBeNull();
    expect(onError).not.toHaveBeenCalled();
  });

  it("keeps a header-only CSV renderable with no data rows", async () => {
    fileService.CsvInspect.mockResolvedValue({ generation: 1, delimiter: ",", hasHeader: true });
    fileService.GetCsvGrid.mockResolvedValue({
      generation: 1,
      startByte: 0,
      nextByte: 8,
      startRow: 1,
      rows: [["id", "name"]],
      columns: 2,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "header-only", onError }));
      await settle();
    });

    const headings = Array.from(host.querySelectorAll("thead th")).map((cell) => cell.textContent);
    expect(headings).toEqual(["#", "id", "name"]);
    expect(host.textContent).toContain("No data rows in this window.");
    expect(onError).not.toHaveBeenCalled();
  });

  it("ignores an out-of-order grid response from the previously active file", async () => {
    const first = deferred<any>();
    fileService.CsvInspect.mockImplementation((fileId: string) => Promise.resolve({
      generation: fileId === "file-a" ? 1 : 2,
      delimiter: ",",
      hasHeader: false,
    }));
    fileService.GetCsvGrid.mockImplementation((fileId: string) => fileId === "file-a"
      ? first.promise
      : Promise.resolve({
        fileId: "file-b",
        generation: 2,
        startByte: 0,
        nextByte: 4,
        startRow: 1,
        rows: [["new-file"]],
        columns: 1,
        atBof: true,
        atEof: true,
      }));

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "file-a", onError }));
      await settle();
    });
    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "file-b", onError }));
      await settle();
    });
    expect(host.textContent).toContain("new-file");

    await act(async () => {
      first.resolve({
        fileId: "file-a",
        generation: 1,
        startByte: 0,
        nextByte: 4,
        startRow: 1,
        rows: [["stale-file"]],
        columns: 1,
        atBof: true,
        atEof: true,
      });
      await settle();
    });
    expect(host.textContent).toContain("new-file");
    expect(host.textContent).not.toContain("stale-file");
    expect(onError).not.toHaveBeenCalled();
  });

  it("ignores a rejected grid request after a newer file owns the component", async () => {
    const first = deferred<any>();
    fileService.CsvInspect.mockImplementation((fileId: string) => Promise.resolve({
      generation: fileId === "file-a" ? 1 : 2,
      delimiter: ",",
      hasHeader: false,
    }));
    fileService.GetCsvGrid.mockImplementation((fileId: string) => fileId === "file-a"
      ? first.promise
      : Promise.resolve({
        fileId: "file-b",
        generation: 2,
        startByte: 0,
        nextByte: 4,
        startRow: 1,
        rows: [["new-file"]],
        columns: 1,
        atBof: true,
        atEof: true,
      }));

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "file-a", onError }));
      await settle();
    });
    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "file-b", onError }));
      await settle();
    });
    expect(host.textContent).toContain("new-file");

    await act(async () => {
      first.reject(new Error("stale file-a failure"));
      await settle();
    });
    expect(host.textContent).toContain("new-file");
    expect(onError).not.toHaveBeenCalled();
  });

  it("reports a malformed row collection without crashing the grid", async () => {
    fileService.CsvInspect.mockResolvedValue({ generation: 1, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid.mockResolvedValue({ rows: "bad rows" });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "malformed", onError }));
      await settle();
    });

    expect(onError).toHaveBeenCalledWith(expect.stringContaining("CsvGridResult.rows"));
    expect(host.querySelector("table")).not.toBeNull();
    expect(host.textContent).toContain("No data rows in this window.");
  });

  it("opens and copies a full bounded Unicode cell from the keyboard", async () => {
    const value = `first line\n${"💾α".repeat(300)}`;
    fileService.CsvInspect.mockResolvedValue({ generation: 1, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid.mockResolvedValue({
      generation: 1,
      startByte: 0,
      nextByte: 2048,
      startRow: 1,
      rows: [[value]],
      columns: 1,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "unicode", onError }));
      await settle();
    });

    const cell = host.querySelector<HTMLTableCellElement>('td[aria-label="Inspect row 1, column 1"]');
    expect(cell).not.toBeNull();
    expect(cell?.tabIndex).toBe(0);
    expect(cell?.title).toContain("Cell preview is truncated");
    expect(cell?.textContent).not.toBe(value);

    await act(async () => {
      cell?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
      await settle();
    });

    const inspector = host.querySelector<HTMLTextAreaElement>('textarea[aria-label="Full bounded cell value"]');
    expect(inspector?.value).toBe(value);
    expect(document.activeElement).toBe(inspector);

    const copy = Array.from(host.querySelectorAll("button")).find((item) => item.textContent === "Copy cell");
    await act(async () => {
      copy?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });
    expect(writeClipboard).toHaveBeenCalledWith(value);
    expect(onError).not.toHaveBeenCalled();
  });

  it("caps malformed oversized cell inspection and reports the safety limit", async () => {
    const value = "x".repeat(128 * 1024 + 20);
    fileService.CsvInspect.mockResolvedValue({ generation: 1, delimiter: ",", hasHeader: false });
    fileService.GetCsvGrid.mockResolvedValue({
      generation: 1,
      startByte: 0,
      nextByte: value.length,
      startRow: 7,
      rows: [[value]],
      columns: 1,
      atBof: true,
      atEof: true,
    });

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "oversized", onError }));
      await settle();
    });

    const cell = host.querySelector<HTMLTableCellElement>('td[aria-label="Inspect row 7, column 1"]');
    expect(cell?.textContent?.length).toBeLessThan(value.length);
    await act(async () => {
      cell?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });

    const inspector = host.querySelector<HTMLTextAreaElement>('textarea[aria-label="Full bounded cell value"]');
    expect(inspector?.value).toHaveLength(128 * 1024);
    expect(host.textContent).toContain("Value exceeded the safety limit");

    const copy = Array.from(host.querySelectorAll("button")).find((item) => item.textContent === "Copy cell");
    await act(async () => {
      copy?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });
    expect(writeClipboard).toHaveBeenCalledWith("x".repeat(128 * 1024));
    expect(onError).not.toHaveBeenCalled();
  });
});
