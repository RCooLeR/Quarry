// QuarryEditor wraps CodeMirror 6 as a *windowed* viewer/editor over a file that
// may be hundreds of GB. CodeMirror only ever holds one bounded window supplied
// by the Go FileService; it swaps to the adjacent window on scroll. Gutters show
// GLOBAL byte offsets and line numbers.
//
// Read-only mode uses GetWindow (robust display for any encoding). Edit mode uses
// GetEditWindow (byte-exact UTF-8 windows over the *edited* view) so changes are
// visible across window swaps; edits are flushed into the Go staging session as
// whole-window replacements on swap, on a debounce, and before save.

import { Compartment, EditorState, Extension, StateEffect, StateField, Transaction } from "@codemirror/state";
import {
  Decoration,
  DecorationSet,
  EditorView,
  lineNumbers,
  gutter,
  GutterMarker,
  ViewPlugin,
  ViewUpdate,
  drawSelection,
  highlightActiveLineGutter,
  keymap,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { FileService } from "../../bindings/github.com/quarry/quarry-wails3";
import { EDITOR_SYNTAX_BACKGROUNDS, highlightFor, syntaxThemeFor } from "./highlight";

export interface FileMetaData {
  fileId: string;
  path: string;
  size: number;
  encoding: string;
  encodingRequiresConfirmation?: boolean;
  detected: string;
  binary: boolean;
  editable: boolean;
  /** A refresh committed authoritative metadata but old-session cleanup warned. */
  refreshWarning?: string;
}

export interface WindowData {
  fileId: string;
  startByte: number;
  nextByte: number;
  text: string;
  lineOffsets: number[];
  lineEndOffsets?: number[];
  lineNumbers: number[];
  lineTruncated?: boolean[];
  startContinuesLine?: boolean;
  endContinuesLine?: boolean;
  sourceBytes?: number;
  budgetBytes?: number;
  atBof: boolean;
  atEof: boolean;
  approx: boolean;
}

export interface WindowStatus {
  fileId: string;
  startByte: number;
  /** Exact raw byte offset of the selected/first-visible source line. */
  positionByte: number;
  endByte: number;
  firstLine: number;
  lastLine: number;
  atBof: boolean;
  atEof: boolean;
  approx: boolean;
}

export interface StagingStateData {
  editCount: number;
  originalSize: number;
  editedSize: number;
  netDelta: number;
  lengthPreserving: boolean;
  inPlaceEligible: boolean;
}

export interface EditorCallbacks {
  onStatus: (s: WindowStatus | null) => void;
  onDirty: (dirty: boolean) => void;
  onStaging: (s: StagingStateData) => void;
  onMode: (editing: boolean) => void;
  /** Receives failures from editor-owned background work such as debounce flushes. */
  onError: (error: unknown) => void;
}

// How to number the rows of a freshly loaded window. Windows are contiguous and
// line-aligned, so exact numbers can be chained from a known anchor (byte 0 =
// line 1) as the user scrolls — far more reliable than the sparse line index,
// which interpolates badly across the multi-MB lines in real SQL dumps.
type Numbering =
  | { kind: "top"; first: number; exact: boolean } // first row is line `first`
  | { kind: "bottom"; last: number; exact: boolean } // last row is line `last`
  | { kind: "approx" }; // fall back to the server's interpolated numbers

const WINDOW_BYTES = 1 << 20;
const EDGE_THRESHOLD = 2000;
const FLUSH_DEBOUNCE_MS = 1200;
const TailWindowService = FileService as unknown as {
  GetTailWindow: (fileId: string, maxBytes: number) => Promise<WindowData>;
};
interface MatchWindowData {
  window: WindowData;
  found: boolean;
  from: number;
  to: number;
}
const MatchWindowService = FileService as unknown as {
  GetMatchWindow: (fileId: string, offset: number, length: number, maxBytes: number) => Promise<MatchWindowData>;
};
const MAX_DOC = 4 << 20;

const encoder = new TextEncoder();
const utf8Len = (s: string) => encoder.encode(s).length;

/** Convert one exact UTF-8 byte boundary to a JavaScript UTF-16 position. */
function utf8ByteToCodeUnit(text: string, targetByte: number): number | null {
  if (!Number.isSafeInteger(targetByte) || targetByte < 0) return null;
  let bytes = 0;
  let codeUnits = 0;
  for (const decoded of text) {
    if (bytes === targetByte) return codeUnits;
    bytes += encoder.encode(decoded).length;
    codeUnits += decoded.length;
    if (bytes > targetByte) return null;
  }
  return bytes === targetByte ? codeUnits : null;
}

interface WinRef {
  lineOffsets: number[];
  lineNumbers: number[];
}

interface WindowRequest {
  fileId: string;
  generation: number;
  editing: boolean;
}

class ByteMarker extends GutterMarker {
  constructor(private readonly label: string) {
    super();
  }
  toDOM(): Node {
    return document.createTextNode(this.label);
  }
}

const setMatch = StateEffect.define<{ from: number; to: number } | null>();
const matchMark = Decoration.mark({ class: "cm-search-match" });
const matchField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(deco, tr) {
    deco = deco.map(tr.changes);
    for (const e of tr.effects) {
      if (e.is(setMatch)) {
        deco = e.value ? Decoration.set([matchMark.range(e.value.from, e.value.to)]) : Decoration.none;
      }
    }
    return deco;
  },
  provide: (f) => EditorView.decorations.from(f),
});

