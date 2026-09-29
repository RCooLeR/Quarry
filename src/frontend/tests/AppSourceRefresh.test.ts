import React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

const runtime = vi.hoisted(() => ({ on: vi.fn(() => () => {}) }));
const backend = vi.hoisted(() => ({
  BeginSearchRequest: vi.fn(),
  CancelSearch: vi.fn(),
  FileState: vi.fn(),
  GetStagingState: vi.fn(),
  OpenViaDialog: vi.fn(),
  RefreshFile: vi.fn(),
  SqlAnalyze: vi.fn(),
}));
const editorHarness = vi.hoisted(() => ({ gotoEnd: vi.fn() }));
const panels = vi.hoisted(() => ({
  nextMount: 0,
  gridResolvers: [] as Array<(value: string) => void>,
  hexResolvers: [] as Array<(value: string) => void>,
  toolsResolvers: [] as Array<(value: string) => void>,
}));

vi.mock("@wailsio/runtime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wailsio/runtime")>();
  return { ...actual, Events: { ...actual.Events, On: runtime.on } };
});
vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({
  AppLifecycle: { ApproveClose: vi.fn(), CancelClose: vi.fn() },
  FileService: backend,
}));

vi.mock("../src/editor/QuarryEditor", () => ({
  QuarryEditor: class {
    private callbacks: { onStatus: (status: unknown) => void };

    constructor(_host: HTMLElement, callbacks: { onStatus: (status: unknown) => void }) {
      this.callbacks = callbacks;
    }

    async attach(fileId: string, _detected: string, _path: string, startByte: number): Promise<void> {
      this.callbacks.onStatus({
        fileId,
        startByte,
        positionByte: startByte,
        endByte: startByte + 256,
        firstLine: 1,
        lastLine: 8,
        atBof: startByte === 0,
        atEof: false,
        approx: false,
      });
    }

    async gotoEnd(): Promise<void> { await editorHarness.gotoEnd(); }
    async refreshSource(meta: { fileId: string; detected: string; path: string }, csvDelimiter: string): Promise<void> {
      await editorHarness.gotoEnd(meta, csvDelimiter);
    }
    async flush(): Promise<void> {}
    clear(): void {}
    destroy(): void {}
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

vi.mock("../src/Tools", async () => {
  const { createElement, useEffect, useState } = await import("react");
  return {
    default: (props: {
      fileId: string;
      detected: string;
      analysis: { tables: Array<{ name: string }> } | null;
      onAnalyze: (fileId: string) => Promise<unknown>;
    }) => {
      const [mount] = useState(() => ++panels.nextMount);
      const [lateValue, setLateValue] = useState("");
      useEffect(() => {
        let resolve!: (value: string) => void;
        const pending = new Promise<string>((done) => { resolve = done; });
        panels.toolsResolvers.push(resolve);
        // Deliberately leave the promise alive on unmount. The App-level key
        // must make a late completion incapable of replacing the new panel.
        void pending.then(setLateValue);
      }, []);
      return createElement("section", { "data-testid": "tools", "data-mount": mount },
        createElement("span", { "data-testid": "tools-local" }, `tools-state-${mount}:${lateValue}`),
        createElement("span", { "data-testid": "tools-analysis" }, props.analysis?.tables?.[0]?.name ?? "no-analysis"),
        createElement("button", {
          type: "button",
          onClick: () => { void props.onAnalyze(props.fileId).catch(() => {}); },
        }, "Run mock analysis"),
      );
    },
  };
});

vi.mock("../src/CsvGrid", async () => {
  const { createElement, useEffect, useState } = await import("react");
  return {
    default: () => {
      const [mount] = useState(() => ++panels.nextMount);
      const [rows, setRows] = useState(`cached-grid-${mount}`);
      useEffect(() => {
        let resolve!: (value: string) => void;
        const pending = new Promise<string>((done) => { resolve = done; });
        panels.gridResolvers.push(resolve);
        void pending.then(setRows);
      }, []);
      return createElement("div", { "data-testid": "csv-grid", "data-mount": mount }, rows);
    },
  };
});

vi.mock("../src/HexView", async () => {
  const { createElement, useEffect, useState } = await import("react");
  return {
    default: () => {
      const [mount] = useState(() => ++panels.nextMount);
      const [bytes, setBytes] = useState(`cached-hex-${mount}`);
      useEffect(() => {
        let resolve!: (value: string) => void;
        const pending = new Promise<string>((done) => { resolve = done; });
        panels.hexResolvers.push(resolve);
        void pending.then(setBytes);
      }, []);
      return createElement("div", { "data-testid": "hex-view", "data-mount": mount }, bytes);
    },
  };
});

vi.mock("../src/XRay", () => ({
  default: (props: { regions: Array<{ name: string }> }) => React.createElement(
    "div",
    { "data-testid": "xray-regions" },
    props.regions.map((region) => region.name).join(",") || "no-regions",
  ),
}));

import App from "../src/App";

const baseMeta = {
  fileId: "same-file",
  path: "C:\\data\\same-size.sql",
  size: 4096,
  encoding: "UTF-8",
  detected: "sql",
  binary: false,
  editable: true,
};

const summary = (name: string) => ({
  tables: [{ name, createOffset: 128, insertOffset: -1, bytes: 256 }],
  createTables: 1,
  insertTables: 0,
  definerCount: 0,
  header: false,
});

const changedState = (modTimeNanos: number) => ({
  size: baseMeta.size,
  modTimeNanos,
  sameOpenedFile: true,
  changedFromOpen: true,
});
const stableState = (modTimeNanos: number) => ({
  size: baseMeta.size,
  modTimeNanos,
  sameOpenedFile: true,
  changedFromOpen: false,
});

async function settle(): Promise<void> {
  for (let i = 0; i < 16; i++) await Promise.resolve();
}

describe("same-file source refresh invalidation", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    vi.useFakeTimers();
    vi.clearAllMocks();
    localStorage.clear();
    panels.nextMount = 0;
    panels.gridResolvers = [];
    panels.hexResolvers = [];
    panels.toolsResolvers = [];
    editorHarness.gotoEnd.mockResolvedValue(undefined);
    backend.CancelSearch.mockResolvedValue(true);
    backend.GetStagingState.mockResolvedValue({
      editCount: 0,
      originalSize: baseMeta.size,
      editedSize: baseMeta.size,
      netDelta: 0,
      lengthPreserving: true,
      inPlaceEligible: false,
    });
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    localStorage.clear();
    vi.useRealTimers();
  });

  async function renderAndOpen(meta = baseMeta): Promise<void> {
    backend.OpenViaDialog.mockResolvedValueOnce(meta);
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });
    const open = host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary");
    await act(async () => {
      open?.click();
      await settle();
    });
  }

  async function clickMenuItem(menu: string, item: string): Promise<void> {
    const trigger = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-label"))
      .find((button) => button.textContent?.trim() === menu);
    expect(trigger, `missing ${menu} menu`).toBeDefined();
    await act(async () => {
      trigger?.click();
      await settle();
    });
    const action = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
      .find((button) => button.textContent?.includes(item));
    expect(action, `missing ${item} action`).toBeDefined();
    await act(async () => {
      action?.click();
      await settle();
    });
  }

  async function runImmediateFollowTick(): Promise<void> {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
      await settle();
    });
  }

  it("clears cached and in-flight SQL analysis/X-ray data while preserving follow-to-EOF", async () => {
    const lateAnalysis = deferred<ReturnType<typeof summary>>();
    backend.SqlAnalyze
      .mockResolvedValueOnce(summary("old_table"))
      .mockImplementationOnce(() => lateAnalysis.promise);
    backend.FileState
      .mockResolvedValueOnce(changedState(2))
      .mockResolvedValueOnce(stableState(2))
      .mockResolvedValue(stableState(2));
    backend.RefreshFile.mockResolvedValue({ ...baseMeta });

    await renderAndOpen();
    await clickMenuItem("Tools", "Analyze & extract tables");
    const analyze = host.querySelector<HTMLButtonElement>("[data-testid='tools'] button");
    await act(async () => {
      analyze?.click();
      await settle();
    });
    expect(host.querySelector("[data-testid='tools-analysis']")?.textContent).toBe("old_table");
    expect(host.querySelector("[data-testid='xray-regions']")?.textContent).toContain("old_table");

    const oldToolsMount = host.querySelector("[data-testid='tools']")?.getAttribute("data-mount");
    await act(async () => {
      host.querySelector<HTMLButtonElement>("[data-testid='tools'] button")?.click();
      await settle();
    });
    expect(backend.SqlAnalyze).toHaveBeenCalledTimes(2);

    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();

    expect(backend.RefreshFile).toHaveBeenCalledWith(baseMeta.fileId);
    expect(editorHarness.gotoEnd).toHaveBeenCalledTimes(1);
    expect(host.querySelector("[data-testid='tools-analysis']")?.textContent).toBe("no-analysis");
    expect(host.querySelector("[data-testid='xray-regions']")?.textContent).toBe("no-regions");
    expect(host.querySelector("[data-testid='tools']")?.getAttribute("data-mount")).not.toBe(oldToolsMount);

    await act(async () => {
      lateAnalysis.resolve(summary("late_stale_table"));
      await settle();
    });
    expect(host.textContent).not.toContain("late_stale_table");
    expect(host.querySelector("[data-testid='xray-regions']")?.textContent).toBe("no-regions");
  });

  it("remounts CSV tools/grid and hex bytes for repeated same-size rewrites and rejects old panel completions", async () => {
    const csvMeta = { ...baseMeta, path: "C:\\data\\same-size.csv", detected: "csv" };
    backend.FileState
      .mockResolvedValueOnce(changedState(10))
      .mockResolvedValueOnce(stableState(10))
      .mockResolvedValueOnce(changedState(11))
      .mockResolvedValueOnce(stableState(11))
      .mockResolvedValue(stableState(11));
    backend.RefreshFile.mockResolvedValue({ ...csvMeta });

    await renderAndOpen(csvMeta);
    await clickMenuItem("Tools", "CSV column tools");
    await clickMenuItem("View", "Grid view");

    const oldToolsMount = host.querySelector("[data-testid='tools']")?.getAttribute("data-mount");
    const oldGridMount = host.querySelector("[data-testid='csv-grid']")?.getAttribute("data-mount");
    expect(oldToolsMount).toBeTruthy();
    expect(oldGridMount).toBeTruthy();
    expect(panels.toolsResolvers).toHaveLength(1);
    expect(panels.gridResolvers).toHaveLength(1);

    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();

    expect(host.querySelector("[data-testid='tools']")?.getAttribute("data-mount")).not.toBe(oldToolsMount);
    expect(host.querySelector("[data-testid='csv-grid']")?.getAttribute("data-mount")).not.toBe(oldGridMount);
    expect(host.textContent).not.toContain(`tools-state-${oldToolsMount}`);
    expect(host.textContent).not.toContain(`cached-grid-${oldGridMount}`);
    expect(panels.toolsResolvers).toHaveLength(2);
    expect(panels.gridResolvers).toHaveLength(2);

    await act(async () => {
      panels.toolsResolvers[0]("late-stale-tools");
      panels.gridResolvers[0]("late-stale-grid");
      panels.toolsResolvers[1]("fresh-tools");
      panels.gridResolvers[1]("fresh-grid");
      await settle();
    });
    expect(host.textContent).not.toContain("late-stale-tools");
    expect(host.textContent).not.toContain("late-stale-grid");
    expect(host.textContent).toContain("fresh-tools");
    expect(host.textContent).toContain("fresh-grid");

    await clickMenuItem("View", "Hex view");
    const oldHexMount = host.querySelector("[data-testid='hex-view']")?.getAttribute("data-mount");
    expect(oldHexMount).toBeTruthy();
    expect(panels.hexResolvers).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
      await settle();
    });

    expect(backend.RefreshFile).toHaveBeenCalledTimes(2);
    expect(editorHarness.gotoEnd).toHaveBeenCalledTimes(2);
    expect(host.querySelector("[data-testid='hex-view']")?.getAttribute("data-mount")).not.toBe(oldHexMount);
    expect(host.textContent).not.toContain(`cached-hex-${oldHexMount}`);
    expect(panels.hexResolvers).toHaveLength(2);

    await act(async () => {
      panels.hexResolvers[0]("late-stale-hex");
      panels.hexResolvers[1]("fresh-hex");
      await settle();
    });
    expect(host.textContent).not.toContain("late-stale-hex");
    expect(host.textContent).toContain("fresh-hex");
  });

  it("treats refresh metadata with a cleanup warning as committed and authoritative", async () => {
    backend.FileState
      .mockResolvedValueOnce(changedState(20))
      .mockResolvedValueOnce(stableState(20))
      .mockResolvedValue(stableState(20));
    backend.RefreshFile.mockResolvedValue({
      ...baseMeta,
      refreshWarning: "replacement installed; old document cleanup was incomplete",
    });

    await renderAndOpen();
    await clickMenuItem("Tools", "Analyze & extract tables");
    const oldToolsMount = host.querySelector("[data-testid='tools']")?.getAttribute("data-mount");
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();

    expect(backend.RefreshFile).toHaveBeenCalledWith(baseMeta.fileId);
    expect(editorHarness.gotoEnd).toHaveBeenCalledOnce();
    expect(host.querySelector("[data-testid='tools']")?.getAttribute("data-mount")).not.toBe(oldToolsMount);
    expect(host.querySelector(".q-notification")?.textContent).toContain("File refreshed with a session-cleanup warning");
    expect(host.querySelector(".q-notification")?.textContent).toContain("old document cleanup was incomplete");
  });

  it("rebinds the attached editor and workbench to changed refresh metadata", async () => {
    const refreshedMeta = {
      ...baseMeta,
      path: "C:\\data\\rotated.bin",
      encoding: "binary",
      detected: "binary",
      binary: true,
      editable: false,
    };
    backend.FileState
      .mockResolvedValueOnce(changedState(25))
      .mockResolvedValueOnce(stableState(25))
      .mockResolvedValue(stableState(25));
    backend.RefreshFile.mockResolvedValue(refreshedMeta);

    await renderAndOpen();
    await clickMenuItem("Tools", "Analyze & extract tables");
    expect(host.querySelector("[data-testid='tools']")).not.toBeNull();
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();

    expect(editorHarness.gotoEnd).toHaveBeenCalledWith(refreshedMeta, "");
    expect(host.querySelector("[data-testid='tools']")).toBeNull();
    expect(host.querySelector(".q-status")?.textContent).toContain("rotated.bin");
    expect(host.querySelector(".q-status")?.textContent).toContain("binary");
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')).toBeNull();
  });

  it("retries renderer synchronization without committing RefreshFile twice", async () => {
    backend.FileState
      .mockResolvedValueOnce(changedState(27))
      .mockResolvedValueOnce(stableState(27))
      .mockResolvedValue(stableState(27));
    backend.RefreshFile.mockResolvedValue({ ...baseMeta });
    editorHarness.gotoEnd
      .mockRejectedValueOnce(new Error("temporary tail reload failure"))
      .mockResolvedValue(undefined);

    await renderAndOpen();
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();
    expect(backend.RefreshFile).toHaveBeenCalledOnce();
    expect(editorHarness.gotoEnd).toHaveBeenCalledOnce();
    expect(host.querySelector(".q-notification")?.textContent).toContain("Live follow could not refresh");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
      await settle();
    });
    expect(backend.RefreshFile).toHaveBeenCalledOnce();
    expect(editorHarness.gotoEnd).toHaveBeenCalledTimes(2);
  });

  it("finishes a committed renderer synchronization after follow is turned off", async () => {
    backend.FileState
      .mockResolvedValueOnce(changedState(28))
      .mockResolvedValue(stableState(28));
    backend.RefreshFile.mockResolvedValue({ ...baseMeta });
    editorHarness.gotoEnd
      .mockRejectedValueOnce(new Error("temporary tail reload failure"))
      .mockResolvedValue(undefined);

    await renderAndOpen();
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();
    expect(backend.RefreshFile).toHaveBeenCalledOnce();
    expect(editorHarness.gotoEnd).toHaveBeenCalledOnce();

    // The failed editor reload no longer lives in closure-local follow state.
    // Stopping follow starts the independent renderer-only recovery worker.
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();

    expect(backend.RefreshFile).toHaveBeenCalledOnce();
    expect(editorHarness.gotoEnd).toHaveBeenCalledTimes(2);
    expect(host.querySelector(".q-status")?.textContent).not.toContain("● live");
    expect(host.querySelector(".q-notification")?.textContent).toContain("Refreshed file view synchronized");
  });

  it("rechecks workbench ownership after FileState resolves before committing a refresh", async () => {
    const pendingState = deferred<ReturnType<typeof changedState>>();
    const pendingOpen = deferred<typeof baseMeta>();
    backend.FileState.mockImplementationOnce(() => pendingState.promise);
    backend.RefreshFile.mockResolvedValue({ ...baseMeta });

    await renderAndOpen();
    backend.OpenViaDialog.mockImplementationOnce(() => pendingOpen.promise);
    await clickMenuItem("View", "Follow tail (live)");
    await runImmediateFollowTick();
    expect(backend.FileState).toHaveBeenCalledOnce();

    // Opening a native dialog reserves the workbench while the follow poll is
    // between awaits. The poll must observe that new owner and skip RefreshFile.
    await clickMenuItem("File", "Open file");
    await act(async () => {
      pendingState.resolve(changedState(30));
      await settle();
    });
    expect(backend.RefreshFile).not.toHaveBeenCalled();

    await act(async () => {
      pendingOpen.resolve({ ...baseMeta, fileId: "" });
      await settle();
    });
  });
});
