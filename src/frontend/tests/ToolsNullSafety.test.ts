import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  CsvPreview: vi.fn(),
  CsvProfile: vi.fn(),
  CsvSchema: vi.fn(),
  SqlLint: vi.fn(),
  SqlSchemaDiff: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import Tools from "../src/Tools";
import { normalizeSqlSummary } from "../src/bridgePayloads";

const CSV_GENERATION = 7;

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function button(host: HTMLElement, label: string): HTMLButtonElement {
  const match = Array.from(host.querySelectorAll("button")).find((item) => item.textContent === label);
  if (!match) throw new Error(`missing button ${label}`);
  return match;
}

function accessibleName(host: HTMLElement, control: HTMLInputElement | HTMLSelectElement): string {
  const direct = control.getAttribute("aria-label")?.trim();
  if (direct) return direct;
  const labelledBy = control.getAttribute("aria-labelledby")?.split(/\s+/).filter(Boolean) ?? [];
  const referenced = labelledBy.map((id) => host.querySelector<HTMLElement>(`#${id}`)?.textContent?.trim() ?? "").filter(Boolean).join(" ");
  if (referenced) return referenced;
  const wrapping = control.closest("label")?.textContent?.trim();
  if (wrapping) return wrapping;
  if (control.id) {
    const explicit = Array.from(host.querySelectorAll<HTMLLabelElement>("label")).find((label) => label.htmlFor === control.id);
    if (explicit?.textContent?.trim()) return explicit.textContent.trim();
  }
  return "";
}

function expectNamedFormControls(host: HTMLElement): void {
  const controls = Array.from(host.querySelectorAll<HTMLInputElement | HTMLSelectElement>("input, select"));
  const names = controls.map((control) => accessibleName(host, control));
  expect(controls.length).toBeGreaterThan(0);
  expect(names.every(Boolean)).toBe(true);
  expect(new Set(names).size).toBe(names.length);
}

