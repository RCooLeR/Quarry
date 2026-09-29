import type { EditorView } from "@codemirror/view";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QuarryEditor } from "../src/editor/QuarryEditor";
import type {
  EditorCallbacks,
  StagingStateData,
  WindowData,
} from "../src/editor/QuarryEditor";

const fileService = vi.hoisted(() => ({
  GetEditWindow: vi.fn(),
  GetEditTailWindow: vi.fn(),
  GetMatchWindow: vi.fn(),
  GetNextWindow: vi.fn(),
  GetPrevWindow: vi.fn(),
  GetTailWindow: vi.fn(),
  GetWindow: vi.fn(),
  OpenFile: vi.fn(),
  StageEdit: vi.fn(),
}));

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({
  FileService: fileService,
}));

interface EditorInternals {
  view: EditorView;
  fileId: string;
  detected: string;
  path: string;
  dirty: boolean;
  startByte: number;
  editWinStart: number;
  editWinOrigLen: number;
  atBof: boolean;
  atEof: boolean;
  loadNext: () => Promise<void>;
  loadPrev: () => Promise<void>;
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function windowData(fileId: string, startByte: number, text: string): WindowData {
  return {
    fileId,
    startByte,
    nextByte: startByte + new TextEncoder().encode(text).length,
    text,
    lineOffsets: [startByte],
    lineNumbers: [startByte + 1],
    atBof: startByte === 0,
    atEof: true,
    approx: startByte !== 0,
  };
}

const stagedState: StagingStateData = {
  editCount: 1,
  originalSize: 8,
  editedSize: 8,
  netDelta: 0,
  lengthPreserving: true,
  inPlaceEligible: true,
};

function callbacks(): EditorCallbacks & {
  onStatus: ReturnType<typeof vi.fn>;
  onDirty: ReturnType<typeof vi.fn>;
  onStaging: ReturnType<typeof vi.fn>;
  onMode: ReturnType<typeof vi.fn>;
  onError: ReturnType<typeof vi.fn>;
} {
  return {
    onStatus: vi.fn(),
    onDirty: vi.fn(),
    onStaging: vi.fn(),
    onMode: vi.fn(),
    onError: vi.fn(),
  };
}

function internals(editor: QuarryEditor): EditorInternals {
  return editor as unknown as EditorInternals;
}

function replaceDocument(editor: QuarryEditor, text: string): void {
  const view = internals(editor).view;
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length, insert: text },
  });
}

function documentText(editor: QuarryEditor): string {
  return internals(editor).view.state.doc.toString();
}