const quarryTheme = EditorView.theme(
  {
    "&": { height: "100%", fontSize: "13px", color: "#cdd6e4", backgroundColor: EDITOR_SYNTAX_BACKGROUNDS.dark },
    ".cm-scroller": { fontFamily: "'JetBrains Mono','Cascadia Code',Consolas,monospace", lineHeight: "1.5" },
    ".cm-gutters": { backgroundColor: "#0d1117", color: "#56627a", border: "none" },
    ".cm-byteGutter": { padding: "0 10px", color: "#4b87d6", fontVariantNumeric: "tabular-nums" },
    ".cm-lineNumbers .cm-gutterElement": { padding: "0 8px", minWidth: "3ch" },
    ".cm-activeLineGutter": { backgroundColor: "#161c26" },
    ".cm-search-match": {
      backgroundColor: "rgba(43,182,196,0.32)",
      outline: "1px solid rgba(63,204,218,0.8)",
      borderRadius: "2px",
    },
    "&.cm-editing .cm-content": { caretColor: "#3fccda" },
  },
  { dark: true },
);

const quarryThemeLight = EditorView.theme(
  {
    "&": { height: "100%", fontSize: "13px", color: "#1d2530", backgroundColor: EDITOR_SYNTAX_BACKGROUNDS.light },
    ".cm-scroller": { fontFamily: "'JetBrains Mono','Cascadia Code',Consolas,monospace", lineHeight: "1.5" },
    ".cm-gutters": { backgroundColor: "#f0f3f7", color: "#8893a5", border: "none" },
    ".cm-byteGutter": { padding: "0 10px", color: "#2f6fd0", fontVariantNumeric: "tabular-nums" },
    ".cm-lineNumbers .cm-gutterElement": { padding: "0 8px", minWidth: "3ch" },
    ".cm-activeLineGutter": { backgroundColor: "#e6ebf2" },
    ".cm-search-match": {
      backgroundColor: "rgba(43,182,196,0.28)",
      outline: "1px solid rgba(31,153,166,0.8)",
      borderRadius: "2px",
    },
    "&.cm-editing .cm-content": { caretColor: "#1f99a6" },
  },
  { dark: false },
);

export function editorThemeFor(theme: string) {
  return [theme === "light" ? quarryThemeLight : quarryTheme, syntaxThemeFor(theme)];
}

export class QuarryEditor {
  private view: EditorView;
  private cb: EditorCallbacks;
  private editableC = new Compartment();
  private langC = new Compartment();
  private themeC = new Compartment();
  private byteGutterC = new Compartment();
  private byteGutter!: Extension;

  private fileId = "";
  private detected = "";
  private path = "";
  private startByte = 0;
  private nextByte = 0;
  private atBof = true;
  private atEof = false;
  private busy = false;
  private applying = false;
  private ref: WinRef = { lineOffsets: [], lineNumbers: [] };