describe("Tools nullable responses", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let onError: ReturnType<typeof vi.fn>;
  let onNotice: ReturnType<typeof vi.fn>;
  let onOperationStart: ReturnType<typeof vi.fn>;
  let onOperationComplete: ReturnType<typeof vi.fn>;
  let sequence: number;

  beforeEach(() => {
    vi.clearAllMocks();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    onError = vi.fn();
    onNotice = vi.fn();
    sequence = 0;
    onOperationStart = vi.fn((operation: string) => ({
      operationId: `test-${++sequence}`,
      sequence,
      operation,
      fileId: "csv-a",
    }));
    onOperationComplete = vi.fn();
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  function renderCsv(): void {
    root.render(React.createElement(Tools, {
      fileId: "csv-a",
      detected: "csv",
      analysis: null,
      otherFiles: [],
      onAnalyze: vi.fn(async () => normalizeSqlSummary(null)),
      activeJob: null,
      onCancelJob: vi.fn(),
      onOperationStart,
      onOperationComplete,
      onNotice,
      onError,
      onClose: vi.fn(),
    }));
  }

  function renderSql(): void {
    root.render(React.createElement(Tools, {
      fileId: "sql-a",
      detected: "sql",
      analysis: normalizeSqlSummary({ tables: null }),
      otherFiles: [{ id: "sql-b", name: "other.sql", detected: "sql" }],
      onAnalyze: vi.fn(async () => normalizeSqlSummary({ tables: null })),
      activeJob: null,
      onCancelJob: vi.fn(),
      onOperationStart,
      onOperationComplete,
      onNotice,
      onError,
      onClose: vi.fn(),
    }));
  }

  it("renders empty schema, header-only preview, and empty profile states", async () => {
    fileService.CsvInspect.mockResolvedValue({
      generation: CSV_GENERATION,
      delimiter: ",",
      delimiterName: "comma",
      candidates: null,
      warnings: null,
    });
    fileService.CsvSchema.mockResolvedValue({ generation: CSV_GENERATION, columns: null, warnings: null });
    fileService.CsvPreview.mockResolvedValue({ generation: CSV_GENERATION, header: ["id"], rows: null, warnings: null });
    fileService.CsvProfile.mockResolvedValue({
      generation: CSV_GENERATION,
      columns: null,
      recordsScanned: 0,
      raggedRows: 0,
      truncated: false,
    });

    await act(async () => {
      renderCsv();
      await settle();
    });

    expect(host.textContent).toContain("No columns detected.");
    expect(host.textContent).toContain("No preview data rows.");
    expect(onError).not.toHaveBeenCalled();

    await act(async () => {
      button(host, "Profile columns").dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });
    expect(host.textContent).toContain("0 rows scanned");
    expect(host.textContent).toContain("No profile columns returned.");
    expect(onError).not.toHaveBeenCalled();
  });

  it("keeps the tools panel mounted and reports malformed CSV slices", async () => {
    fileService.CsvInspect.mockResolvedValue({ generation: CSV_GENERATION, delimiter: "," });
    fileService.CsvSchema.mockResolvedValue({ generation: CSV_GENERATION, columns: { unexpected: true } });

    await act(async () => {
      renderCsv();
      await settle();
    });

    expect(onError).toHaveBeenCalledWith(
      expect.objectContaining({ operation: "Inspect CSV" }),
      "Unable to refresh CSV schema and preview",
      expect.stringContaining("CsvSchemaResult.columns"),
    );
    expect(host.querySelector(".q-tools")).not.toBeNull();
    expect(host.textContent).toContain("No columns detected.");
  });

  it("renders nullable SQL summary, lint, and an identical schema diff", async () => {
    fileService.SqlLint.mockResolvedValue({ findings: null });
    fileService.SqlSchemaDiff.mockResolvedValue({
      fileA: "current.sql",
      fileB: "other.sql",
      addedTables: null,
      removedTables: null,
      changedTables: null,
      unchangedCount: 3,
    });

    await act(async () => {
      renderSql();
      await settle();
    });
    expect(host.textContent).toContain("No tables found in the analyzed dump.");

    await act(async () => {
      button(host, "Lint dump").dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });
    expect(host.textContent).toContain("No lint findings.");

    const target = Array.from(host.querySelectorAll("select")).find((select) =>
      Array.from(select.options).some((option) => option.value === "sql-b"),
    );
    if (!target) throw new Error("missing schema diff target");
    await act(async () => {
      target.value = "sql-b";
      target.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await act(async () => {
      button(host, "Diff schemas").dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });

    expect(host.textContent).toContain("No structural differences.");
    expect(onNotice).toHaveBeenCalledWith(
      expect.objectContaining({ operation: "Compare SQL schemas" }),
      "Schemas are identical",
    );
    expect(onError).not.toHaveBeenCalled();
  });

  it("keeps large SQL table results in bounded pages and filters without rendering every table", async () => {
    const tables = Array.from({ length: 450 }, (_, index) => ({
      name: `table-${String(index).padStart(3, "0")}`,
      createOffset: index * 10,
      insertOffset: -1,
      bytes: 10,
    }));
    await act(async () => {
      root.render(React.createElement(Tools, {
        fileId: "sql-a",
        detected: "sql",
        analysis: normalizeSqlSummary({ tables }),
        otherFiles: [],
        onAnalyze: vi.fn(async () => normalizeSqlSummary({ tables })),
        activeJob: null,
        onCancelJob: vi.fn(),
        onOperationStart,
        onOperationComplete,
        onNotice,
        onError,
        onClose: vi.fn(),
      }));
      await settle();
    });

    expect(host.querySelectorAll(".q-ttables [role=listitem]")).toHaveLength(200);
    expect(host.textContent).toContain("Page 1 of 3");
    expect(host.textContent).toContain("table-000");
    expect(host.textContent).not.toContain("table-449");

    await act(async () => { button(host, "Next tables").click(); await settle(); });
    expect(host.querySelectorAll(".q-ttables [role=listitem]")).toHaveLength(200);
    expect(host.textContent).toContain("Page 2 of 3");
    expect(host.textContent).toContain("table-200");
    expect(host.textContent).not.toContain("table-000");

    const filter = host.querySelector<HTMLInputElement>('input[aria-label="Filter analyzed SQL tables"]');
    if (!filter) throw new Error("missing SQL table filter");
    await act(async () => {
      const setValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
      setValue?.call(filter, "table-449");
      filter.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    expect(host.querySelectorAll(".q-ttables [role=listitem]")).toHaveLength(1);
    expect(host.textContent).toContain("table-449");
  });

  it("renders a table diff when only one nested change category is present", async () => {
    fileService.SqlSchemaDiff.mockResolvedValue({
      changedTables: [{
        name: "users",
        status: "changed",
        addedColumns: null,
        removedColumns: null,
        changedColumns: null,
        oldColumnOrder: null,
        newColumnOrder: null,
        addedConstraints: null,
        removedConstraints: null,
        addedIndexes: ["idx_users_email"],
        removedIndexes: null,
        changedOptions: null,
      }],
    });

    await act(async () => {
      renderSql();
      await settle();
    });
    const target = Array.from(host.querySelectorAll("select")).find((select) =>
      Array.from(select.options).some((option) => option.value === "sql-b"),
    );
    if (!target) throw new Error("missing schema diff target");
    await act(async () => {
      target.value = "sql-b";
      target.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await act(async () => {
      button(host, "Diff schemas").dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await settle();
    });

    expect(host.textContent).toContain("users");
    expect(host.textContent).toContain("+ indexes: idx_users_email");
    expect(onError).not.toHaveBeenCalled();
  });

  it("provides a distinct programmatic name for every CSV form control", async () => {
    fileService.CsvInspect.mockResolvedValue({
      generation: CSV_GENERATION,
      delimiter: ",",
      delimiterName: "comma",
      hasHeader: true,
      columns: 1,
      confidence: "high",
    });
    fileService.CsvSchema.mockResolvedValue({
      generation: CSV_GENERATION,
      columns: [{ name: "email", sqlType: "TEXT" }],
    });
    fileService.CsvPreview.mockResolvedValue({ generation: CSV_GENERATION, header: ["email"], rows: [["a@example.test"]] });

    await act(async () => {
      renderCsv();
      await settle();
    });

    expectNamedFormControls(host);
    expect(host.querySelector('[aria-labelledby="q-tools-title"]')).not.toBeNull();
    expect(host.querySelector('button[aria-label="Close data tools"]')).not.toBeNull();
  });

  it("keeps only the newest out-of-order delimiter refresh and disables transforms until both slices settle", async () => {
    const oldSchema = deferred<any>();
    const oldPreview = deferred<any>();
    const newSchema = deferred<any>();
    const newPreview = deferred<any>();
    fileService.CsvInspect.mockResolvedValue({ generation: CSV_GENERATION, delimiter: ",", hasHeader: true });
    fileService.CsvSchema.mockImplementation((_id: string, delimiter: string) => {
      if (delimiter === ";") return oldSchema.promise;
      if (delimiter === "|") return newSchema.promise;
      return Promise.resolve({ generation: CSV_GENERATION, columns: [{ name: "initial", sqlType: "TEXT" }] });
    });
    fileService.CsvPreview.mockImplementation((_id: string, delimiter: string) => {
      if (delimiter === ";") return oldPreview.promise;
      if (delimiter === "|") return newPreview.promise;
      return Promise.resolve({ generation: CSV_GENERATION, header: ["initial"], rows: [["initial-value"]] });
    });

    await act(async () => { renderCsv(); await settle(); });
    expect(button(host, "Add column").disabled).toBe(false);
    const separator = host.querySelector<HTMLSelectElement>("#q-csv-separator");
    if (!separator) throw new Error("missing CSV separator selector");
    await act(async () => {
      separator.value = ";";
      separator.dispatchEvent(new Event("change", { bubbles: true }));
      await settle();
    });
    await act(async () => {
      separator.value = "|";
      separator.dispatchEvent(new Event("change", { bubbles: true }));
      await settle();
    });
    expect(button(host, "Add column").disabled).toBe(true);
    expect(host.querySelector('[role="status"]')?.textContent).toContain("Refreshing schema and preview");

    await act(async () => { newSchema.resolve({ generation: CSV_GENERATION, columns: [{ name: "newest-column", sqlType: "TEXT" }] }); await settle(); });
    expect(button(host, "Add column").disabled).toBe(true);
    await act(async () => { newPreview.resolve({ generation: CSV_GENERATION, header: ["newest-column"], rows: [["newest-value"]] }); await settle(); });
    expect(button(host, "Add column").disabled).toBe(false);
    expect(host.textContent).toContain("newest-column");
    expect(host.textContent).toContain("newest-value");

    await act(async () => {
      oldSchema.resolve({ generation: CSV_GENERATION, columns: [{ name: "stale-column", sqlType: "TEXT" }] });
      oldPreview.resolve({ generation: CSV_GENERATION, header: ["stale-column"], rows: [["stale-value"]] });
      await settle();
    });
    expect(host.textContent).toContain("newest-column");
    expect(host.textContent).not.toContain("stale-column");
    expect(host.textContent).not.toContain("stale-value");
  });

  it("keeps only the newest out-of-order header refresh", async () => {
    const noHeaderSchema = deferred<any>();
    const noHeaderPreview = deferred<any>();
    const headerSchema = deferred<any>();
    const headerPreview = deferred<any>();
    let schemaWithHeaderCalls = 0;
    let previewWithHeaderCalls = 0;
    fileService.CsvInspect.mockResolvedValue({ generation: CSV_GENERATION, delimiter: ",", hasHeader: true });
    fileService.CsvSchema.mockImplementation((_id: string, _delimiter: string, hasHeader: boolean) => {
      if (!hasHeader) return noHeaderSchema.promise;
      if (schemaWithHeaderCalls++ === 0) return Promise.resolve({ generation: CSV_GENERATION, columns: [{ name: "initial", sqlType: "TEXT" }] });
      return headerSchema.promise;
    });
    fileService.CsvPreview.mockImplementation((_id: string, _delimiter: string, hasHeader: boolean) => {
      if (!hasHeader) return noHeaderPreview.promise;
      if (previewWithHeaderCalls++ === 0) return Promise.resolve({ generation: CSV_GENERATION, header: ["initial"], rows: [["initial-value"]] });
      return headerPreview.promise;
    });

    await act(async () => { renderCsv(); await settle(); });
    const headerToggle = Array.from(host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')).find((input) =>
      input.closest("label")?.textContent?.includes("First row is a header"),
    );
    if (!headerToggle) throw new Error("missing header toggle");
    await act(async () => {
      headerToggle.click();
      await settle();
    });
    await act(async () => {
      headerToggle.click();
      await settle();
    });
    expect(button(host, "Convert → .sql").disabled).toBe(true);

    await act(async () => {
      headerSchema.resolve({ generation: CSV_GENERATION, columns: [{ name: "header-newest", sqlType: "TEXT" }] });
      headerPreview.resolve({ generation: CSV_GENERATION, header: ["header-newest"], rows: [["header-newest-value"]] });
      await settle();
    });
    expect(button(host, "Convert → .sql").disabled).toBe(false);
    await act(async () => {
      noHeaderSchema.resolve({ generation: CSV_GENERATION, columns: [{ name: "header-stale", sqlType: "TEXT" }] });
      noHeaderPreview.resolve({ generation: CSV_GENERATION, header: [], rows: [["header-stale-value"]] });
      await settle();
    });
    expect(host.textContent).toContain("header-newest");
    expect(host.textContent).not.toContain("header-stale");
  });

  it("provides a distinct programmatic name for every SQL form control", async () => {
    await act(async () => {
      renderSql();
      await settle();
    });

    expectNamedFormControls(host);
    expect(host.querySelector('select[aria-label="Dump to compare"]')).not.toBeNull();
    expect(host.querySelector('input[aria-label="Find SQL text"]')).not.toBeNull();
    expect(host.querySelector('input[aria-label="Replace SQL text with"]')).not.toBeNull();
    expect(host.textContent).toContain("Find / replace");
    expect(host.textContent).toContain("serialized byte lengths");
  });
});
