// QuarryEditor wraps CodeMirror 6 as a *windowed* viewer/editor over a file that
// may be hundreds of GB. CodeMirror only ever holds one bounded window supplied
// by the Go FileService; it swaps to the adjacent window on scroll. Gutters show
// GLOBAL byte offsets and line numbers.
//
// Read-only mode uses GetWindow (robust display for any encoding). Edit mode uses
// GetEditWindow (byte-exact UTF-8 windows over the *edited* view) so changes are
// visible across window swaps; edits are flushed into the Go staging session as
// whole-window replacements on swap, on a debounce, and before save.

import { Compartment, EditorState, StateEffect, StateField, Transaction } from "@codemirror/state";
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
import { highlightFor } from "./highlight";

export interface FileMetaData {
  fileId: string;
  path: string;
  size: number;
  encoding: string;
  detected: string;
  binary: boolean;
  editable: boolean;
}

export interface WindowData {
  fileId: string;
  startByte: number;
  nextByte: number;
  text: string;
  lineOffsets: number[];
  lineNumbers: number[];
  atBof: boolean;
  atEof: boolean;
  approx: boolean;
}

export interface WindowStatus {
  startByte: number;
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
const MAX_DOC = 4 << 20;

const encoder = new TextEncoder();
const utf8Len = (s: string) => encoder.encode(s).length;

interface WinRef {
  lineOffsets: number[];
  lineNumbers: number[];
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
    "&": { height: "100%", fontSize: "13px", color: "#cdd6e4", backgroundColor: "#12161c" },
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

export class QuarryEditor {
  private view: EditorView;
  private cb: EditorCallbacks;
  private editableC = new Compartment();
  private langC = new Compartment();

  private fileId = "";
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

