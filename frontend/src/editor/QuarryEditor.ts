// QuarryEditor wraps CodeMirror 6 as a *windowed* viewer over a file that may be
// hundreds of GB. CodeMirror never holds the whole file: it holds one bounded
// window (~1 MiB of decoded text) supplied by the Go FileService, and swaps to
// the adjacent window when the user scrolls near an edge. Gutters show GLOBAL
// byte offsets and line numbers, not the window-local 1..N.
//
// Phase 0 is read-only: this proves the windowing substrate (open is instant,
// memory is bounded, scroll loads the next/prev window atomically). Editing,
// diff, and save come in later phases.

import { EditorState, Transaction } from "@codemirror/state";
import {
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
import { defaultKeymap } from "@codemirror/commands";
import { FileService } from "../../bindings/github.com/quarry/quarry-wails3";

// Mirrors the Go DTOs (kept local so we don't depend on generated model paths).
export interface FileMetaData {
  fileId: string;
  path: string;
  size: number;
  encoding: string;
  detected: string;
  binary: boolean;
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

const WINDOW_BYTES = 1 << 20; // 1 MiB window budget
const EDGE_THRESHOLD = 2000; // chars from a doc edge that trigger a window load

// Per-window coordinate arrays the gutters read. A shared mutable ref so the
// gutter closures always see the current window after a swap.
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

const quarryTheme = EditorView.theme(
  {
    "&": { height: "100%", fontSize: "13px", color: "#cdd6e4", backgroundColor: "#12161c" },
    ".cm-scroller": { fontFamily: "'JetBrains Mono','Cascadia Code',Consolas,monospace", lineHeight: "1.5" },
    ".cm-gutters": { backgroundColor: "#0d1117", color: "#56627a", border: "none" },
    ".cm-byteGutter": { padding: "0 10px", color: "#4b87d6", fontVariantNumeric: "tabular-nums" },
    ".cm-lineNumbers .cm-gutterElement": { padding: "0 8px", minWidth: "3ch" },
    ".cm-activeLineGutter": { backgroundColor: "#161c26" },
  },
  { dark: true },
);

export class QuarryEditor {
  private view: EditorView;
  private onStatus: (s: WindowStatus | null) => void;

  private fileId = "";
  private startByte = 0;
  private nextByte = 0;
  private atBof = true;
  private atEof = false;
  private busy = false;
  private ref: WinRef = { lineOffsets: [], lineNumbers: [] };

  constructor(parent: HTMLElement, onStatus: (s: WindowStatus | null) => void) {
    this.onStatus = onStatus;
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
        highlightActiveLineGutter(),
        drawSelection(),
        EditorView.lineWrapping,
        keymap.of(defaultKeymap),
        EditorState.readOnly.of(true),
        EditorView.editable.of(false),
        edgeWatcher,
        quarryTheme,
      ],
    });

    this.view = new EditorView({ state, parent });
  }

  /** Open a path in Go, then show its first window. */
  async openPath(path: string): Promise<FileMetaData> {
    const meta = (await FileService.OpenFile(path)) as FileMetaData;
    await this.attach(meta.fileId, 0);
    return meta;
  }

  /** Point the editor at an already-open file id and load a window at startByte. */
  async attach(fileId: string, startByte = 0): Promise<void> {
    this.busy = true; // suppress edge-watcher during the swap
    this.fileId = fileId;
    this.startByte = startByte;
    this.nextByte = startByte;
    this.atBof = startByte === 0;
    this.atEof = false;
    const w = (await FileService.GetWindow(fileId, startByte, WINDOW_BYTES)) as WindowData;
    this.apply(w, "top");
  }

  /** Detach from any file and show an empty document. */
  clear(): void {
    this.fileId = "";
    this.busy = true;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: "" },
      annotations: Transaction.addToHistory.of(false),
    });
    this.ref.lineOffsets = [];
    this.ref.lineNumbers = [];
    this.onStatus(null);
    this.busy = false;
  }

  destroy(): void {
    this.view.destroy();
  }

  private async loadNext(): Promise<void> {
    if (this.busy || this.atEof || !this.fileId) return;
    this.busy = true;
    try {
      const w = (await FileService.GetNextWindow(this.fileId, this.nextByte, WINDOW_BYTES)) as WindowData;
      this.apply(w, "top");
    } catch (e) {
      console.error("loadNext", e);
      this.busy = false;
    }
  }

  private async loadPrev(): Promise<void> {
    if (this.busy || this.atBof || !this.fileId) return;
    this.busy = true;
    try {
      const w = (await FileService.GetPrevWindow(this.fileId, this.startByte, WINDOW_BYTES)) as WindowData;
      this.apply(w, "bottom");
    } catch (e) {
      console.error("loadPrev", e);
      this.busy = false;
    }
  }

  // apply swaps the whole document for the new window in ONE transaction
  // (changes + selection + scroll), which is the documented fix for the
  // CodeMirror flash-to-line-1 bug. addToHistory:false keeps swaps off any undo
  // stack. anchor decides which edge of the new window the viewport lands on so
  // forward/backward scrolling reads continuously.
  private apply(w: WindowData, anchor: "top" | "bottom"): void {
    this.startByte = w.startByte;
    this.nextByte = w.nextByte;
    this.atBof = w.atBof;
    this.atEof = w.atEof;
    this.ref.lineOffsets = w.lineOffsets;
    this.ref.lineNumbers = w.lineNumbers;

    const doc = w.text;
    const pos = anchor === "bottom" ? doc.length : 0;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: doc },
      selection: { anchor: pos },
      effects: EditorView.scrollIntoView(pos, { y: anchor === "bottom" ? "end" : "start" }),
      annotations: Transaction.addToHistory.of(false),
    });

    this.emitStatus(w);
    // Clear the busy flag after the swap-induced update settles, so the swap
    // itself can't immediately re-trigger an opposite-edge load.
    setTimeout(() => {
      this.busy = false;
    }, 80);
  }

  private emitStatus(w: WindowData): void {
    const nums = w.lineNumbers;
    this.onStatus({
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
