import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
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
  CloseFile: vi.fn(),
  DiscardEdits: vi.fn(),
  FindNextRequest: vi.fn(),
  FindPrevRequest: vi.fn(),
  GetStagingState: vi.fn(),
  HarvestMatchesViaDialog: vi.fn(),
  OpenViaDialog: vi.fn(),
  ResolveLine: vi.fn(),
  SaveCopyViaDialog: vi.fn(),
  SearchAllRequest: vi.fn(),
}));
const editorHarness = vi.hoisted(() => ({
  attach: vi.fn(),
  clear: vi.fn(),
  callbacks: null as null | { onStatus: (status: unknown) => void; onMode: (enabled: boolean) => void },
  flush: vi.fn(),
  gotoByte: vi.fn(),
  gotoEnd: vi.fn(),
  refresh: vi.fn(),
  setEditMode: vi.fn(),
  showMatch: vi.fn(),
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
    constructor(_host: HTMLElement, callbacks: { onStatus: (status: unknown) => void; onMode: (enabled: boolean) => void }) {
      editorHarness.callbacks = callbacks;
    }

    async attach(fileId: string, detected: string, path: string, startByte: number, csvDelimiter = ""): Promise<void> {
      await editorHarness.attach(fileId, detected, path, startByte, csvDelimiter);
      editorHarness.callbacks?.onStatus({
        fileId,
        startByte,
        positionByte: startByte,
        endByte: startByte + 1024,
        firstLine: 1,
        lastLine: 20,
        atBof: startByte === 0,
        atEof: false,
        approx: startByte !== 0,
      });
    }

    async showMatch(...args: unknown[]): Promise<void> {
      editorHarness.showMatch(...args);
    }

    async gotoByte(offset: number): Promise<void> {
      await editorHarness.gotoByte(offset);
    }

    async gotoEnd(): Promise<void> {
      await editorHarness.gotoEnd();
    }

    async flush(): Promise<void> {
      await editorHarness.flush();
    }

    async refresh(): Promise<void> {
      await editorHarness.refresh();
    }

    async setEditMode(enabled: boolean): Promise<void> {
      await editorHarness.setEditMode(enabled);
      editorHarness.callbacks?.onMode(enabled);
    }

    clear(): void {
      editorHarness.clear();
    }

    destroy(): void {}
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

import App from "../src/App";
import {
  SEARCH_PLAIN_INPUT_MAX_BYTES,
  SEARCH_REGEX_INPUT_MAX_BYTES,
} from "../src/textInputLimits";

const fileA = {
  fileId: "file-a",
  path: "C:\\data\\alpha.sql",
  size: 1_000_000,
  encoding: "UTF-8",
  detected: "sql",
  binary: false,
  editable: true,
};
const fileB = { ...fileA, fileId: "file-b", path: "C:\\data\\beta.sql" };
const textFile = { ...fileA, fileId: "file-text", path: "C:\\data\\notes.log", detected: "text" };

async function settle(): Promise<void> {
  for (let i = 0; i < 12; i++) await Promise.resolve();
}

describe("per-tab navigation and search ownership", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    editorHarness.callbacks = null;
    editorHarness.attach.mockReset().mockResolvedValue(undefined);
    editorHarness.flush.mockReset().mockResolvedValue(undefined);
    editorHarness.gotoByte.mockReset().mockResolvedValue(undefined);
    editorHarness.gotoEnd.mockReset().mockResolvedValue(undefined);
    editorHarness.refresh.mockReset().mockResolvedValue(undefined);
    editorHarness.setEditMode.mockReset().mockResolvedValue(undefined);
    backend.GetStagingState.mockResolvedValue({
      editCount: 0,
      originalSize: 1_000_000,
      editedSize: 1_000_000,
      netDelta: 0,
      lengthPreserving: true,
      inPlaceEligible: false,
    });
    backend.HarvestMatchesViaDialog.mockResolvedValue({ outputPath: "C:\\exports\\matches.txt", recordsWritten: 1 });
    backend.DiscardEdits.mockResolvedValue({
      editCount: 0,
      originalSize: 1_000_000,
      editedSize: 1_000_000,
      netDelta: 0,
      lengthPreserving: true,
      inPlaceEligible: false,
    });
    backend.CloseFile.mockResolvedValue(undefined);
    backend.SaveCopyViaDialog.mockResolvedValue({
      mode: "copy",
      outputPath: "C:\\exports\\saved.sql",
      bytesWritten: 1_000_000,
    });
    let searchRequest = 0;
    backend.BeginSearchRequest.mockReset().mockImplementation(async () => `search${++searchRequest}`);
    backend.CancelSearch.mockReset().mockResolvedValue(true);
    backend.FindNextRequest.mockReset().mockResolvedValue({ found: false });
    backend.FindPrevRequest.mockReset().mockResolvedValue({ found: false });
    backend.SearchAllRequest.mockReset().mockResolvedValue({ hits: [] });
    backend.ResolveLine.mockReset().mockResolvedValue({
      offset: 0,
      resolvedLine: 1,
      exact: true,
      found: true,
      indexComplete: true,
      limited: false,
    });
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    localStorage.clear();
  });

  async function renderApp(): Promise<void> {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });
  }

  async function openFile(meta: typeof fileA): Promise<void> {
    backend.OpenViaDialog.mockResolvedValueOnce(meta);
    const open = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-top button"))
      .find((button) => button.textContent?.includes("Open file"));
    await act(async () => {
      open?.click();
      await settle();
    });
  }

  function emitPosition(fileId: string, positionByte: number): void {
    act(() => editorHarness.callbacks?.onStatus({
      fileId,
      startByte: Math.max(0, positionByte - 256),
      positionByte,
      endByte: positionByte + 768,
      firstLine: 100,
      lastLine: 120,
      atBof: false,
      atEof: false,
      approx: false,
    }));
  }

  function setQuery(value: string): void {
    const input = host.querySelector<HTMLInputElement>('input[aria-label="Find text"]');
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    act(() => {
      setter?.call(input, value);
      input?.dispatchEvent(new Event("input", { bubbles: true }));
    });
  }

  function openSearch(): void {
    act(() => window.dispatchEvent(new KeyboardEvent("keydown", {
      key: "f",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    })));
  }

  function openGoto(): void {
    act(() => window.dispatchEvent(new KeyboardEvent("keydown", {
      key: "g",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    })));
  }

  function setGotoValue(value: string): void {
    const input = host.querySelector<HTMLInputElement>('input[aria-label="Line, byte offset, or percentage"]');
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    act(() => {
      setter?.call(input, value);
      input?.dispatchEvent(new Event("input", { bubbles: true }));
    });
  }

  async function listAll(): Promise<void> {
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="List up to 1,000 matches"]')?.click();
      await settle();
    });
  }

  function tabButton(fileName: string): HTMLButtonElement {
    const match = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-sb-activate"))
      .find((button) => button.getAttribute("aria-label")?.startsWith(fileName + ","));
    if (!match) throw new Error(`missing tab ${fileName}`);
    return match;
  }

  function closeButton(fileName: string): HTMLButtonElement {
    const match = host.querySelector<HTMLButtonElement>(`button[aria-label="Close ${fileName}"]`);
    if (!match) throw new Error(`missing close button ${fileName}`);
    return match;
  }

  it("restores each tab's deep owned raw position across round trips", async () => {
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 300_000);
    await openFile(fileB);
    emitPosition("file-b", 700_000);

    await act(async () => {
      tabButton("alpha.sql").click();
      await settle();
    });
    expect(editorHarness.attach).toHaveBeenLastCalledWith("file-a", "sql", fileA.path, 300_000, "");

    await act(async () => {
      tabButton("beta.sql").click();
      await settle();
    });
    expect(editorHarness.attach).toHaveBeenLastCalledWith("file-b", "sql", fileB.path, 700_000, "");
  });

  it("rolls back a failed new-file attach, closes its session, and preserves the previous editor owner", async () => {
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 345_678);
    backend.CloseFile.mockClear();
    editorHarness.attach.mockRejectedValueOnce(new Error("initial window rejected"));

    await openFile(fileB);

    expect(backend.CloseFile).toHaveBeenCalledOnce();
    expect(backend.CloseFile).toHaveBeenCalledWith("file-b");
    expect(tabButton("alpha.sql").getAttribute("aria-current")).toBe("page");
    expect(host.querySelector('button[aria-label^="beta.sql,"]')).toBeNull();
    expect(host.textContent).toContain("initial window rejected");

    openGoto();
    setGotoValue("0x60000");
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Go to position"]')?.click();
      await settle();
    });
    expect(editorHarness.gotoByte).toHaveBeenCalledWith(0x60000);
  });

  it("fails ambiguous encoding closed with actionable recovery and no provisional tab", async () => {
    const ambiguous = {
      ...fileB,
      fileId: "ambiguous",
      path: "C:\\data\\ambiguous.txt",
      encoding: "UTF-16",
      encodingRequiresConfirmation: true,
      detected: "text",
      editable: false,
    };
    await renderApp();
    await openFile(ambiguous);

    expect(editorHarness.attach).not.toHaveBeenCalled();
    expect(backend.CloseFile).toHaveBeenCalledWith("ambiguous");
    expect(host.querySelector('button[aria-label^="ambiguous.txt,"]')).toBeNull();
    expect(host.textContent).toContain("Encoding detection is ambiguous");
    expect(host.textContent).toContain("Convert a copy to UTF-8");
    expect(host.textContent).toContain("source file was not changed");
  });

  it("bookmarks the exact owned position rather than the start of its editor window", async () => {
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 345_678);

    const viewMenu = Array.from(host.querySelectorAll<HTMLButtonElement>('[aria-haspopup="menu"]'))
      .find((button) => button.textContent === "View");
    await act(async () => {
      viewMenu?.click();
      await settle();
    });
    const add = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
      .find((button) => button.textContent?.includes("Add bookmark here"));
    await act(async () => {
      add?.click();
      await settle();
    });

    const bookmark = host.querySelector<HTMLButtonElement>(".q-bookmark-seek");
    expect(bookmark?.getAttribute("aria-label")).toContain("byte 345678");
    await act(async () => {
      bookmark?.click();
      await settle();
    });
    expect(editorHarness.gotoByte).toHaveBeenCalledWith(345_678);
  });

  it("hands focus from a removed inactive sidebar close button to its nearest successor", async () => {
    await renderApp();
    await openFile(fileA);
    await openFile(fileB);

    const closeA = closeButton("alpha.sql");
    closeA.focus();
    await act(async () => {
      closeA.click();
      await settle();
    });

    expect(host.querySelector('button[aria-label="Close alpha.sql"]')).toBeNull();
    expect(document.activeElement).toBe(closeButton("beta.sql"));
  });

  it("hands focus from a removed active sidebar close button to the remaining tab", async () => {
    await renderApp();
    await openFile(fileA);
    await openFile(fileB);
    await act(async () => {
      tabButton("alpha.sql").click();
      await settle();
    });

    const closeA = closeButton("alpha.sql");
    closeA.focus();
    await act(async () => {
      closeA.click();
      await settle();
    });

    expect(document.activeElement).toBe(closeButton("beta.sql"));
  });

  it("moves focus to the empty-state Open button after closing the last tab", async () => {
    await renderApp();
    await openFile(fileA);

    const closeA = closeButton("alpha.sql");
    closeA.focus();
    await act(async () => {
      closeA.click();
      await settle();
    });

    const emptyOpen = host.querySelector<HTMLButtonElement>("button.q-empty-open");
    expect(emptyOpen).not.toBeNull();
    expect(document.activeElement).toBe(emptyOpen);
  });

  it("restores the retained close button after cancellation or backend failure", async () => {
    const stagedState = {
      editCount: 1,
      originalSize: 1_000_000,
      editedSize: 1_000_010,
      netDelta: 10,
      lengthPreserving: false,
      inPlaceEligible: false,
    };
    backend.GetStagingState.mockResolvedValue(stagedState);
    await renderApp();
    await openFile(fileA);

    let closeA = closeButton("alpha.sql");
    closeA.focus();
    await act(async () => {
      closeA.click();
      await settle();
    });
    const cancel = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-close-actions button"))
      .find((button) => button.textContent === "Cancel");
    await act(async () => {
      cancel?.click();
      await settle();
    });
    closeA = closeButton("alpha.sql");
    expect(document.activeElement).toBe(closeA);

    backend.GetStagingState.mockResolvedValue({
      ...stagedState,
      editCount: 0,
      editedSize: 1_000_000,
      netDelta: 0,
      lengthPreserving: true,
    });
    backend.CloseFile.mockRejectedValueOnce(new Error("backend close failed"));
    closeA.focus();
    await act(async () => {
      closeA.click();
      await settle();
    });
    expect(document.activeElement).toBe(closeButton("alpha.sql"));
  });

  it.each(["save copy", "discard"])(
    "hands focus off after a staged inactive tab is resolved with %s",
    async (choice) => {
      backend.GetStagingState.mockResolvedValue({
        editCount: 1,
        originalSize: 1_000_000,
        editedSize: 1_000_010,
        netDelta: 10,
        lengthPreserving: false,
        inPlaceEligible: false,
      });
      await renderApp();
      await openFile(fileA);
      await openFile(fileB);

      const closeA = closeButton("alpha.sql");
      closeA.focus();
      await act(async () => {
        closeA.click();
        await settle();
      });
      const decision = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-close-actions button"))
        .find((button) => button.textContent?.toLowerCase().startsWith(choice));
      expect(decision).toBeDefined();
      await act(async () => {
        decision?.click();
        await settle();
      });

      expect(document.activeElement).toBe(closeButton("beta.sql"));
    },
  );

  it("makes an attached editor read-only before close-discard clears its backend session", async () => {
    const stagedState = {
      editCount: 1,
      originalSize: 1_000_000,
      editedSize: 1_000_010,
      netDelta: 10,
      lengthPreserving: false,
      inPlaceEligible: false,
    };
    backend.GetStagingState.mockResolvedValue(stagedState);
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 345_678);

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Close alpha.sql"]')?.click();
      await settle();
    });
    const discard = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-close-actions button"))
      .find((button) => button.textContent === "Discard");
    expect(discard).toBeDefined();

    await act(async () => {
      discard?.click();
      await settle();
    });

    expect(editorHarness.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.DiscardEdits).toHaveBeenCalledWith("file-a");
    expect(editorHarness.setEditMode.mock.invocationCallOrder[0])
      .toBeLessThan(backend.DiscardEdits.mock.invocationCallOrder[0]);
    expect(backend.CloseFile).toHaveBeenCalledWith("file-a");
    expect(host.querySelector('button[aria-label^="alpha.sql,"]')).toBeNull();
  });

  it("keeps staging and the tab when the editor cannot become read-only before close-discard", async () => {
    const stagedState = {
      editCount: 1,
      originalSize: 1_000_000,
      editedSize: 1_000_010,
      netDelta: 10,
      lengthPreserving: false,
      inPlaceEligible: false,
    };
    backend.GetStagingState.mockResolvedValue(stagedState);
    editorHarness.setEditMode.mockRejectedValueOnce(new Error("read-only reload failed"));
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 456_789);

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Close alpha.sql"]')?.click();
      await settle();
    });
    const discard = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-close-actions button"))
      .find((button) => button.textContent === "Discard");
    await act(async () => {
      discard?.click();
      await settle();
    });

    expect(backend.DiscardEdits).not.toHaveBeenCalled();
    expect(backend.CloseFile).not.toHaveBeenCalled();
    expect(host.querySelector('button[aria-label^="alpha.sql,"]')).not.toBeNull();
    expect(host.textContent).toContain("read-only reload failed");
  });

  it("starts an initial Find Previous at the current owned mid-file position", async () => {
    await renderApp();
    await openFile(fileA);
    emitPosition("file-a", 456_789);
    openSearch();
    setQuery("needle");

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Find previous match"]')?.click();
      await settle();
    });

    expect(backend.BeginSearchRequest).toHaveBeenCalledWith("file-a");
    expect(backend.FindPrevRequest).toHaveBeenCalledWith("search1", "needle", 456_789, false, false, false);
  });

  it("bounds search input by UTF-8 bytes and tightens an existing query for regex", async () => {
    await renderApp();
    await openFile(fileA);
    openSearch();

    const input = host.querySelector<HTMLInputElement>('input[aria-label="Find text"]');
    expect(input?.maxLength).toBe(SEARCH_PLAIN_INPUT_MAX_BYTES);
    const justOverRegex = "🙂".repeat((SEARCH_REGEX_INPUT_MAX_BYTES / 4) + 1);
    setQuery(justOverRegex);
    expect(new TextEncoder().encode(input?.value ?? "")).toHaveLength(SEARCH_REGEX_INPUT_MAX_BYTES + 4);

    act(() => host.querySelector<HTMLButtonElement>('button[aria-label="Use regular expression"]')?.click());
    expect(input?.maxLength).toBe(SEARCH_REGEX_INPUT_MAX_BYTES);
    expect(new TextEncoder().encode(input?.value ?? "")).toHaveLength(SEARCH_REGEX_INPUT_MAX_BYTES);
    expect(input?.value).toBe("🙂".repeat(SEARCH_REGEX_INPUT_MAX_BYTES / 4));
    expect(host.querySelector('[role="search"]')?.textContent).toContain("limited to 64 KiB of UTF-8");

    setQuery("é".repeat((SEARCH_REGEX_INPUT_MAX_BYTES / 2) + 1));
    expect(new TextEncoder().encode(input?.value ?? "")).toHaveLength(SEARCH_REGEX_INPUT_MAX_BYTES);
    expect(host.querySelector('[role="search"]')?.textContent).toContain("limited to 64 KiB of UTF-8");
  });

  it("never sends plain-search text to the regex-only match extractor", async () => {
    await renderApp();
    await openFile(fileA);
    openSearch();
    setQuery("value.*");

    const extract = host.querySelector<HTMLButtonElement>(
      'button[aria-label="Extract all regular-expression matches to a new file"]',
    );
    expect(extract?.disabled).toBe(true);
    expect(extract?.title).toContain("Turn on regular-expression search");
    expect(backend.HarvestMatchesViaDialog).not.toHaveBeenCalled();

    act(() => host.querySelector<HTMLButtonElement>('button[aria-label="Use regular expression"]')?.click());
    expect(extract?.disabled).toBe(false);
    await act(async () => {
      extract?.click();
      await settle();
    });
    expect(backend.HarvestMatchesViaDialog).toHaveBeenCalledWith("file-a", "value.*", true);
  });

  it("keeps bounded line fallback visible and navigates to the actual resolved line", async () => {
    backend.ResolveLine.mockResolvedValueOnce({
      offset: 0,
      resolvedLine: 1,
      exact: false,
      found: true,
      indexComplete: true,
      limited: true,
    });
    await renderApp();
    await openFile(fileA);
    openGoto();
    setGotoValue("2");

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Go to position"]')?.click();
      await settle();
    });

    expect(backend.ResolveLine).toHaveBeenCalledWith("file-a", 2);
    expect(editorHarness.gotoByte).toHaveBeenCalledWith(0);
    const gotoRegion = host.querySelector<HTMLElement>('[aria-label="Go to file position"]');
    expect(gotoRegion).not.toBeNull();
    expect(gotoRegion?.textContent).toContain("bounded scan limit");
    expect(gotoRegion?.textContent).toContain("line 1 instead");
    expect(gotoRegion?.textContent).not.toContain("Line does not exist");
  });

  it("does not describe a limited lookup without a fallback as a missing line", async () => {
    backend.ResolveLine.mockResolvedValueOnce({
      offset: 0,
      resolvedLine: 0,
      exact: false,
      found: false,
      indexComplete: true,
      limited: true,
    });
    await renderApp();
    await openFile(fileA);
    openGoto();
    setGotoValue("2");

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Go to position"]')?.click();
      await settle();
    });

    expect(editorHarness.gotoByte).not.toHaveBeenCalled();
    const gotoRegion = host.querySelector<HTMLElement>('[aria-label="Go to file position"]');
    expect(gotoRegion?.textContent).toContain("bounded scan limit");
    expect(gotoRegion?.textContent).not.toContain("Line does not exist");
  });

  it("invalidates result actions synchronously on query and option changes", async () => {
    backend.SearchAllRequest
      .mockResolvedValueOnce({ hits: [{ offset: 10, length: 2, line: 2, preview: "old query" }] })
      .mockResolvedValueOnce({ hits: [{ offset: 20, length: 2, line: 3, preview: "new query" }] });
    await renderApp();
    await openFile(fileA);
    openSearch();
    setQuery("old");
    await listAll();

    const oldResult = host.querySelector<HTMLButtonElement>(".q-result-button");
    act(() => {
      const input = host.querySelector<HTMLInputElement>('input[aria-label="Find text"]');
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
      setter?.call(input, "new");
      input?.dispatchEvent(new Event("input", { bubbles: true }));
      oldResult?.click();
    });
    expect(editorHarness.showMatch).not.toHaveBeenCalled();
    expect(host.querySelector(".q-result-button")).toBeNull();

    await listAll();
    const newResult = host.querySelector<HTMLButtonElement>(".q-result-button");
    const regexToggle = host.querySelector<HTMLButtonElement>('button[aria-label="Use regular expression"]');
    act(() => {
      regexToggle?.click();
      newResult?.click();
    });
    expect(editorHarness.showMatch).not.toHaveBeenCalled();
    expect(host.querySelector(".q-result-button")).toBeNull();
    expect(backend.SearchAllRequest.mock.calls).toEqual([
      ["search1", "old", false, false, false, 1000],
      ["search2", "new", false, false, false, 1000],
    ]);
  });

  it("passes live search ownership into a listed-match editor request", async () => {
    backend.SearchAllRequest.mockResolvedValueOnce({
      hits: [{ offset: 10, length: 2, line: 2, preview: "owned result" }],
    });
    await renderApp();
    await openFile(fileA);
    openSearch();
    setQuery("owned");
    await listAll();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-result-button")?.click();
      await settle();
    });
    expect(editorHarness.showMatch).toHaveBeenCalledOnce();
    const isCurrent = editorHarness.showMatch.mock.calls[0][5] as (() => boolean) | undefined;
    expect(isCurrent).toBeTypeOf("function");
    expect(isCurrent?.()).toBe(true);

    setQuery("replacement");
    expect(isCurrent?.()).toBe(false);
  });

  it("ignores an out-of-order tab search and keeps only the exact current context", async () => {
    const pendingA = deferred<{ hits: Array<{ offset: number; length: number; line: number; preview: string }> }>();
    backend.SearchAllRequest
      .mockImplementationOnce(() => pendingA.promise)
      .mockResolvedValueOnce({ hits: [{ offset: 200, length: 1, line: 20, preview: "result B" }] });
    await renderApp();
    await openFile(fileA);
    openSearch();
    setQuery("alpha");
    act(() => host.querySelector<HTMLButtonElement>('button[aria-label="List up to 1,000 matches"]')?.click());
    await act(async () => { await settle(); });

    await openFile(fileB);
    setQuery("beta");
    await listAll();
    expect(host.textContent).toContain("result B");

    await act(async () => {
      pendingA.resolve({ hits: [{ offset: 100, length: 1, line: 10, preview: "stale result A" }] });
      await settle();
    });
    expect(host.textContent).toContain("result B");
    expect(host.textContent).not.toContain("stale result A");
  });

  it("cancels only the active search and never applies its late result", async () => {
    const pending = deferred<{ hits: Array<{ offset: number; length: number; line: number; preview: string }> }>();
    backend.SearchAllRequest.mockImplementationOnce(() => pending.promise);
    await renderApp();
    await openFile(fileA);
    openSearch();
    setQuery("needle");

    act(() => host.querySelector<HTMLButtonElement>('button[aria-label="List up to 1,000 matches"]')?.click());
    await act(async () => { await settle(); });
    const cancel = host.querySelector<HTMLButtonElement>('button[aria-label="Cancel active search"]');
    expect(cancel).not.toBeNull();

    act(() => cancel?.click());
    expect(backend.CancelSearch).toHaveBeenCalledWith("search1");
    expect(host.querySelector('button[aria-label="Cancel active search"]')).toBeNull();
    expect(host.textContent).toContain("Search cancelled");

    const secondPending = deferred<{ hits: Array<{ offset: number; length: number; line: number; preview: string }> }>();
    backend.SearchAllRequest.mockImplementationOnce(() => secondPending.promise);
    act(() => host.querySelector<HTMLButtonElement>('button[aria-label="List up to 1,000 matches"]')?.click());
    await act(async () => { await settle(); });
    const currentCancel = host.querySelector<HTMLButtonElement>('button[aria-label="Cancel active search"]');
    expect(currentCancel).not.toBeNull();

    // A detached button retains its old closure. Its delayed click must remain
    // scoped to search1 and must not read/cancel the newer active search2.
    act(() => cancel?.click());
    expect(backend.CancelSearch.mock.calls).toEqual([["search1"]]);
    expect(host.querySelector('button[aria-label="Cancel active search"]')).toBe(currentCancel);
    act(() => currentCancel?.click());
    expect(backend.CancelSearch.mock.calls).toEqual([["search1"], ["search2"]]);

    await act(async () => {
      pending.resolve({ hits: [{ offset: 999, length: 6, line: 99, preview: "late canceled result" }] });
      secondPending.resolve({ hits: [{ offset: 1000, length: 6, line: 100, preview: "newer canceled result" }] });
      await settle();
    });
    expect(host.textContent).not.toContain("late canceled result");
    expect(host.textContent).not.toContain("newer canceled result");
    expect(editorHarness.showMatch).not.toHaveBeenCalled();
  });

  it("rejects a stale result click after tab-switch invalidation", async () => {
    backend.SearchAllRequest.mockResolvedValue({ hits: [{ offset: 50, length: 3, line: 5, preview: "file A result" }] });
    await renderApp();
    await openFile(fileA);
    await openFile(fileB);
    await act(async () => {
      tabButton("alpha.sql").click();
      await settle();
    });
    openSearch();
    setQuery("value");
    await listAll();
    const stale = host.querySelector<HTMLButtonElement>(".q-result-button");
    editorHarness.showMatch.mockClear();

    await act(async () => {
      tabButton("beta.sql").click();
      stale?.click();
      await settle();
    });

    expect(editorHarness.showMatch).not.toHaveBeenCalled();
    expect(host.querySelector(".q-result-button")).toBeNull();
  });

  it("closes data tools when the active file does not support them", async () => {
    await renderApp();
    await openFile(fileA);

    const viewMenu = Array.from(host.querySelectorAll<HTMLButtonElement>('[aria-haspopup="menu"]'))
      .find((button) => button.textContent === "View");
    await act(async () => {
      viewMenu?.click();
      await settle();
    });
    const dataTools = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
      .find((button) => button.textContent?.includes("Data tools panel"));
    expect(dataTools, host.innerHTML).toBeDefined();
    expect(dataTools?.disabled).toBe(false);
    await act(async () => {
      dataTools?.click();
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      await settle();
    });
    expect(host.querySelector(".q-tools, .q-panel-loading-tools")).not.toBeNull();

    await openFile(textFile);
    expect(host.querySelector(".q-tools, .q-panel-loading-tools")).toBeNull();

    await act(async () => {
      tabButton("alpha.sql").click();
      await settle();
    });
    expect(host.querySelector(".q-tools, .q-panel-loading-tools")).toBeNull();
  });
});
