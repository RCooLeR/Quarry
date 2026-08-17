import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({
  CsvInspect: vi.fn(),
  CsvPreview: vi.fn(),
  CsvSchema: vi.fn(),
  CsvExportJSONLViaDialog: vi.fn(),
  SqlReplaceViaDialog: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import Tools, {
  parseToolsCsvNullValues,
  TOOLS_CSV_IDENTIFIER_INPUT_LIMIT,
  TOOLS_CSV_NULL_TOKEN_LIMIT,
  TOOLS_CSV_VALUE_INPUT_LIMIT,
} from "../src/Tools";
import type { NotificationOwner } from "../src/notificationState";
import {
  SQL_FIND_INPUT_MAX_BYTES,
  SQL_REPLACEMENT_INPUT_MAX_BYTES,
} from "../src/textInputLimits";

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

describe("data-tools product contract", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let sequence: number;

  beforeEach(() => {
    vi.clearAllMocks();
    sequence = 0;
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    fileService.CsvInspect.mockResolvedValue({
      generation: 7,
      delimiter: ",",
      delimiterName: "comma",
      confidence: "medium",
      columns: 3,
      hasHeader: true,
      candidates: [
        { delimiter: ",", name: "comma", columns: 3, score: 0.9 },
        { delimiter: "\t", name: "tab", columns: 2, score: 0.4 },
      ],
      warnings: ["delimiter confidence is not high"],
    });
    fileService.CsvSchema.mockResolvedValue({
      generation: 7,
      columns: [],
      hasHeader: true,
      warnings: ["schema used a bounded sample"],
    });
    fileService.CsvPreview.mockResolvedValue({
      generation: 7,
      header: [],
      rows: [],
      warnings: ["preview omitted a partial record"],
    });
    fileService.CsvExportJSONLViaDialog.mockResolvedValue({ outputPath: "out.jsonl" });
    fileService.SqlReplaceViaDialog.mockResolvedValue({ outputPath: "out.sql" });
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  const props = (detected: string) => ({
    fileId: "file-a",
    detected,
    analysis: null,
    otherFiles: [],
    onAnalyze: vi.fn(),
    activeJob: null,
    onCancelJob: vi.fn(),
    onDialectChange: vi.fn(),
    onOperationStart: (operation: string): NotificationOwner => ({
      operationId: `test-${++sequence}`,
      sequence,
      operation,
      fileId: "file-a",
    }),
    onOperationComplete: vi.fn(),
    onWorkbenchBusyChange: vi.fn(),
    onNotice: vi.fn(),
    onError: vi.fn(),
    onClose: vi.fn(),
  });

  it("fails closed for an unsupported file type", async () => {
    await act(async () => {
      root.render(React.createElement(Tools, props("json")));
      await settle();
    });

    expect(host.textContent).toContain("Data tools unavailable");
    expect(host.textContent).toContain("No operation can run for this file type");
    expect(host.textContent).not.toContain("Analyze dump");
    expect(fileService.CsvInspect).not.toHaveBeenCalled();
  });

  it("shows ranked candidates and every bounded-sample warning", async () => {
    const input = props("csv");
    await act(async () => {
      root.render(React.createElement(Tools, input));
      await settle();
    });

    const separator = host.querySelector<HTMLSelectElement>("#q-csv-separator");
    expect(separator).not.toBeNull();
    expect(Array.from(separator?.options ?? []).map((option) => option.textContent)).toEqual(expect.arrayContaining([
      expect.stringContaining("comma - 3 cols"),
      expect.stringContaining("tab - 2 cols"),
    ]));
    expect(host.textContent).toContain("delimiter confidence is not high");
    expect(host.textContent).toContain("schema used a bounded sample");
    expect(host.textContent).toContain("preview omitted a partial record");
    expect(input.onDialectChange).toHaveBeenCalledWith({ delimiter: ",", hasHeader: true, origin: "detected" });

    await act(async () => {
      if (separator) {
        separator.value = ";";
        separator.dispatchEvent(new Event("change", { bubbles: true }));
      }
      await settle();
    });
    expect(input.onDialectChange).toHaveBeenLastCalledWith({ delimiter: ";", hasHeader: true, origin: "override" });
    expect(fileService.CsvSchema).toHaveBeenLastCalledWith("file-a", ";", true);
  });

  it("passes explicit JSONL typing and exposes serialization-aware SQL replacement controls", async () => {
    const csv = props("csv");
    await act(async () => {
      root.render(React.createElement(Tools, csv));
      await settle();
    });
    const numeric = Array.from(host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]'))
      .find((element) => element.parentElement?.textContent?.includes("Type unambiguous numeric values"));
    await act(async () => {
      numeric?.click();
      const jsonl = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
        .find((button) => button.textContent === "JSONL");
      jsonl?.click();
      await settle();
    });
    expect(fileService.CsvExportJSONLViaDialog).toHaveBeenCalledWith("file-a", 7, ",", true, false);

    await act(async () => {
      root.render(React.createElement(Tools, props("sql")));
      await settle();
    });
    const find = host.querySelector<HTMLInputElement>('input[aria-label="Find SQL text"]');
    const replacement = host.querySelector<HTMLInputElement>('input[aria-label="Replace SQL text with"]');
    expect(find).not.toBeNull();
    expect(replacement).not.toBeNull();
    expect(host.textContent).toContain("Find / replace");
    expect(host.textContent).toContain("serialized byte lengths");
    expect(host.textContent).toContain("regex replacement remains unavailable");
    const replaceButton = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent?.includes("Replace"));
    expect(replaceButton?.disabled).toBe(true);
    await act(async () => {
      const valueSetter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
      if (!find || !valueSetter) throw new Error("find input value setter is unavailable");
      valueSetter.call(find, "old.test");
      find.dispatchEvent(new Event("input", { bubbles: true }));
      replaceButton?.click();
      await settle();
    });
    expect(fileService.SqlReplaceViaDialog).toHaveBeenCalledWith("file-a", "old.test", "", false, false, false);
    expect(host.textContent).toContain("Changes statement grouping");
    expect(host.textContent).toContain("statement-level triggers, atomicity, rollback");
    expect(host.textContent).toContain("Session preamble, ALTER/DROP, triggers, and other DML are omitted");
  });

  it("reserves the workbench synchronously and rejects a same-tick transform double click", async () => {
    const input = props("csv");
    await act(async () => {
      root.render(React.createElement(Tools, input));
      await settle();
    });
    const jsonl = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent === "JSONL");
    expect(jsonl?.disabled).toBe(false);

    await act(async () => {
      jsonl?.click();
      jsonl?.click();
      await settle();
    });

    expect(fileService.CsvExportJSONLViaDialog).toHaveBeenCalledOnce();
    expect(input.onWorkbenchBusyChange.mock.calls).toEqual([[true], [false]]);
  });

  it("bounds serialized SQL find and replacement text by UTF-8 bytes", async () => {
    await act(async () => {
      root.render(React.createElement(Tools, props("sql")));
      await settle();
    });

    const find = host.querySelector<HTMLInputElement>('input[aria-label="Find SQL text"]');
    const replacement = host.querySelector<HTMLInputElement>('input[aria-label="Replace SQL text with"]');
    const valueSetter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    if (!find || !replacement || !valueSetter) throw new Error("SQL replacement inputs are unavailable");
    expect(find.maxLength).toBe(SQL_FIND_INPUT_MAX_BYTES);
    expect(replacement.maxLength).toBe(SQL_REPLACEMENT_INPUT_MAX_BYTES);

    await act(async () => {
      valueSetter.call(find, "🙂".repeat((SQL_FIND_INPUT_MAX_BYTES / 4) + 1));
      find.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    expect(new TextEncoder().encode(find.value)).toHaveLength(SQL_FIND_INPUT_MAX_BYTES);

    await act(async () => {
      valueSetter.call(replacement, "é".repeat((SQL_REPLACEMENT_INPUT_MAX_BYTES / 2) + 1));
      replacement.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    expect(new TextEncoder().encode(replacement.value)).toHaveLength(SQL_REPLACEMENT_INPUT_MAX_BYTES);
    expect(host.textContent).toContain("SQL find text is limited to 64 KiB of UTF-8");
    expect(host.textContent).toContain("SQL replacement text is limited to 256 KiB of UTF-8");
    expect(find.getAttribute("aria-describedby")).toBe("q-sql-replace-input-limit");
    expect(replacement.getAttribute("aria-describedby")).toBe("q-sql-replace-input-limit");

    const replaceButton = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent?.includes("Replace"));
    await act(async () => {
      replaceButton?.click();
      await settle();
    });
    expect(fileService.SqlReplaceViaDialog).toHaveBeenCalledWith(
      "file-a",
      find.value,
      replacement.value,
      false,
      false,
      false,
    );
  });

  it("does not enable CSV transforms for mismatched schema and preview generations", async () => {
    fileService.CsvPreview.mockResolvedValue({
      generation: 8,
      header: [],
      rows: [],
      warnings: [],
    });
    const input = props("csv");
    await act(async () => {
      root.render(React.createElement(Tools, input));
      await settle();
    });

    const jsonl = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent === "JSONL");
    expect(jsonl?.disabled).toBe(true);
    expect(input.onError).toHaveBeenCalledWith(
      expect.objectContaining({ operation: "Inspect CSV" }),
      "Unable to refresh CSV schema and preview",
      expect.stringContaining("does not match preview generation"),
    );
    expect(fileService.CsvExportJSONLViaDialog).not.toHaveBeenCalled();
  });

  it("bounds CSV transform text state and null-token fanout", async () => {
    fileService.CsvSchema.mockResolvedValue({
      generation: 7,
      columns: [{ name: "id", sqlType: "TEXT", nonNull: 1, null: 0, samples: ["1"] }],
      hasHeader: true,
      warnings: [],
    });
    fileService.CsvPreview.mockResolvedValue({
      generation: 7,
      header: ["id"],
      rows: [["1"]],
      warnings: [],
    });
    await act(async () => {
      root.render(React.createElement(Tools, props("csv")));
      await settle();
    });

    expect(host.querySelector<HTMLInputElement>("#q-csv-table-name")?.maxLength).toBe(TOOLS_CSV_IDENTIFIER_INPUT_LIMIT);
    expect(host.querySelector<HTMLInputElement>('input[aria-label^="Output name for column"]')?.maxLength).toBe(TOOLS_CSV_IDENTIFIER_INPUT_LIMIT);
    for (const selector of [
      "#q-csv-null-values",
      'input[aria-label="Constant column value"]',
      "#q-csv-redaction-fixed",
      'input[aria-label="Filter comparison value"]',
    ]) {
      expect(host.querySelector<HTMLInputElement>(selector)?.maxLength).toBe(TOOLS_CSV_VALUE_INPUT_LIMIT);
    }

    const excessive = Array.from({ length: TOOLS_CSV_NULL_TOKEN_LIMIT + 1 }, (_, index) => `v${index}`).join(",");
    expect(parseToolsCsvNullValues(excessive)).toMatchObject({ overflow: true });
    const input = host.querySelector<HTMLInputElement>("#q-csv-null-values");
    const valueSetter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    if (!input || !valueSetter) throw new Error("NULL token input value setter is unavailable");
    await act(async () => {
      valueSetter.call(input, excessive);
      input.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    expect(host.textContent).toContain(`NULL tokens exceed the ${TOOLS_CSV_NULL_TOKEN_LIMIT}-item configuration limit`);
    const preview = Array.from(host.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent === "Preview SQL");
    expect(preview?.disabled).toBe(true);
  });
});