async function settleRequests(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

describe("QuarryEditor safety", () => {
  let editor: QuarryEditor | null = null;
  let cb: ReturnType<typeof callbacks>;
  let consoleError: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.resetAllMocks();
    vi.useRealTimers();
    document.body.innerHTML = '<div id="editor"></div>';
    cb = callbacks();
    consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    editor?.destroy();
    editor = null;
    consoleError.mockRestore();
    vi.useRealTimers();
    document.body.replaceChildren();
  });

  async function createEditingEditor(fileId = "file-a", text = "original"): Promise<QuarryEditor> {
    fileService.GetWindow.mockResolvedValueOnce(windowData(fileId, 0, text));
    fileService.GetEditWindow.mockResolvedValueOnce(windowData(fileId, 0, text));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach(fileId);
    await editor.setEditMode(true);
    return editor;
  }

  it("preserves CRLF and lone CR code units in an editable window", async () => {
    const text = "first\r\nsecond\rthird\n";
    const ed = await createEditingEditor("file-a", text);
    expect(documentText(ed)).toBe(text);
    internals(ed).view.dispatch({ changes: { from: 0, to: 5, insert: "FIRST" } });
    fileService.StageEdit.mockResolvedValueOnce(stagedState);
    await ed.flush();
    expect(fileService.StageEdit).toHaveBeenLastCalledWith("file-a", 0, text.length, "FIRST\r\nsecond\rthird\n");
  });

  it("preserves pasted line endings instead of silently normalizing them", async () => {
    const ed = await createEditingEditor("file-a", "original\n");
    internals(ed).view.dispatch({ changes: { from: 0, insert: "pasted\r\ntext\r" } });
    fileService.StageEdit.mockResolvedValueOnce(stagedState);
    await ed.flush();
    expect(fileService.StageEdit).toHaveBeenLastCalledWith("file-a", 0, 9, "pasted\r\ntext\roriginal\n");
  });

  it("navigates to the staged tail while editing without installing rendered source text", async () => {
    const ed = await createEditingEditor("file-a", "original");
    replaceDocument(ed, "changed");
    fileService.StageEdit.mockResolvedValueOnce(stagedState);
    fileService.GetEditTailWindow.mockResolvedValueOnce(windowData("file-a", 2000, "edited tail"));

    await ed.gotoEnd();

    expect(fileService.GetTailWindow).not.toHaveBeenCalled();
    expect(fileService.GetEditTailWindow).toHaveBeenLastCalledWith("file-a", 1 << 20);
    expect(documentText(ed)).toBe("edited tail");
    expect(ed.isEditing()).toBe(true);
    expect(internals(ed).view.state.selection.main.head).toBe("edited tail".length);
  });

  it("ignores an edited-tail response after a local edit supersedes navigation", async () => {
    const ed = await createEditingEditor();
    const tail = deferred<WindowData>();
    fileService.GetEditTailWindow.mockReturnValueOnce(tail.promise);
    const loading = ed.gotoEnd();
    await settleRequests();
    replaceDocument(ed, "new local edit");
    tail.resolve(windowData("file-a", 2000, "stale tail"));
    await loading;

    expect(fileService.GetEditWindow).toHaveBeenCalledTimes(1);
    expect(fileService.GetTailWindow).not.toHaveBeenCalled();
    expect(documentText(ed)).toBe("new local edit");
    expect(internals(ed).dirty).toBe(true);
  });

  it.each(["", "x", "😀", "last line\n", "a😀\n", "first\nlast"])("installs the exact edited tail %j and selects its end", async (text) => {
    const ed = await createEditingEditor();
    fileService.GetEditTailWindow.mockResolvedValueOnce(windowData("file-a", 0, text));
    await ed.gotoEnd();
    expect(fileService.GetEditTailWindow).toHaveBeenLastCalledWith("file-a", 1 << 20);
    expect(fileService.GetTailWindow).not.toHaveBeenCalled();
    expect(documentText(ed)).toBe(text);
    expect(internals(ed).view.state.selection.main.head).toBe(text.length);
  });

  it("keeps the current editable window intact when edited-tail loading fails", async () => {
    const ed = await createEditingEditor();
    fileService.GetEditTailWindow.mockRejectedValueOnce(new Error("tail unavailable"));
    await expect(ed.gotoEnd()).rejects.toThrow("tail unavailable");
    expect(fileService.GetEditWindow).toHaveBeenCalledTimes(1);
    expect(documentText(ed)).toBe("original");
  });

  it("keeps the viewer read-only until the backend edit window succeeds", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "read-only source"));
    const editWindow = deferred<WindowData>();
    fileService.GetEditWindow.mockReturnValueOnce(editWindow.promise);
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    const enabling = editor.setEditMode(true);
    await settleRequests();
    expect(editor.isEditing()).toBe(false);
    expect(internals(editor).view.contentDOM.getAttribute("contenteditable")).toBe("false");
    expect(documentText(editor)).toBe("read-only source");

    const failure = new Error("backend rejected edit capability");
    editWindow.reject(failure);
    await expect(enabling).rejects.toBe(failure);
    expect(editor.isEditing()).toBe(false);
    expect(internals(editor).view.contentDOM.getAttribute("contenteditable")).toBe("false");
    expect(internals(editor).view.dom.classList.contains("cm-editing")).toBe(false);
    expect(documentText(editor)).toBe("read-only source");
    expect(cb.onMode).not.toHaveBeenCalledWith(true);
  });

  it("rejects a failed flush, remains dirty, and blocks navigation and attach", async () => {
    const ed = await createEditingEditor();
    replaceDocument(ed, "changed");

    const failure = new Error("stage failed");
    fileService.StageEdit.mockRejectedValue(failure);

    await expect(ed.flush()).rejects.toBe(failure);
    expect(internals(ed).dirty).toBe(true);
    expect(cb.onDirty).toHaveBeenLastCalledWith(true);
    expect(documentText(ed)).toBe("changed");

    const editWindowCalls = fileService.GetEditWindow.mock.calls.length;
    await expect(ed.gotoByte(4096)).rejects.toBe(failure);
    await expect(ed.gotoEnd()).rejects.toBe(failure);
    await expect(ed.showMatch(4096, 4, "test", false, true)).rejects.toBe(failure);
    await expect(ed.refresh()).rejects.toBe(failure);
    expect(fileService.GetEditWindow).toHaveBeenCalledTimes(editWindowCalls);

    await expect(ed.setEditMode(false)).rejects.toBe(failure);
    expect(ed.isEditing()).toBe(true);

    internals(ed).atEof = false;
    await expect(internals(ed).loadNext()).rejects.toBe(failure);
    expect(fileService.GetNextWindow).not.toHaveBeenCalled();
    internals(ed).atBof = false;
    await expect(internals(ed).loadPrev()).rejects.toBe(failure);
    expect(fileService.GetPrevWindow).not.toHaveBeenCalled();

    await expect(ed.attach("file-b")).rejects.toBe(failure);
    expect(fileService.GetWindow).not.toHaveBeenCalledWith("file-b", expect.anything(), expect.anything());
    expect(documentText(ed)).toBe("changed");
    expect(internals(ed).dirty).toBe(true);
  });

  it("clears an editing window as blank read-only state and reports mode and dirty resets", async () => {
    const ed = await createEditingEditor();
    replaceDocument(ed, "discarded local text");
    expect(ed.isEditing()).toBe(true);
    expect(internals(ed).dirty).toBe(true);
    expect(internals(ed).view.contentDOM.getAttribute("contenteditable")).toBe("true");

    ed.clear();

    expect(ed.isEditing()).toBe(false);
    expect(internals(ed).fileId).toBe("");
    expect(internals(ed).dirty).toBe(false);
    expect(documentText(ed)).toBe("");
    expect(internals(ed).view.contentDOM.getAttribute("contenteditable")).toBe("false");
    expect(internals(ed).view.dom.classList.contains("cm-editing")).toBe(false);
    expect(cb.onMode).toHaveBeenLastCalledWith(false);
    expect(cb.onDirty).toHaveBeenLastCalledWith(false);
    expect(cb.onStatus).toHaveBeenLastCalledWith(null);
  });

  it("loads the backend tail window and anchors it at the bottom", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "initial"));
    fileService.GetTailWindow.mockResolvedValueOnce({
      ...windowData("file-a", 900, "last\nlines"),
      lineOffsets: [900, 905],
      lineNumbers: [10, 11],
      atBof: false,
      atEof: true,
    });
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    await editor.gotoEnd();

    expect(fileService.GetTailWindow).toHaveBeenCalledWith("file-a", 1 << 20);
    expect(documentText(editor)).toBe("last\nlines");
    expect(internals(editor).startByte).toBe(900);
    expect(cb.onStatus.mock.calls.at(-1)?.[0]?.atEof).toBe(true);
  });

  it("pairs refreshed metadata with its new tail and rejects a pre-refresh tail completion", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "SELECT old_value;"));
    const staleTail = deferred<WindowData>();
    fileService.GetTailWindow
      .mockImplementationOnce(() => staleTail.promise)
      .mockResolvedValueOnce(windowData("file-a", 900, "SELECT fresh_bytes;"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a", "sql", "old.sql");
    expect(host.querySelector(".q-sql-keyword")?.textContent).toBe("SELECT");

    const staleLoad = editor.gotoEnd();
    await settleRequests();
    const refreshedMeta = {
      fileId: "file-a",
      path: "rotated.bin",
      size: 19,
      encoding: "binary",
      detected: "binary",
      binary: true,
      editable: false,
    };
    await editor.refreshSource(refreshedMeta);

    expect(documentText(editor)).toBe("SELECT fresh_bytes;");
    expect(host.querySelector(".q-sql-keyword")).toBeNull();
    expect(internals(editor).detected).toBe("binary");
    expect(internals(editor).path).toBe("rotated.bin");

    staleTail.resolve(windowData("file-a", 500, "SELECT stale_bytes;"));
    await staleLoad;
    expect(documentText(editor)).toBe("SELECT fresh_bytes;");
    expect(internals(editor).startByte).toBe(900);
  });

  it("reports an owned exact raw position for the selected visible line", async () => {
    fileService.GetWindow.mockResolvedValueOnce({
      ...windowData("file-a", 100, "alpha\nbeta\ngamma"),
      lineOffsets: [100, 106, 111],
      lineNumbers: [20, 21, 22],
      atBof: false,
    });
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a", "", "", 100);

    expect(cb.onStatus.mock.calls.at(-1)?.[0]).toMatchObject({
      fileId: "file-a",
      startByte: 100,
      positionByte: 100,
    });

    const view = internals(editor).view;
    view.dispatch({ selection: { anchor: view.state.doc.line(3).from } });

    expect(cb.onStatus.mock.calls.at(-1)?.[0]).toMatchObject({
      fileId: "file-a",
      startByte: 100,
      positionByte: 111,
    });
  });

  it("uses the backend raw-byte to UTF-16 span for the exact repeated match", async () => {
    const text = "😀漢 target and target";
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, text));
    fileService.GetMatchWindow.mockResolvedValueOnce({
      window: windowData("file-a", 0, text),
      found: true,
      from: 15,
      to: 21,
    });
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    await editor.showMatch(20, 6, "target", false, true);

    expect(fileService.GetMatchWindow).toHaveBeenCalledWith("file-a", 20, 6, 1 << 20);
    const view = internals(editor).view;
    expect(view.state.sliceDoc(view.state.selection.main.from, view.state.selection.main.to)).toBe("target");
    expect(view.state.selection.main.from).toBe(15);
  });

  it("does not apply a match window after its search ownership is canceled", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "current window"));
    const pending = deferred<{
      window: WindowData;
      found: boolean;
      from: number;
      to: number;
    }>();
    fileService.GetMatchWindow.mockImplementationOnce(() => pending.promise);
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    let current = true;
    const showing = editor.showMatch(100, 6, "target", false, true, () => current);
    await Promise.resolve();
    await Promise.resolve();
    expect(fileService.GetMatchWindow).toHaveBeenCalledOnce();

    current = false;
    pending.resolve({
      window: windowData("file-a", 90, "stale target window"),
      found: true,
      from: 6,
      to: 12,
    });
    await showing;

    expect(documentText(editor)).toBe("current window");
    expect(internals(editor).startByte).toBe(0);
  });

  it("fails closed when a search byte offset is not an exact JavaScript integer", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "visible"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    await expect(editor.showMatch(Number.MAX_SAFE_INTEGER + 1, 1, "x", false, true)).rejects.toThrow(
      "exact integer range",
    );
    expect(fileService.GetMatchWindow).not.toHaveBeenCalled();
  });

  it("positions an exact zero-length regex result without searching for another occurrence", async () => {
    const text = "a😀b";
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, text));
    fileService.GetMatchWindow.mockResolvedValueOnce({
      window: windowData("file-a", 0, text),
      found: true,
      from: 3,
      to: 3,
    });
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a");

    await editor.showMatch(5, 0, "$", true, true);

    expect(internals(editor).view.state.selection.main.from).toBe(3);
    expect(internals(editor).view.state.selection.main.to).toBe(3);
  });

  it("rejects a non-adjacent previous-window response without replacing visible text", async () => {
    fileService.GetWindow.mockResolvedValueOnce({
      ...windowData("file-a", 100, "current"),
      atBof: false,
    });
    fileService.GetPrevWindow.mockResolvedValueOnce({
      ...windowData("file-a", 20, "wrong previous"),
      nextByte: 99,
      atBof: false,
      atEof: false,
    });
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);
    await editor.attach("file-a", "", "", 100);

    await expect(internals(editor).loadPrev()).rejects.toThrow("previous window is not adjacent");
    expect(documentText(editor)).toBe("current");
    expect(internals(editor).startByte).toBe(100);
  });

  it("surfaces a debounced flush failure through onError without clearing dirty state", async () => {
    const ed = await createEditingEditor();
    const failure = new Error("debounced stage failed");
    fileService.StageEdit.mockRejectedValueOnce(failure);

    vi.useFakeTimers();
    replaceDocument(ed, "changed by debounce");
    await vi.advanceTimersByTimeAsync(1200);

    expect(fileService.StageEdit).toHaveBeenCalledOnce();
    expect(cb.onError).toHaveBeenCalledOnce();
    expect(cb.onError).toHaveBeenCalledWith(failure);
    expect(internals(ed).dirty).toBe(true);
    expect(cb.onDirty).toHaveBeenLastCalledWith(true);
  });

  it("drains a newer local revision without clearing it when an earlier flush resolves", async () => {
    const ed = await createEditingEditor();
    const firstStage = deferred<StagingStateData>();
    fileService.StageEdit
      .mockImplementationOnce(() => firstStage.promise)
      .mockResolvedValueOnce(stagedState);

    replaceDocument(ed, "first version");
    const flush = ed.flush();
    await settleRequests();
    replaceDocument(ed, "newer version");

    firstStage.resolve(stagedState);
    await flush;

    expect(fileService.StageEdit).toHaveBeenCalledTimes(2);
    expect(fileService.StageEdit.mock.calls[1]).toEqual([
      "file-a",
      0,
      new TextEncoder().encode("first version").length,
      "newer version",
    ]);
    expect(documentText(ed)).toBe("newer version");
    expect(internals(ed).dirty).toBe(false);
    expect(cb.onDirty).toHaveBeenLastCalledWith(false);
  });

  it("ignores an older same-file response after a newer window and local edit", async () => {
    const ed = await createEditingEditor("file-a", "initial");
    const requestA = deferred<WindowData>();
    const requestB = deferred<WindowData>();
    fileService.GetEditWindow
      .mockImplementationOnce(() => requestA.promise)
      .mockImplementationOnce(() => requestB.promise);

    const loadA = ed.gotoByte(100);
    const loadB = ed.gotoByte(200);
    await settleRequests();

    requestB.resolve(windowData("file-a", 200, "window-b"));
    await loadB;
    replaceDocument(ed, "newer local edit");

    const statusAfterB = cb.onStatus.mock.calls.at(-1)?.[0];
    expect(statusAfterB?.startByte).toBe(200);
    expect(internals(ed).dirty).toBe(true);

    requestA.resolve(windowData("file-a", 100, "stale-window-a"));
    await loadA;

    expect(documentText(ed)).toBe("newer local edit");
    expect(internals(ed).startByte).toBe(200);
    expect(internals(ed).editWinStart).toBe(200);
    expect(internals(ed).editWinOrigLen).toBe(new TextEncoder().encode("window-b").length);
    expect(internals(ed).dirty).toBe(true);
    expect(cb.onDirty).toHaveBeenLastCalledWith(true);
    expect(cb.onStatus.mock.calls.at(-1)?.[0]).toBe(statusAfterB);

    fileService.StageEdit.mockResolvedValueOnce(stagedState);
    await ed.flush();
    expect(fileService.StageEdit).toHaveBeenCalledWith(
      "file-a",
      200,
      new TextEncoder().encode("window-b").length,
      "newer local edit",
    );
  });

  it("ignores an out-of-order response for a file that is no longer attached", async () => {
    const requestA = deferred<WindowData>();
    const requestB = deferred<WindowData>();
    fileService.GetWindow.mockImplementation((fileId: string) => {
      if (fileId === "file-a") return requestA.promise;
      if (fileId === "file-b") return requestB.promise;
      throw new Error(`unexpected file ${fileId}`);
    });

    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);

    const attachA = editor.attach("file-a");
    const attachB = editor.attach("file-b");
    await settleRequests();

    requestB.resolve(windowData("file-b", 800, "active-file-b"));
    await attachB;
    const statusAfterB = cb.onStatus.mock.calls.at(-1)?.[0];

    requestA.resolve(windowData("file-a", 100, "stale-file-a"));
    await attachA;

    expect(documentText(editor)).toBe("active-file-b");
    expect(internals(editor).startByte).toBe(800);
    expect(cb.onStatus.mock.calls.at(-1)?.[0]).toBe(statusAfterB);
  });

  it("rejects a current response carrying a different file identity", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-b", 0, "wrong file"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);

    await expect(editor.attach("file-a")).rejects.toThrow(
      "window response file mismatch: expected file-a, received file-b",
    );
    expect(documentText(editor)).toBe("");
    expect(cb.onStatus).not.toHaveBeenCalled();
  });

  it("keeps the previous identity paired with its text when a new attachment fails", async () => {
    fileService.GetWindow
      .mockResolvedValueOnce(windowData("file-a", 0, "visible file A"))
      .mockRejectedValueOnce(new Error("file B load failed"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);

    await editor.attach("file-a");
    expect(documentText(editor)).toBe("visible file A");

    await expect(editor.attach("file-b")).rejects.toThrow("file B load failed");
    expect(internals(editor).fileId).toBe("file-a");
    expect(documentText(editor)).toBe("visible file A");
    expect(cb.onStatus.mock.calls.at(-1)?.[0]?.startByte).toBe(0);
  });

  it("switches the active SQL syntax palette together with the editor theme", async () => {
    fileService.GetWindow.mockResolvedValueOnce(windowData("file-a", 0, "SELECT 42 FROM `orders`"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb, "dark");

    await editor.attach("file-a", "sql", "dump.sql");
    const keyword = host.querySelector<HTMLElement>(".q-sql-keyword");
    expect(keyword).not.toBeNull();
    expect(getComputedStyle(keyword!).color).toBe("rgb(86, 156, 214)");

    editor.setTheme("light");

    expect(host.querySelector(".q-sql-keyword")).toBe(keyword);
    expect(getComputedStyle(keyword!).color).toBe("rgb(23, 78, 166)");
  });

  it("applies a CSV separator only to the currently attached CSV file", async () => {
    fileService.GetWindow
      .mockResolvedValueOnce(windowData("file-a", 0, "a,b\n1,2"))
      .mockResolvedValueOnce(windowData("file-b", 0, "select 1;"));
    const host = document.querySelector<HTMLElement>("#editor");
    if (!host) throw new Error("missing editor test host");
    editor = new QuarryEditor(host, cb);

    await editor.attach("file-a", "csv", "first.csv", 0, ",");
    expect(editor.setCsvDelimiter("file-a", ";")).toBe(true);
    expect(editor.setCsvDelimiter("another-file", "\t")).toBe(false);
    expect(editor.setCsvDelimiter("file-a", "::")).toBe(false);

    await editor.attach("file-b", "sql", "dump.sql");
    expect(editor.setCsvDelimiter("file-b", ",")).toBe(false);
    expect(editor.setCsvDelimiter("file-a", ",")).toBe(false);
  });
});