  // Exact line numbers of the current window, used to chain the next/prev one.
  private winFirstNum = 1;
  private winLastNum = 1;
  private winExact = false;

  private editMode = false;
  private dirty = false;
  private editWinStart = 0;
  private editWinOrigLen = 0;
  private flushTimer: number | null = null;
  private flushInFlight: Promise<void> | null = null;
  private editRevision = 0;
  private editContextGeneration = 0;
  private windowRequestGeneration = 0;

  constructor(parent: HTMLElement, cb: EditorCallbacks, theme: string = "dark", showByteOffsets: boolean = true) {
    this.cb = cb;
    const ref = this.ref;
    const self = this;

    const globalLineNumbers = lineNumbers({
      formatNumber: (n) => {
        const g = ref.lineNumbers[n - 1];
        return g != null ? String(g) : String(n);
      },
    });

    const byteGutter = gutter({
      class: "cm-byteGutter",
      lineMarker: (view, block) => {
        const lineNo = view.state.doc.lineAt(block.from).number;
        const off = ref.lineOffsets[lineNo - 1];
        if (off == null) return null;
        return new ByteMarker("0x" + off.toString(16));
      },
      lineMarkerChange: () => true,
    });
    this.byteGutter = byteGutter;

    const edgeWatcher = ViewPlugin.fromClass(
      class {
        update(u: ViewUpdate) {
          if (u.docChanged && self.editMode && !self.applying) {
            self.markDirty();
          }
          if (!self.applying && (u.viewportChanged || u.selectionSet)) {
            self.emitCurrentStatus();
          }
          if (!u.viewportChanged) return;
          const len = u.state.doc.length;
          const vp = u.view.viewport;
          if (vp.to > len - EDGE_THRESHOLD) {
            void self.loadNext().catch((error) => self.reportAsyncError("load next window", error));
          } else if (vp.from < EDGE_THRESHOLD) {
            void self.loadPrev().catch((error) => self.reportAsyncError("load previous window", error));
          }
        }
      },
    );

    const state = EditorState.create({
      doc: "",
      extensions: [
        globalLineNumbers,
        this.byteGutterC.of(showByteOffsets ? byteGutter : []),
        matchField,
        highlightActiveLineGutter(),
        drawSelection(),
        history(),
        EditorView.lineWrapping,
        keymap.of([...defaultKeymap, ...historyKeymap]),
        EditorState.changeFilter.of((tr) => tr.newDoc.length <= MAX_DOC),
        this.editableC.of([EditorState.readOnly.of(true), EditorView.editable.of(false)]),
        this.langC.of([]),
        edgeWatcher,
        this.themeC.of(editorThemeFor(theme)),
      ],
    });

    this.view = new EditorView({ state, parent });

    // Viewport-edge detection (above) fires a window swap as you *approach* an
    // edge, which keeps mid-window scrolling smooth. But when a swap lands you
    // exactly at the top (or bottom) of the new window there's nothing left to
    // scroll into, so no further scroll events fire and you'd be pinned. This
    // wheel handler covers that gap: wheeling against an already-pinned edge
    // pulls in the adjacent window. Each wheel event is single-direction, so it
    // can't oscillate.
    this.view.scrollDOM.addEventListener(
      "wheel",
      (e: WheelEvent) => {
        if (this.busy || !this.fileId) return;
        const el = this.view.scrollDOM;
        const atTop = el.scrollTop <= 0;
        const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 1;
        if (e.deltaY < 0 && atTop && !this.atBof) {
          void this.loadPrev().catch((error) => this.reportAsyncError("load previous window", error));
        } else if (e.deltaY > 0 && atBottom && !this.atEof) {
          void this.loadNext().catch((error) => this.reportAsyncError("load next window", error));
        }
      },
      { passive: true },
    );
  }

  // ---- public API ---------------------------------------------------------

  async openPath(path: string): Promise<FileMetaData> {
    const meta = (await FileService.OpenFile(path)) as FileMetaData;
    await this.attach(meta.fileId, meta.detected, meta.path, 0);
    return meta;
  }