  constructor(parent: HTMLElement, cb: EditorCallbacks) {
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

    const edgeWatcher = ViewPlugin.fromClass(
      class {
        update(u: ViewUpdate) {
          if (u.docChanged && self.editMode && !self.applying) {
            self.markDirty();
          }
          if (!u.viewportChanged) return;
          const len = u.state.doc.length;
          const vp = u.view.viewport;
          if (vp.to > len - EDGE_THRESHOLD) void self.loadNext();
          else if (vp.from < EDGE_THRESHOLD) void self.loadPrev();
        }
      },
    );

    const state = EditorState.create({
      doc: "",
      extensions: [
        globalLineNumbers,
        byteGutter,
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
        quarryTheme,
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
        if (e.deltaY < 0 && atTop && !this.atBof) void this.loadPrev();
        else if (e.deltaY > 0 && atBottom && !this.atEof) void this.loadNext();
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
  async attach(fileId: string, detected = "", path = "", startByte = 0): Promise<void> {
    await this.flush();
    this.setMode(false); // a freshly activated file starts read-only
    this.setLanguage(detected, path);
    this.busy = true;
    this.fileId = fileId;
    this.startByte = startByte;
    this.nextByte = startByte;
    this.atBof = startByte === 0;
    this.atEof = false;
    const w = (await FileService.GetWindow(fileId, startByte, WINDOW_BYTES)) as WindowData;
    const numbering: Numbering =
      startByte === 0 ? { kind: "top", first: 1, exact: true } : { kind: "approx" };
    this.apply(w, "top", numbering);
  }

  clear(): void {
    this.cancelFlushTimer();
    this.fileId = "";
    this.editMode = false;
    this.dirty = false;
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
    if (!on) await this.flush();
    this.setMode(on);
    if (!this.fileId) return;
    this.busy = true;
    await this.loadAt(this.startByte, "top", { kind: "approx" });
  }

  /** Flush the current window's pending edits into the Go staging session. */
  async flush(): Promise<void> {
    this.cancelFlushTimer();
    if (!this.editMode || !this.dirty || !this.fileId) return;
    const text = this.view.state.doc.toString();
    try {
      const st = (await FileService.StageEdit(this.fileId, this.editWinStart, this.editWinOrigLen, text)) as StagingStateData;
      this.editWinOrigLen = utf8Len(text);
      this.dirty = false;
      this.cb.onDirty(false);
      this.cb.onStaging(st);
      this.view.requestMeasure(); // nudge WebView2 to repaint after the React update
    } catch (e) {
      console.error("flush", e);
    }
  }

  /** Reload the current window (e.g. after a save changed the file on disk). */
  async refresh(): Promise<void> {
    if (!this.fileId) return;
    this.busy = true;
    // Same window, same content (in-place patch is length-preserving) — keep the
    // numbering we already had.
    const numbering: Numbering = this.editMode
      ? { kind: "approx" }
      : { kind: "top", first: this.winFirstNum, exact: this.winExact };
    await this.loadAt(this.startByte, "top", numbering);
  }

  async gotoByte(byte: number): Promise<void> {
    if (!this.fileId) return;
    await this.flush();
    this.busy = true;
    await this.loadAt(Math.max(0, byte), "top", { kind: "approx" });
  }

  async showMatch(offset: number, length: number, query: string, regex: boolean, caseSensitive: boolean): Promise<void> {
    if (!this.fileId) return;
    await this.flush();
    this.busy = true;
    await this.loadAt(Math.max(0, offset - 256), "top", { kind: "approx" });

    const text = this.view.state.doc.toString();
    const hint = Math.max(0, offset - this.startByte - 8);
    let from = -1;
    let len = length;
    if (regex) {
      try {
        const re = new RegExp(query, caseSensitive ? "g" : "gi");
        re.lastIndex = hint;
        const m = re.exec(text);
        if (m) {
          from = m.index;
          len = m[0].length || length;
        }
      } catch {
        /* invalid client regex */
      }
    } else {
      const hay = caseSensitive ? text : text.toLowerCase();
      const needle = caseSensitive ? query : query.toLowerCase();
      from = hay.indexOf(needle, hint);
      if (from < 0) from = hay.indexOf(needle);
      len = query.length;
    }
    if (from >= 0) {
      const to = Math.min(text.length, from + len);
      this.view.dispatch({
        selection: { anchor: from, head: to },
        effects: [setMatch.of({ from, to }), EditorView.scrollIntoView(from, { y: "center" })],
      });
    }
  }

  isEditing(): boolean {
    return this.editMode;
  }

  /** Swap the syntax-highlighting extension to match the file type. */
  private setLanguage(detected: string, path: string): void {
    this.view.dispatch({ effects: this.langC.reconfigure(highlightFor(detected, path)) });
  }

  destroy(): void {
    this.cancelFlushTimer();
    this.view.destroy();
  }

  // ---- internals ----------------------------------------------------------

  private setMode(on: boolean): void {
    if (this.editMode === on) {
      this.view.dom.classList.toggle("cm-editing", on);
      return;
    }
    this.editMode = on;
    this.view.dispatch({
      effects: this.editableC.reconfigure([EditorState.readOnly.of(!on), EditorView.editable.of(on)]),
    });
    this.view.dom.classList.toggle("cm-editing", on);
    this.view.requestMeasure();
    this.cb.onMode(on);
  }

  private markDirty(): void {
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
      void this.flush();
    }, FLUSH_DEBOUNCE_MS);
  }

  private cancelFlushTimer(): void {
    if (this.flushTimer != null) {
      window.clearTimeout(this.flushTimer);
      this.flushTimer = null;
    }
  }

  private async fetchWindow(startByte: number): Promise<WindowData> {
    if (this.editMode) {
      return (await FileService.GetEditWindow(this.fileId, startByte, WINDOW_BYTES)) as WindowData;
    }
    return (await FileService.GetWindow(this.fileId, startByte, WINDOW_BYTES)) as WindowData;
  }

  private async loadAt(startByte: number, anchor: "top" | "bottom", numbering: Numbering): Promise<void> {
    const w = await this.fetchWindow(startByte);
    this.apply(w, anchor, numbering);
  }

  private async loadNext(): Promise<void> {
    if (this.busy || this.atEof || !this.fileId) return;
    this.busy = true;
    await this.flush();
    try {
      const w = this.editMode
        ? ((await FileService.GetEditWindow(this.fileId, this.nextByte, WINDOW_BYTES)) as WindowData)
        : ((await FileService.GetNextWindow(this.fileId, this.nextByte, WINDOW_BYTES)) as WindowData);
      // The next window starts on the line after this one's last (windows are
      // contiguous and line-aligned), so exact numbers continue seamlessly.
      const numbering: Numbering = this.editMode
        ? { kind: "approx" }
        : { kind: "top", first: this.winLastNum + 1, exact: this.winExact };
      this.apply(w, "top", numbering);
    } catch (e) {
      console.error("loadNext", e);
      this.busy = false;
    }
  }

  private async loadPrev(): Promise<void> {
    if (this.busy || this.atBof || !this.fileId) return;
    this.busy = true;
    await this.flush();
    try {
      const w = this.editMode
        ? ((await FileService.GetEditWindow(this.fileId, Math.max(0, this.startByte - WINDOW_BYTES), WINDOW_BYTES)) as WindowData)
        : ((await FileService.GetPrevWindow(this.fileId, this.startByte, WINDOW_BYTES)) as WindowData);
      // The previous window ends on the line before this one's first.
      const numbering: Numbering = this.editMode
        ? { kind: "approx" }
        : { kind: "bottom", last: this.winFirstNum - 1, exact: this.winExact };
      this.apply(w, "bottom", numbering);
    } catch (e) {
      console.error("loadPrev", e);
      this.busy = false;
    }
  }

  private apply(w: WindowData, anchor: "top" | "bottom", numbering: Numbering): void {
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

    const doc = w.text;
    const pos = anchor === "bottom" ? doc.length : 0;
    this.applying = true;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: doc },
      selection: { anchor: pos },
      effects: [setMatch.of(null), EditorView.scrollIntoView(pos, { y: anchor === "bottom" ? "end" : "start" })],
      annotations: Transaction.addToHistory.of(false),
    });
    this.applying = false;

    this.emitStatus(w, numbers, exact);
    setTimeout(() => {
      this.busy = false;
    }, 80);
  }

  private emitStatus(w: WindowData, numbers: number[], exact: boolean): void {
    this.cb.onStatus({
      startByte: w.startByte,
      endByte: w.nextByte,
      firstLine: numbers[0] ?? 0,
      lastLine: numbers[numbers.length - 1] ?? 0,
      atBof: w.atBof,
      atEof: w.atEof,
      approx: !exact,
    });
  }
}
