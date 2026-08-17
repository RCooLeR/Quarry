import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

type EventCallback = (event: { data: unknown }) => void;

const runtime = vi.hoisted(() => ({
  handlers: new Map<string, EventCallback>(),
  on: vi.fn((name: string, callback: EventCallback) => {
    runtime.handlers.set(name, callback);
    return () => runtime.handlers.delete(name);
  }),
}));

const backend = vi.hoisted(() => ({
  CancelJob: vi.fn(),
  GetStagingState: vi.fn(),
  OpenFile: vi.fn(),
  OpenViaDialog: vi.fn(),
}));

const editorHarness = vi.hoisted(() => ({
  callbacks: null as null | {
    onError: (error: unknown) => void;
    onStatus: (status: unknown) => void;
  },
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
    constructor(_host: HTMLElement, callbacks: typeof editorHarness.callbacks) {
      editorHarness.callbacks = callbacks;
    }

    async attach(fileId: string, _detected: string, _path: string, startByte: number): Promise<void> {
      editorHarness.callbacks?.onStatus({
        fileId,
        startByte,
        positionByte: startByte,
        endByte: startByte + 512,
        firstLine: 1,
        lastLine: 10,
        atBof: startByte === 0,
        atEof: false,
        approx: false,
      });
    }

    clear(): void {}
    destroy(): void {}
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

import App from "../src/App";

const fileA = {
  fileId: "file-a",
  path: "/data/alpha.sql",
  size: 10_000,
  encoding: "UTF-8",
  detected: "sql",
  binary: false,
  editable: true,
};
const fileB = { ...fileA, fileId: "file-b", path: "/data/beta.sql" };

async function settle(): Promise<void> {
  for (let i = 0; i < 12; i++) await Promise.resolve();
}

describe("App operation notifications", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let consoleError: ReturnType<typeof vi.spyOn>;
  const clipboardWrite = vi.fn();

  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    runtime.handlers.clear();
    editorHarness.callbacks = null;
    backend.CancelJob.mockResolvedValue(undefined);
    backend.GetStagingState.mockResolvedValue({
      editCount: 0,
      originalSize: 10_000,
      editedSize: 10_000,
      netDelta: 0,
      lengthPreserving: true,
      inPlaceEligible: false,
    });
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: clipboardWrite },
    });
    clipboardWrite.mockResolvedValue(undefined);
    consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    localStorage.clear();
    consoleError.mockRestore();
    vi.useRealTimers();
  });

  async function renderApp(): Promise<void> {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });
  }

  async function openViaDialog(meta: typeof fileA): Promise<void> {
    backend.OpenViaDialog.mockResolvedValueOnce(meta);
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
  }

  async function emit(name: string, data: Record<string, unknown>): Promise<void> {
    await act(async () => {
      runtime.handlers.get(name)?.({ data });
      await settle();
    });
  }

  function startJob(id: string, sequence: number, title: string, fileId: string): Promise<void> {
    return emit("quarry:job-start", {
      id,
      sequence,
      title,
      kind: "transform",
      fileId,
      completed: 0,
      total: 100,
    });
  }

  function notificationMessage(): string | null {
    return host.querySelector(".q-notification-message")?.textContent ?? null;
  }

  function tabButton(fileName: string): HTMLButtonElement {
    const button = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-sb-activate"))
      .find((candidate) => candidate.getAttribute("aria-label")?.startsWith(`${fileName},`));
    if (!button) throw new Error(`missing tab ${fileName}`);
    return button;
  }

  it("preserves the exact pasted path, including leading and trailing whitespace", async () => {
    const rawPath = "  /tmp/source with spaces.sql  ";
    backend.OpenFile.mockResolvedValueOnce({ ...fileA, path: rawPath });
    await renderApp();

    const input = host.querySelector<HTMLInputElement>(".q-path");
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    await act(async () => {
      setter?.call(input, rawPath);
      input?.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    const button = host.querySelector<HTMLButtonElement>(".q-empty-path .q-btn");
    expect(button?.disabled).toBe(false);

    await act(async () => {
      button?.click();
      await settle();
    });
    expect(backend.OpenFile).toHaveBeenCalledWith(rawPath);
  });

  it("keeps a newer job notification when cancellation and terminal events arrive out of order", async () => {
    backend.CancelJob.mockRejectedValueOnce(new Error("already committed"));
    await renderApp();
    await openViaDialog(fileA);
    await startJob("job-1", 1, "First export", fileA.fileId);

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-job .q-btn")?.click();
      runtime.handlers.get("quarry:job-start")?.({ data: {
        id: "job-2",
        sequence: 2,
        title: "Second export",
        kind: "transform",
        fileId: fileA.fileId,
        total: 100,
      } });
      await settle();
    });
    expect(notificationMessage()).toBe("Second export started");

    await emit("quarry:job-end", { id: "job-1", sequence: 1, status: "cancelled" });
    expect(notificationMessage()).toBe("Second export started");

    await emit("quarry:job-end", { id: "job-2", sequence: 2, status: "cancelled", note: "user cancelled" });
    const notification = host.querySelector<HTMLElement>(".q-notification");
    expect(notificationMessage()).toBe("Second export cancelled");
    expect(notification?.getAttribute("role")).toBe("status");
    expect(notification?.getAttribute("aria-live")).toBe("polite");
    expect(notification?.textContent).toContain("Job ID: job-2");
    expect(notification?.textContent).not.toContain("Job ID: job-1");
  });

  it("does not surface a stale file's completion after switching tabs", async () => {
    await renderApp();
    await openViaDialog(fileA);
    await openViaDialog(fileB);
    await act(async () => {
      tabButton("alpha.sql").click();
      await settle();
    });
    await startJob("job-a", 1, "Alpha export", fileA.fileId);
    expect(notificationMessage()).toBe("Alpha export started");

    await act(async () => {
      tabButton("beta.sql").click();
      await settle();
    });
    expect(host.querySelector(".q-notification")).toBeNull();

    await startJob("job-b", 2, "Beta export", fileB.fileId);
    await emit("quarry:job-end", { id: "job-a", sequence: 1, status: "completed" });
    expect(notificationMessage()).toBe("Beta export started");

    await emit("quarry:job-end", { id: "job-b", sequence: 2, status: "failed", note: "disk full" });
    const notification = host.querySelector<HTMLElement>(".q-notification");
    expect(notificationMessage()).toBe("Beta export failed");
    expect(notification?.getAttribute("role")).toBe("alert");
    expect(notification?.getAttribute("aria-live")).toBe("assertive");
    expect(notification?.textContent).toContain("Job ID: job-b");
  });

  it("replaces repeated errors, exposes bounded copy details, and supports dismissal", async () => {
    await renderApp();
    await openViaDialog(fileA);
    await act(async () => editorHarness.callbacks?.onError(new Error("first editor failure")));
    expect(notificationMessage()).toBe("first editor failure");
    await act(async () => editorHarness.callbacks?.onError(new Error("second editor failure")));

    const notification = host.querySelector<HTMLElement>(".q-notification");
    expect(notificationMessage()).toBe("second editor failure");
    expect(notification?.getAttribute("role")).toBe("alert");
    await act(async () => {
      Array.from(host.querySelectorAll<HTMLButtonElement>(".q-notification-actions .q-btn"))
        .find((button) => button.textContent?.includes("Copy details"))?.click();
      await settle();
    });
    expect(clipboardWrite).toHaveBeenCalledTimes(1);
    expect(String(clipboardWrite.mock.calls[0][0])).toContain("Editor: second editor failure");

    act(() => {
      Array.from(host.querySelectorAll<HTMLButtonElement>(".q-notification-actions .q-btn"))
        .find((button) => button.textContent?.includes("Dismiss"))?.click();
    });
    expect(host.querySelector(".q-notification")).toBeNull();
  });

  it("expires success deterministically while failed terminal status persists", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-12T12:00:00Z"));
    await renderApp();
    await openViaDialog(fileA);
    await startJob("job-success", 1, "Export", fileA.fileId);
    await emit("quarry:job-end", { id: "job-success", sequence: 1, status: "completed" });
    expect(notificationMessage()).toBe("Export completed");

    act(() => vi.advanceTimersByTime(9_999));
    expect(notificationMessage()).toBe("Export completed");
    act(() => vi.advanceTimersByTime(1));
    expect(host.querySelector(".q-notification")).toBeNull();

    await startJob("job-failure", 2, "Replace", fileA.fileId);
    await emit("quarry:job-end", { id: "job-failure", sequence: 2, status: "panicked", note: "unexpected panic" });
    expect(notificationMessage()).toBe("Replace failed");
    act(() => vi.advanceTimersByTime(60_000));
    expect(notificationMessage()).toBe("Replace failed");
  });
});