  /** Switch to an already-open file (flushing the current one first). */
  async attach(fileId: string, detected = "", path = "", startByte = 0, csvDelimiter = ""): Promise<void> {
    await this.flush();
    this.setMode(false); // a freshly activated file starts read-only
    const numbering: Numbering =
      startByte === 0 ? { kind: "top", first: 1, exact: true } : { kind: "approx" };
    const request: WindowRequest = {
      fileId,
      generation: ++this.windowRequestGeneration,
      editing: false,
    };
    this.busy = true;
    try {
      const w = await this.fetchWindow(request, startByte);
      // Attachment is transactional: keep the previous file identity and bytes
      // paired until the new response is both current and identity-checked.
      if (request.generation !== this.windowRequestGeneration || this.editMode) return;
      if (w.fileId !== fileId) {
        throw new Error(`window response file mismatch: expected ${fileId}, received ${w.fileId}`);
      }
      this.setLanguage(detected, path, csvDelimiter);
      this.fileId = fileId;
      this.detected = detected;
      this.path = path;
      this.apply(request, w, "top", numbering);
    } catch (error) {
      if (request.generation === this.windowRequestGeneration) this.busy = false;
      throw error;
    }
  }

  clear(): void {
    this.cancelFlushTimer();
    this.invalidateWindowRequests();
    this.editRevision++;
    this.editContextGeneration++;
    const wasDirty = this.dirty;
    this.fileId = "";
    this.detected = "";
    this.path = "";
    // clear() is also the final safety barrier after a discarded-session reload
    // fails. Reconfigure CodeMirror itself instead of only changing the model
    // flag, otherwise stale text can remain contenteditable after detachment.
    this.setMode(false, false);
    this.dirty = false;
    if (wasDirty) this.cb.onDirty(false);
    this.busy = true;
    this.applying = true;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: "" },
      effects: [setMatch.of(null)],
      annotations: Transaction.addToHistory.of(false),
    });
    this.applying = false;
    this.ref.lineOffsets = [];
    this.ref.lineNumbers = [];
    this.cb.onStatus(null);
    this.busy = false;
  }

  /** Turn editing on/off for the current file, reloading the window. */
  async setEditMode(on: boolean): Promise<void> {
    if (on === this.editMode) return;
    const fileId = this.fileId;
    if (!fileId) return;

    if (on) {
      // Enabling edit mode is transactional. Keep CodeMirror read-only while
      // the backend proves that this exact file/window is editable; otherwise
      // a rejected GetEditWindow would leave raw or synthetic viewer text
      // locally editable without a valid staging contract.
      const request: WindowRequest = {
        fileId,
        generation: ++this.windowRequestGeneration,
        editing: true,
      };
      this.busy = true;
      let committed = false;
      try {
        const w = await this.fetchWindow(request, this.startByte);
        if (
          request.generation !== this.windowRequestGeneration
          || this.fileId !== fileId
          || this.editMode
        ) return;
        if (w.fileId !== fileId) {
          throw new Error(`window response file mismatch: expected ${fileId}, received ${w.fileId}`);
        }

        // No browser event can interleave between this synchronous mode flip
        // and apply. If apply rejects its validated response, the catch below
        // immediately restores read-only mode before yielding control.
        this.setMode(true, false);
        try {
          committed = this.apply(request, w, "top", { kind: "approx" });
        } catch (error) {
          this.setMode(false);
          throw error;
        }
        if (!committed) this.setMode(false);
        return;
      } finally {
        if (
          !committed
          && request.generation === this.windowRequestGeneration
          && this.fileId === fileId
          && !this.editMode
        ) {
          this.busy = false;
        }
      }
    }

    await this.flush();
    if (this.fileId !== fileId) return;
    this.setMode(false);
    await this.loadAt(fileId, this.startByte, "top", { kind: "approx" });
  }

  /** Flush the current window's pending edits into the Go staging session. */
  flush(): Promise<void> {
    this.cancelFlushTimer();
    if (this.flushInFlight) return this.flushInFlight;
    if (!this.editMode || !this.dirty || !this.fileId) return Promise.resolve();

    const operation = this.flushDirtyWindow();
    let tracked: Promise<void>;
    tracked = operation.finally(() => {
      if (this.flushInFlight === tracked) this.flushInFlight = null;
    });
    this.flushInFlight = tracked;
    return tracked;
  }

  /** Reload the current window (e.g. after a save changed the file on disk). */
  async refresh(): Promise<void> {
    const fileId = this.fileId;
    if (!fileId) return;
    await this.flush();
    if (this.fileId !== fileId) return;
    // Keep the numbering for this window while reloading its current view.
    const numbering: Numbering = this.editMode
      ? { kind: "approx" }
      : { kind: "top", first: this.winFirstNum, exact: this.winExact };
    await this.loadAt(fileId, this.startByte, "top", numbering);
  }

  async gotoByte(byte: number): Promise<void> {
    const fileId = this.fileId;
    if (!fileId) return;
    await this.flush();
    if (this.fileId !== fileId) return;
    await this.loadAt(fileId, Math.max(0, byte), "top", { kind: "approx" });
  }

  async gotoEnd(): Promise<void> {
    const fileId = this.fileId;
    if (!fileId) return;
    await this.flush();
    if (this.fileId !== fileId) return;
    const request = this.beginWindowRequest(fileId);
    try {
      const w = await TailWindowService.GetTailWindow(fileId, WINDOW_BYTES);
      this.apply(request, w, "bottom", { kind: "approx" });
    } finally {
      this.finishWindowRequest(request);
    }
  }

  /**
   * Rebind renderer configuration after RefreshFile replaces this file's
   * backend generation, then load the authoritative tail. The request is
   * invalidated before the first await so a pre-refresh scroll response cannot
   * commit afterward. Metadata is committed only with a validated new window.
   */
  async refreshSource(meta: FileMetaData, csvDelimiter = ""): Promise<void> {
    const fileId = this.fileId;
    if (!fileId || meta.fileId !== fileId) {
      throw new Error(`cannot refresh editor metadata for unattached file ${meta.fileId || "<empty>"}`);
    }
    if (this.editMode || this.dirty) {
      throw new Error("cannot refresh the source while the editor owns pending edits");
    }

    this.cancelFlushTimer();
    this.invalidateWindowRequests();
    const request = this.beginWindowRequest(fileId);
    try {
      const w = await TailWindowService.GetTailWindow(fileId, WINDOW_BYTES);
      if (!this.isWindowRequestCurrent(request)) return;
      if (w.fileId !== fileId) {
        throw new Error(`window response file mismatch: expected ${fileId}, received ${w.fileId}`);
      }
      // Apply validates and installs the authoritative window first. Only then
      // pair its renderer configuration with the refreshed metadata; a
      // malformed window must leave the previous document configuration whole.
      if (!this.apply(request, w, "bottom", { kind: "approx" })) return;
      this.setLanguage(meta.detected, meta.path, csvDelimiter);
      this.detected = meta.detected;
      this.path = meta.path;
    } finally {
      this.finishWindowRequest(request);
    }
  }

  async showMatch(
    offset: number,
    length: number,
    _query: string,
    _regex: boolean,
    _caseSensitive: boolean,
    isCurrent: () => boolean = () => true,
  ): Promise<void> {
    const fileId = this.fileId;
    if (!fileId) return;
    if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(length) || offset < 0 || length < 0) {
      throw new Error("search result exceeds JavaScript's exact integer range");
    }
    await this.flush();
    if (this.fileId !== fileId || !isCurrent()) return;

    // Edit windows are UTF-8-only and may include staged state that is not in
    // the immutable source document. Map their exact relative UTF-8 byte range
    // locally; never search for a repeated query as a positional fallback.
    if (this.editMode) {
      const applied = await this.loadAt(fileId, Math.max(0, offset - 256), "top", { kind: "approx" }, isCurrent);
      if (!applied) return;
      const text = this.view.state.doc.toString();
      const from = utf8ByteToCodeUnit(text, offset - this.startByte);
      const to = utf8ByteToCodeUnit(text, offset - this.startByte + length);
      if (from === null || to === null || to < from) return;
      this.view.dispatch({
        selection: { anchor: from, head: to },
        effects: [setMatch.of(to > from ? { from, to } : null), EditorView.scrollIntoView(from, { y: "center" })],
      });
      return;
    }

    const request = this.beginWindowRequest(fileId);
    try {
      const result = await MatchWindowService.GetMatchWindow(fileId, offset, length, WINDOW_BYTES);
      if (!isCurrent()) return;
      if (!this.apply(request, result.window, "top", { kind: "approx" })) return;
      const docLength = this.view.state.doc.length;
      if (!result.found || result.from < 0 || result.to < result.from || result.to > docLength) return;
      this.view.dispatch({
        selection: { anchor: result.from, head: result.to },
        effects: [
          setMatch.of(result.to > result.from ? { from: result.from, to: result.to } : null),
          EditorView.scrollIntoView(result.from, { y: "center" }),
        ],
      });
    } finally {
      this.finishWindowRequest(request);
    }
  }

  isEditing(): boolean {
    return this.editMode;
  }

  /** Apply a confirmed per-file CSV dialect without affecting another tab. */
  setCsvDelimiter(fileId: string, delimiter: string): boolean {
    if (!fileId || fileId !== this.fileId) return false;
    const type = this.detected.toLowerCase();
    if (type !== "csv" && type !== "tsv") return false;
    if (Array.from(delimiter).length !== 1 || delimiter === "\0" || delimiter === "\r" || delimiter === "\n") {
      return false;
    }
    this.setLanguage(this.detected, this.path, delimiter);
    return true;
  }

  /** Swap the syntax-highlighting extension to match the file type. */
  private setLanguage(detected: string, path: string, csvDelimiter = ""): void {
    this.view.dispatch({ effects: this.langC.reconfigure(highlightFor(detected, path, csvDelimiter)) });
  }

  /** Swap the editor color theme ("dark" | "light"). */
  setTheme(theme: string): void {
    this.view.dispatch({ effects: this.themeC.reconfigure(editorThemeFor(theme)) });
  }

  /** Show or hide the byte-offset gutter beside the line numbers. */
  setShowByteOffsets(on: boolean): void {
    this.view.dispatch({ effects: this.byteGutterC.reconfigure(on ? this.byteGutter : []) });
  }

  destroy(): void {
    this.cancelFlushTimer();
    this.invalidateWindowRequests();
    this.editRevision++;
    this.editContextGeneration++;
    this.view.destroy();
  }

  // ---- internals ----------------------------------------------------------

  private setMode(on: boolean, invalidateRequests = true): void {
    if (this.editMode === on) {
      this.view.dom.classList.toggle("cm-editing", on);
      return;
    }
    if (invalidateRequests) this.invalidateWindowRequests();
    this.editContextGeneration++;
    this.editMode = on;
    this.view.dispatch({
      effects: this.editableC.reconfigure([EditorState.readOnly.of(!on), EditorView.editable.of(on)]),
    });
    this.view.dom.classList.toggle("cm-editing", on);
    this.view.requestMeasure();
    this.cb.onMode(on);
  }

  private markDirty(): void {
    this.editRevision++;
    this.invalidateWindowRequests();
    if (!this.dirty) {
      this.dirty = true;
      this.cb.onDirty(true);
    }
    this.scheduleFlush();
  }

  private scheduleFlush(): void {
    this.cancelFlushTimer();
    this.flushTimer = window.setTimeout(() => {
      this.flushTimer = null;
      void this.flush().catch((error) => this.reportAsyncError("flush edits", error));
    }, FLUSH_DEBOUNCE_MS);
  }

  private cancelFlushTimer(): void {
    if (this.flushTimer != null) {
      window.clearTimeout(this.flushTimer);
      this.flushTimer = null;
    }
  }

  private async flushDirtyWindow(): Promise<void> {
    while (this.editMode && this.dirty && this.fileId) {
      const fileId = this.fileId;
      const startByte = this.editWinStart;
      const originalLength = this.editWinOrigLen;
      const revision = this.editRevision;
      const contextGeneration = this.editContextGeneration;
      const text = this.view.state.doc.toString();
      const st = (await FileService.StageEdit(fileId, startByte, originalLength, text)) as StagingStateData;

      // The call may have completed after clear/destroy or a different file/window
      // took ownership of the editor. The backend staged the captured snapshot,
      // but it must not mutate the state belonging to the newer editor context.
      if (
        this.fileId !== fileId ||
        !this.editMode ||
        this.editWinStart !== startByte ||
        this.editContextGeneration !== contextGeneration
      ) {
        return;
      }

      this.editWinOrigLen = utf8Len(text);
      this.cb.onStaging(st);
      if (this.editRevision === revision) {
        this.dirty = false;
        this.cb.onDirty(false);
      }
      this.view.requestMeasure(); // nudge WebView2 to repaint after the React update
    }
  }

  private beginWindowRequest(fileId: string): WindowRequest {
    const request: WindowRequest = {
      fileId,
      generation: ++this.windowRequestGeneration,
      editing: this.editMode,
    };
    this.busy = true;
    return request;
  }

  private invalidateWindowRequests(): void {
    this.windowRequestGeneration++;
    this.busy = false;
  }

  private isWindowRequestCurrent(request: WindowRequest): boolean {
    return (
      request.generation === this.windowRequestGeneration &&
      request.fileId === this.fileId &&
      request.editing === this.editMode
    );
  }

  private finishWindowRequest(request: WindowRequest): void {
    if (this.isWindowRequestCurrent(request)) this.busy = false;
  }

  private reportAsyncError(operation: string, error: unknown): void {
    console.error(operation, error);
    try {
      this.cb.onError(error);
    } catch (callbackError) {
      console.error("editor error callback", callbackError);
    }
  }

  private async fetchWindow(request: WindowRequest, startByte: number): Promise<WindowData> {
    if (request.editing) {
      return (await FileService.GetEditWindow(request.fileId, startByte, WINDOW_BYTES)) as WindowData;
    }
    return (await FileService.GetWindow(request.fileId, startByte, WINDOW_BYTES)) as WindowData;
  }

  private async loadAt(
    fileId: string,
    startByte: number,
    anchor: "top" | "bottom",
    numbering: Numbering,
    isCurrent: () => boolean = () => true,
  ): Promise<boolean> {
    const request = this.beginWindowRequest(fileId);
    try {
      const w = await this.fetchWindow(request, startByte);
      if (!isCurrent()) {
        this.finishWindowRequest(request);
        return false;
      }
      return this.apply(request, w, anchor, numbering);
    } catch (error) {
      this.finishWindowRequest(request);
      throw error;
    }
  }

  private async loadNext(): Promise<void> {
    if (this.busy || this.atEof || !this.fileId) return;
    const fileId = this.fileId;
    const request = this.beginWindowRequest(fileId);
    try {
      await this.flush();
      if (!this.isWindowRequestCurrent(request)) return;
      const w = request.editing
        ? ((await FileService.GetEditWindow(request.fileId, this.nextByte, WINDOW_BYTES)) as WindowData)
        : ((await FileService.GetNextWindow(request.fileId, this.nextByte, WINDOW_BYTES)) as WindowData);
      // The next window starts on the line after this one's last (windows are
      // contiguous and line-aligned), so exact numbers continue seamlessly.
      const numbering: Numbering = request.editing || w.startContinuesLine
        ? { kind: "approx" }
        : { kind: "top", first: this.winLastNum + 1, exact: this.winExact };
      this.apply(request, w, "top", numbering);
    } finally {
      this.finishWindowRequest(request);
    }
  }

  private async loadPrev(): Promise<void> {
    if (this.busy || this.atBof || !this.fileId) return;
    const fileId = this.fileId;
    const request = this.beginWindowRequest(fileId);
    try {
      await this.flush();
      if (!this.isWindowRequestCurrent(request)) return;
      const expectedNextByte = this.startByte;
      const w = request.editing
        ? ((await FileService.GetEditWindow(request.fileId, Math.max(0, this.startByte - WINDOW_BYTES), WINDOW_BYTES)) as WindowData)
        : ((await FileService.GetPrevWindow(request.fileId, this.startByte, WINDOW_BYTES)) as WindowData);
      if (!request.editing && this.isWindowRequestCurrent(request) && w.nextByte !== expectedNextByte) {
        throw new Error(`previous window is not adjacent: expected next byte ${expectedNextByte}, received ${w.nextByte}`);
      }
      // The previous window ends on the line before this one's first.
      const numbering: Numbering = request.editing || w.endContinuesLine
        ? { kind: "approx" }
        : { kind: "bottom", last: this.winFirstNum - 1, exact: this.winExact };
      this.apply(request, w, "bottom", numbering);
    } finally {
      this.finishWindowRequest(request);
    }
  }

  private apply(
    request: WindowRequest,
    w: WindowData,
    anchor: "top" | "bottom",
    numbering: Numbering,
  ): boolean {
    if (!this.isWindowRequestCurrent(request)) return false;
    if (w.fileId !== request.fileId) {
      throw new Error(`window response file mismatch: expected ${request.fileId}, received ${w.fileId}`);
    }
    if (w.lineOffsets.length !== w.lineNumbers.length) {
      throw new Error("window response line offset/number counts do not match");
    }

    this.startByte = w.startByte;
    this.nextByte = w.nextByte;
    this.atBof = w.atBof;
    this.atEof = w.atEof;

    const rowCount = w.lineNumbers.length;
    let numbers = w.lineNumbers;
    let exact = false;
    if (numbering.kind === "approx" || this.editMode) {
      numbers = w.lineNumbers;
      exact = !w.approx;
    } else if (numbering.kind === "top") {
      numbers = Array.from({ length: rowCount }, (_, i) => numbering.first + i);
      exact = numbering.exact;
    } else {
      const first = numbering.last - (rowCount - 1);
      numbers = Array.from({ length: rowCount }, (_, i) => first + i);
      exact = numbering.exact;
    }

    this.ref.lineOffsets = w.lineOffsets;
    this.ref.lineNumbers = numbers;
    this.winFirstNum = numbers[0] ?? 0;
    this.winLastNum = numbers[rowCount - 1] ?? 0;
    this.winExact = exact;

    this.editWinStart = w.startByte;
    this.editWinOrigLen = utf8Len(w.text);
    this.dirty = false;
    this.editRevision++;
    this.editContextGeneration++;

    const doc = w.text;
    const pos = anchor === "bottom" ? doc.length : 0;
    this.applying = true;
    try {
      this.view.dispatch({
        changes: { from: 0, to: this.view.state.doc.length, insert: doc },
        selection: { anchor: pos },
        effects: [setMatch.of(null), EditorView.scrollIntoView(pos, { y: anchor === "bottom" ? "end" : "start" })],
        annotations: Transaction.addToHistory.of(false),
      });
    } finally {
      this.applying = false;
    }

    this.emitCurrentStatus();
    this.busy = false;
    return true;
  }

  private rawLineStartAt(docPosition: number): number {
    const docLength = this.view.state.doc.length;
    const bounded = Math.max(0, Math.min(docLength, docPosition));
    const line = this.view.state.doc.lineAt(bounded).number;
    const raw = this.ref.lineOffsets[line - 1];
    return Number.isSafeInteger(raw) && raw >= 0 ? raw : this.startByte;
  }

  private currentPositionByte(): number {
    const viewport = this.view.viewport;
    const selection = this.view.state.selection.main;
    const selectedPosition = selection.head >= viewport.from && selection.head <= viewport.to
      ? selection.head
      : viewport.from;
    return this.rawLineStartAt(selectedPosition);
  }

  private emitCurrentStatus(): void {
    if (!this.fileId) return;
    this.cb.onStatus({
      fileId: this.fileId,
      startByte: this.startByte,
      positionByte: this.currentPositionByte(),
      endByte: this.nextByte,
      firstLine: this.winFirstNum,
      lastLine: this.winLastNum,
      atBof: this.atBof,
      atEof: this.atEof,
      approx: !this.winExact,
    });
  }
}
