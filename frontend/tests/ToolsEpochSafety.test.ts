import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  CsvPreview: vi.fn(),
  CsvProfile: vi.fn(),
  CsvSchema: vi.fn(),
  CsvToSQLConfigPreview: vi.fn(),
  CsvMarkdownPreview: vi.fn(),
  SqlLint: vi.fn(),
  SqlSchemaDiff: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import Tools, { TOOLS_CSV_COLUMN_DOM_LIMIT, TOOLS_CSV_WARNING_DOM_LIMIT } from "../src/Tools";
import type { NotificationOwner } from "../src/notificationState";

const CSV_GENERATION = 7;

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function settle(): Promise<void> {
  for (let index = 0; index < 12; index++) await Promise.resolve();
}

function button(host: HTMLElement, label: string): HTMLButtonElement {
  const match = Array.from(host.querySelectorAll("button")).find((item) => item.textContent === label);
  if (!match) throw new Error(`missing button ${label}`);
  return match;
}

function changeSelect(control: HTMLSelectElement, value: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value")?.set;
  setter?.call(control, value);
  control.dispatchEvent(new Event("change", { bubbles: true }));
}

describe("Tools request epochs", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let sequence: number;
  let onError: ReturnType<typeof vi.fn>;
  let onNotice: ReturnType<typeof vi.fn>;
  let writeText: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    sequence = 0;
    onError = vi.fn();
    onNotice = vi.fn();
    writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    fileService.CsvInspect.mockResolvedValue({
      generation: CSV_GENERATION,
      delimiter: ",",
      delimiterName: "comma",
      hasHeader: true,
      columns: 2,
    });
    fileService.CsvSchema.mockResolvedValue({
      generation: CSV_GENERATION,
      columns: [{ name: "first", sqlType: "TEXT" }, { name: "second", sqlType: "TEXT" }],
    });
    fileService.CsvPreview.mockResolvedValue({
      generation: CSV_GENERATION,
      header: ["first", "second"],
      rows: [["one", "two"]],
    });
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  const commonProps = (fileId: string, detected: string) => ({
    fileId,
    detected,
    analysis: null,
    otherFiles: [],
    onAnalyze: vi.fn(),
    activeJob: null,
    onCancelJob: vi.fn(),
    onOperationStart: (operation: string): NotificationOwner => ({
      operationId: `tools-${++sequence}`,
      sequence,
      operation,
      fileId,
    }),
    onOperationComplete: vi.fn(),
    onNotice,
    onError,
    onClose: vi.fn(),
  });

  async function renderCsv(): Promise<void> {
    await act(async () => {
      root.render(React.createElement(Tools, commonProps("csv-a", "csv")));
      await settle();
    });
  }

  const sqlAnalysis = (createTables: number) => ({
    tables: [],
    createTables,
    insertTables: 0,
    definerCount: 0,
    header: false,
  });

  it("does not let a late detector response overwrite an explicit dialect refresh", async () => {
    const inspection = deferred<any>();
    fileService.CsvInspect.mockReturnValue(inspection.promise);
    fileService.CsvSchema.mockImplementation((_id: string, delimiter: string) => Promise.resolve({
      generation: CSV_GENERATION,
      columns: [{ name: delimiter === ";" ? "explicit-column" : "detected-column", sqlType: "TEXT" }],
    }));
    fileService.CsvPreview.mockImplementation((_id: string, delimiter: string) => Promise.resolve({
      generation: CSV_GENERATION,
      header: [delimiter === ";" ? "explicit-column" : "detected-column"],
      rows: [],
    }));
    await renderCsv();
    const delimiter = host.querySelector<HTMLSelectElement>("#q-csv-separator");
    if (!delimiter) throw new Error("missing CSV separator");

    await act(async () => {
      changeSelect(delimiter, ";");
      await settle();
    });
    expect(host.textContent).toContain("explicit-column");
    await act(async () => {
      inspection.resolve({ generation: CSV_GENERATION, delimiter: ",", delimiterName: "comma", hasHeader: true });
      await settle();
    });

    expect(delimiter.value).toBe(";");
    expect(host.textContent).toContain("explicit-column");
    expect(host.textContent).not.toContain("detected-column");
    expect(fileService.CsvSchema).not.toHaveBeenCalledWith("csv-a", ",", true);
  });

  it("clears schema-dependent selections and rejects an older profile after a dialect refresh", async () => {
    const staleProfile = deferred<any>();
    fileService.CsvProfile.mockReturnValue(staleProfile.promise);
    fileService.CsvSchema.mockImplementation((_id: string, delimiter: string) => Promise.resolve({
      generation: CSV_GENERATION,
      columns: delimiter === ";"
        ? [{ name: "new-first", sqlType: "TEXT" }, { name: "new-second", sqlType: "TEXT" }]
        : [{ name: "first", sqlType: "TEXT" }, { name: "second", sqlType: "TEXT" }],
    }));
    fileService.CsvPreview.mockImplementation((_id: string, delimiter: string) => Promise.resolve({
      generation: CSV_GENERATION,
      header: delimiter === ";" ? ["new-first", "new-second"] : ["first", "second"],
      rows: [],
    }));
    await renderCsv();

    const redaction = host.querySelector<HTMLSelectElement>('select[aria-label^="Redaction mode for column 1"]');
    const filter = host.querySelector<HTMLSelectElement>('select[aria-label="Filter column"]');
    const dedupe = host.querySelector<HTMLSelectElement>("#q-csv-dedupe-key");
    const delimiter = host.querySelector<HTMLSelectElement>("#q-csv-separator");
    if (!redaction || !filter || !dedupe || !delimiter) throw new Error("missing CSV controls");
    await act(async () => {
      changeSelect(redaction, "hash");
      changeSelect(filter, "1");
      changeSelect(dedupe, "1");
      button(host, "Profile columns").click();
      await settle();
    });
    await act(async () => {
      changeSelect(delimiter, ";");
      await settle();
    });

    expect(host.querySelector<HTMLSelectElement>('select[aria-label^="Redaction mode for column 1"]')?.value).toBe("off");
    expect(host.querySelector<HTMLSelectElement>('select[aria-label="Filter column"]')?.value).toBe("0");
    expect(host.querySelector<HTMLSelectElement>("#q-csv-dedupe-key")?.value).toBe("-1");
    expect(host.textContent).toContain("new-first");

    await act(async () => {
      staleProfile.resolve({
        generation: CSV_GENERATION,
        recordsScanned: 1,
        columns: [{ name: "stale-profile-column", sqlType: "TEXT", nonNull: 1, null: 0, top: [] }],
      });
      await settle();
    });
    expect(host.textContent).not.toContain("stale-profile-column");
    expect(onError).not.toHaveBeenCalled();
  });

  it("does not commit an older SQL preview after its conversion configuration changes", async () => {
    const stalePreview = deferred<any>();
    fileService.CsvToSQLConfigPreview.mockReturnValue(stalePreview.promise);
    await renderCsv();

    await act(async () => {
      button(host, "Preview SQL").click();
      await settle();
    });
    const tableName = host.querySelector<HTMLInputElement>("#q-csv-table-name");
    if (!tableName) throw new Error("missing table name");
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
      setter?.call(tableName, "changed_table");
      tableName.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    await act(async () => {
      stalePreview.resolve({ generation: CSV_GENERATION, text: "STALE SQL PREVIEW" });
      await settle();
    });

    expect(host.textContent).not.toContain("STALE SQL PREVIEW");
    expect(onError).not.toHaveBeenCalled();
  });

  it("does not copy an older Markdown response after the CSV configuration changes", async () => {
    const staleMarkdown = deferred<any>();
    fileService.CsvMarkdownPreview.mockReturnValue(staleMarkdown.promise);
    await renderCsv();

    await act(async () => {
      button(host, "Copy preview as Markdown").click();
      await settle();
    });
    const header = Array.from(host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')).find((input) =>
      input.closest("label")?.textContent?.includes("First row is a header"),
    );
    if (!header) throw new Error("missing header toggle");
    await act(async () => {
      header.click();
      await settle();
    });
    await act(async () => {
      staleMarkdown.resolve({ generation: CSV_GENERATION, text: "stale markdown" });
      await settle();
    });

    expect(writeText).not.toHaveBeenCalled();
    expect(onNotice).not.toHaveBeenCalledWith(expect.anything(), "Copied preview as Markdown");
    expect(onError).not.toHaveBeenCalled();
  });

  it("clears a schema diff on target change and rejects the older target response", async () => {
    const oldDiff = deferred<any>();
    fileService.SqlSchemaDiff.mockImplementation((_source: string, target: string) => {
      if (target === "sql-b") return oldDiff.promise;
      return Promise.resolve({ fileA: "a.sql", fileB: "c.sql", addedTables: ["current_table"] });
    });
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-a", "sql"),
        otherFiles: [
          { id: "sql-b", name: "b.sql", detected: "sql" },
          { id: "sql-c", name: "c.sql", detected: "sql" },
        ],
      }));
      await settle();
    });
    const target = host.querySelector<HTMLSelectElement>('select[aria-label="Dump to compare"]');
    if (!target) throw new Error("missing diff target");
    await act(async () => {
      changeSelect(target, "sql-b");
      button(host, "Diff schemas").click();
      await settle();
    });
    await act(async () => {
      changeSelect(target, "sql-c");
      await settle();
    });
    await act(async () => {
      oldDiff.resolve({ fileA: "a.sql", fileB: "b.sql", addedTables: ["stale_table"] });
      await settle();
    });
    expect(host.textContent).not.toContain("stale_table");
    expect(onNotice).not.toHaveBeenCalled();

    await act(async () => {
      button(host, "Diff schemas").click();
      await settle();
    });
    expect(host.textContent).toContain("current_table");
  });

  it("rejects a late SQL lint result after the analysis context changes", async () => {
    const staleLint = deferred<any>();
    fileService.SqlLint.mockReturnValue(staleLint.promise);
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      button(host, "Lint dump").click();
      await settle();
    });

    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(2),
      }));
      await settle();
    });
    await act(async () => {
      staleLint.resolve({
        findings: [{ severity: "warn", title: "stale lint finding", detail: "old analysis" }],
      });
      await settle();
    });

    expect(host.textContent).not.toContain("stale lint finding");
    expect(onError).not.toHaveBeenCalled();
  });

  it("does not report a late SQL lint failure after the file changes", async () => {
    const staleLint = deferred<any>();
    fileService.SqlLint.mockReturnValue(staleLint.promise);
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      button(host, "Lint dump").click();
      await settle();
    });
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-b", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      staleLint.reject(new Error("stale lint failure"));
      await settle();
    });

    expect(onError).not.toHaveBeenCalled();
  });

  it("does not report a late SQL lint failure from an unmounted Tools instance", async () => {
    const staleLint = deferred<any>();
    fileService.SqlLint.mockReturnValueOnce(staleLint.promise);
    await act(async () => {
      root.render(React.createElement(Tools, {
        key: "old-tools",
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      button(host, "Lint dump").click();
      await settle();
    });
    await act(async () => {
      root.render(React.createElement(Tools, {
        key: "new-tools",
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      staleLint.reject(new Error("unmounted lint failure"));
      await settle();
    });

    expect(onError).not.toHaveBeenCalled();
  });

  it("does not publish a completed SQL analysis after the file changes", async () => {
    const staleAnalysis = deferred<any>();
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-a", "sql"),
        analysis: sqlAnalysis(1),
        onAnalyze: vi.fn(() => staleAnalysis.promise),
      }));
      await settle();
    });
    await act(async () => {
      button(host, "Re-analyze dump").click();
      await settle();
    });
    await act(async () => {
      root.render(React.createElement(Tools, {
        ...commonProps("sql-b", "sql"),
        analysis: sqlAnalysis(1),
      }));
      await settle();
    });
    await act(async () => {
      staleAnalysis.resolve(sqlAnalysis(9));
      await settle();
    });

    expect(onNotice).not.toHaveBeenCalledWith(expect.anything(), "Found 0 tables");
    expect(onError).not.toHaveBeenCalled();
    expect(button(host, "Re-analyze dump").disabled).toBe(false);
  });

  it("materializes a bounded number of distinct CSV warnings and reports omissions", async () => {
    const warningCount = TOOLS_CSV_WARNING_DOM_LIMIT + 17;
    fileService.CsvInspect.mockResolvedValue({
      generation: CSV_GENERATION,
      delimiter: ",",
      delimiterName: "comma",
      hasHeader: true,
      columns: 2,
      warnings: Array.from({ length: warningCount }, (_, index) => `warning-${index}`),
    });
    await renderCsv();

    const warningPanel = host.querySelector<HTMLElement>(".q-csv-warnings");
    expect(warningPanel?.querySelectorAll("li")).toHaveLength(TOOLS_CSV_WARNING_DOM_LIMIT);
    expect(warningPanel?.textContent).toContain(`Showing the first ${TOOLS_CSV_WARNING_DOM_LIMIT} warnings`);
    expect(warningPanel?.textContent).toContain("17 additional warnings are omitted");
    expect(warningPanel?.textContent).not.toContain(`warning-${TOOLS_CSV_WARNING_DOM_LIMIT}`);
  });

  it("materializes at most 256 CSV columns in each Tools view and reports omissions", async () => {
    const count = TOOLS_CSV_COLUMN_DOM_LIMIT + 44;
    const columns = Array.from({ length: count }, (_, index) => ({ name: `column-${index}`, sqlType: "TEXT" }));
    const cells = Array.from({ length: count }, (_, index) => `value-${index}`);
    fileService.CsvSchema.mockResolvedValue({ generation: CSV_GENERATION, columns });
    fileService.CsvPreview.mockResolvedValue({ generation: CSV_GENERATION, header: cells, rows: [cells] });
    fileService.CsvProfile.mockResolvedValue({
      generation: CSV_GENERATION,
      recordsScanned: 1,
      columns: columns.map((column) => ({ ...column, nonNull: 1, null: 0, top: [] })),
    });
    await renderCsv();

    const columnConfig = host.querySelector<HTMLElement>('[aria-labelledby="q-csv-columns-label"]');
    const redactionConfig = host.querySelector<HTMLElement>('[aria-label="Redaction mode by column"]');
    expect(columnConfig?.querySelectorAll(".q-colrow")).toHaveLength(TOOLS_CSV_COLUMN_DOM_LIMIT);
    expect(redactionConfig?.querySelectorAll(".q-colrow")).toHaveLength(TOOLS_CSV_COLUMN_DOM_LIMIT);
    expect(host.querySelectorAll(".q-tprev thead th")).toHaveLength(TOOLS_CSV_COLUMN_DOM_LIMIT);
    expect(host.querySelectorAll(".q-tprev tbody tr:first-child td")).toHaveLength(TOOLS_CSV_COLUMN_DOM_LIMIT);
    expect(host.textContent).toContain("44 additional columns are omitted");

    await act(async () => {
      button(host, "Profile columns").click();
      await settle();
    });
    expect(host.querySelectorAll(".q-profile .q-prof")).toHaveLength(TOOLS_CSV_COLUMN_DOM_LIMIT);
    expect(host.textContent).toContain("Profile materializes the first 256 columns; 44 additional columns are omitted.");
  });
});
