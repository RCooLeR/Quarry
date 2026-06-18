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

  private fileId = "";
  private startByte = 0;
  private nextByte = 0;
  private atBof = true;
  private atEof = false;
  private busy = false;
  private applying = false;
  private ref: WinRef = { lineOffsets: [], lineNumbers: [] };

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
        edgeWatcher,
        quarryTheme,
      ],
    });

    this.view = new EditorView({ state, parent });
  }

  // ---- public API ---------------------------------------------------------

  async openPath(path: string): Promise<FileMetaData> {
    const meta = (await FileService.OpenFile(path)) as FileMetaData;
    await this.attach(meta.fileId, 0);
    return meta;
  }

  /** Switch to an already-open file (flushing the current one first). */
  async attach(fileId: string, startByte = 0): Promise<void> {
    await this.flush();
    this.setMode(false); // a freshly activated file starts read-only
    this.busy = true;
    this.fileId = fileId;
    this.startByte = startByte;
    this.nextByte = startByte;
    this.atBof = startByte === 0;
    this.atEof = false;
    const w = (await FileService.GetWindow(fileId, startByte, WINDOW_BYTES)) as WindowData;
    this.apply(w, "top");
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
    await this.loadAt(this.startByte, "top");
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
    await this.loadAt(this.startByte, "top");
  }

  async gotoByte(byte: number): Promise<void> {
    if (!this.fileId) return;
    await this.flush();
    this.busy = true;
    await this.loadAt(Math.max(0, byte), "top");
  }

  async showMatch(offset: number, length: number, query: string, regex: boolean, caseSensitive: boolean): Promise<void> {
    if (!this.fileId) return;
    await this.flush();
    this.busy = true;
    await this.loadAt(Math.max(0, offset - 256), "top");

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

  private async loadAt(startByte: number, anchor: "top" | "bottom"): Promise<void> {
    const w = await this.fetchWindow(startByte);
    this.apply(w, anchor);
  }

  private async loadNext(): Promise<void> {
    if (this.busy || this.atEof || !this.fileId) return;
    this.busy = true;
    await this.flush();
    try {
      const w = this.editMode
        ? ((await FileService.GetEditWindow(this.fileId, this.nextByte, WINDOW_BYTES)) as WindowData)
        : ((await FileService.GetNextWindow(this.fileId, this.nextByte, WINDOW_BYTES)) as WindowData);
      this.apply(w, "top");
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
      this.apply(w, "bottom");
    } catch (e) {
      console.error("loadPrev", e);
      this.busy = false;
    }
  }

  private apply(w: WindowData, anchor: "top" | "bottom"): void {
    this.startByte = w.startByte;
    this.nextByte = w.nextByte;
    this.atBof = w.atBof;
    this.atEof = w.atEof;
    this.ref.lineOffsets = w.lineOffsets;
    this.ref.lineNumbers = w.lineNumbers;
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

    this.emitStatus(w);
    setTimeout(() => {
      this.busy = false;
    }, 80);
  }

  private emitStatus(w: WindowData): void {
    const nums = w.lineNumbers;
    this.cb.onStatus({
      startByte: w.startByte,
      endByte: w.nextByte,
      firstLine: nums[0] ?? 0,
      lastLine: nums[nums.length - 1] ?? 0,
      atBof: w.atBof,
      atEof: w.atEof,
      approx: w.approx,
    });
  }
}
