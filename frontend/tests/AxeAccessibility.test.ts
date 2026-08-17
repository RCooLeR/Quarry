import React, { act } from "react";
import { createRoot } from "react-dom/client";
import axe, { type Result, type RunOptions } from "axe-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runtime = vi.hoisted(() => ({
  on: vi.fn(() => () => {}),
}));

const fileService = vi.hoisted(() => ({
  BeginSearchRequest: vi.fn(),
  CancelSearch: vi.fn(),
  CsvInspect: vi.fn(),
  GetCsvGrid: vi.fn(),
  GetStagingState: vi.fn(),
  OpenViaDialog: vi.fn(),
  SearchAllRequest: vi.fn(),
}));

vi.mock("@wailsio/runtime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wailsio/runtime")>();
  return { ...actual, Events: { ...actual.Events, On: runtime.on } };
});

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({
  AppLifecycle: {
    ApproveClose: vi.fn(),
    CancelClose: vi.fn(),
  },
  FileService: fileService,
}));

vi.mock("../src/editor/QuarryEditor", () => ({
  QuarryEditor: class {
    destroy(): void {}
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

import App from "../src/App";
import CommandPalette, { type Command } from "../src/CommandPalette";
import CsvGrid from "../src/CsvGrid";
import Help from "../src/Help";
import UnsavedChangesDialog from "../src/UnsavedChangesDialog";
import type { CloseDecisionContext } from "../src/closeSafety";

// axe-core documents color-contrast as unsupported in jsdom. Keep every other
// WCAG A/AA and best-practice rule enabled so this remains a broad structural
// gate instead of a hand-picked assertion list.
const AXE_OPTIONS: RunOptions = {
  runOnly: {
    type: "tag",
    values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22a", "wcag22aa", "best-practice"],
  },
  rules: {
    "color-contrast": { enabled: false },
  },
};

function violationDetails(violations: Result[]): string {
  return violations.map((violation) => {
    const nodes = violation.nodes.map((node) => (
      `    ${node.target.join(" ")}\n      ${node.failureSummary ?? node.html}`
    )).join("\n");
    return `${violation.id}: ${violation.help}\n${nodes}`;
  }).join("\n\n");
}

async function expectNoAxeViolations(container: HTMLElement, surface: string): Promise<void> {
  const results = await axe.run(container, AXE_OPTIONS);
  expect(
    results.violations,
    `${surface} has automated accessibility violations:\n${violationDetails(results.violations)}`,
  ).toEqual([]);
}

async function settle(): Promise<void> {
  for (let index = 0; index < 8; index++) await Promise.resolve();
}

async function nextAnimationFrame(): Promise<void> {
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

describe("automated axe accessibility gate", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    vi.clearAllMocks();
    fileService.BeginSearchRequest.mockResolvedValue("search1");
    fileService.CancelSearch.mockResolvedValue(true);
    fileService.SearchAllRequest.mockResolvedValue({ hits: [] });
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("proves the gate detects a real accessible-name violation", async () => {
    act(() => {
      root.render(React.createElement("button", { type: "button" }));
    });

    const results = await axe.run(host, AXE_OPTIONS);
    expect(results.violations.map((violation) => violation.id)).toContain("button-name");
  });

  it("keeps the empty workbench shell free of detectable WCAG A/AA violations", async () => {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });

    await expectNoAxeViolations(host, "Empty workbench");
  });

  it("checks the command, Help, and unsaved-changes modal surfaces", async () => {
    const commands: Command[] = [
      { id: "open", label: "Open file", group: "File", run: vi.fn() },
      {
        id: "edit",
        label: "Turn editing on",
        group: "Edit",
        enabled: false,
        disabledReason: "This file cannot be edited safely",
        run: vi.fn(),
      },
    ];

    await act(async () => {
      root.render(React.createElement(CommandPalette, { commands, onClose: vi.fn() }));
      await settle();
    });
    await expectNoAxeViolations(host, "Command palette");

    await act(async () => {
      root.render(React.createElement(Help, { onClose: vi.fn() }));
      await settle();
    });
    await expectNoAxeViolations(host, "Help dialog");

    const context: CloseDecisionContext = {
      target: {
        fileId: "important",
        path: "C:\\data\\important.sql",
        dirty: true,
        editorAttached: true,
        scope: "tab",
      },
      staging: {
        editCount: 2,
        originalSize: 1024,
        editedSize: 1032,
        netDelta: 8,
        lengthPreserving: false,
        inPlaceEligible: false,
      },
    };
    await act(async () => {
      root.render(React.createElement(UnsavedChangesDialog, { context, onChoose: vi.fn() }));
      await settle();
    });
    await expectNoAxeViolations(host, "Unsaved-changes dialog");
  });

  it("checks a populated CSV grid and its bounded cell inspector", async ({ onTestFinished }) => {
    const consoleErrors: string[] = [];
    const consoleError = vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      consoleErrors.push(args.map(String).join(" "));
    });
    onTestFinished(() => consoleError.mockRestore());

    const inspectResult = {
      generation: 1,
      delimiter: ",",
      hasHeader: true,
    };
    const gridResult = {
      fileId: "orders",
      generation: 1,
      startByte: 0,
      nextByte: 128,
      startRow: 1,
      rows: [
        ["order_id", "customer"],
        ["1001", "Ada"],
        ["1002", "Grace"],
      ],
      columns: 2,
      atBof: true,
      atEof: true,
    };
    let resolveInspect!: (value: typeof inspectResult) => void;
    let resolveGrid!: (value: typeof gridResult) => void;
    const inspectPromise = new Promise<typeof inspectResult>((resolve) => { resolveInspect = resolve; });
    const gridPromise = new Promise<typeof gridResult>((resolve) => { resolveGrid = resolve; });
    fileService.CsvInspect.mockReturnValue(inspectPromise);
    fileService.GetCsvGrid.mockReturnValue(gridPromise);

    await act(async () => {
      root.render(React.createElement(CsvGrid, { fileId: "orders", onError: vi.fn() }));
      await settle();
    });
    await act(async () => {
      // Resolve the component's effect-owned RPCs only after the initial render
      // act has flushed. This keeps the async apply and its viewport RAF inside
      // a second deterministic act boundary.
      resolveInspect(inspectResult);
      resolveGrid(gridResult);
      await settle();
      // CsvGrid applies its loaded window, then synchronizes the virtualized
      // viewport in requestAnimationFrame. Keep both state transitions inside
      // act so axe never scans a moving/incompletely committed tree.
      await nextAnimationFrame();
      await nextAnimationFrame();
      await settle();
    });
    await expectNoAxeViolations(host, "Populated CSV grid");

    const firstCell = host.querySelector<HTMLElement>('[aria-label="Inspect row 2, column 1"]');
    expect(firstCell).not.toBeNull();
    await act(async () => {
      firstCell?.click();
      await settle();
    });
    await expectNoAxeViolations(host, "CSV cell inspector");
    expect(consoleErrors, "CSV axe surface emitted unexpected React/runtime errors").toEqual([]);
  });
});
