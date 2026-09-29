import { useEffect, useRef } from "react";
import { MergeView } from "@codemirror/merge";
import { EditorState } from "@codemirror/state";
import { EditorView, lineNumbers } from "@codemirror/view";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface DiffWindowData { startByte: number; nextByte: number; original: string; edited: string; atBof: boolean; atEof: boolean; }

const DIFF_BYTES = 64 * 1024;
export const MAX_DIFF_TEXT_CODE_UNITS = DIFF_BYTES;

/** Keep malformed bridge responses from constructing an unbounded MergeView. */
export function normalizeDiffWindow(value: unknown): DiffWindowData {
  if (typeof value !== "object" || value == null || Array.isArray(value)) {
    throw new Error("invalid diff-window response");
  }
  const payload = value as Record<string, unknown>;
  const offset = (field: "startByte" | "nextByte") => {
    const item = payload[field];
    if (typeof item !== "number" || !Number.isSafeInteger(item) || item < 0) {
      throw new Error(`invalid diff-window ${field}`);
    }
    return item;
  };
  const text = (field: "original" | "edited") => {
    const item = payload[field];
    if (typeof item !== "string" || item.length > MAX_DIFF_TEXT_CODE_UNITS) {
      throw new Error(`invalid or oversized diff-window ${field}`);
    }
    return item;
  };
  const startByte = offset("startByte");
  const nextByte = offset("nextByte");
  if (nextByte < startByte || typeof payload.atBof !== "boolean" || typeof payload.atEof !== "boolean") {
    throw new Error("invalid diff-window range or edge flags");
  }
  return {
    startByte,
    nextByte,
    original: text("original"),
    edited: text("edited"),
    atBof: payload.atBof,
    atEof: payload.atEof,
  };
}

const diffThemeDark = EditorView.theme(
  {
    "&": { fontSize: "12px", backgroundColor: "#0e1217", color: "#cdd6e4" },
    ".cm-scroller": { fontFamily: "'JetBrains Mono',Consolas,monospace" },
    ".cm-gutters": { backgroundColor: "#0b0f14", color: "#56627a", border: "none" },
  },
  { dark: true },
);

const diffThemeLight = EditorView.theme(
  {
    "&": { fontSize: "12px", backgroundColor: "#fbfcfe", color: "#1d2530" },
    ".cm-scroller": { fontFamily: "'JetBrains Mono',Consolas,monospace" },
    ".cm-gutters": { backgroundColor: "#eef1f6", color: "#66718a", border: "none" },
  },
  { dark: false },
);

interface Props {
  fileId: string;
  startByte: number;
  theme: string;
  onError?: (message: string) => void;
}

export default function DiffView({ fileId, startByte, theme, onError }: Props) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let mv: MergeView | null = null;
    let cancelled = false;
    (async () => {
      try {
        const w = normalizeDiffWindow(await FileService.GetDiffWindow(fileId, startByte, DIFF_BYTES));
        if (cancelled || !ref.current) return;
        const ext = [EditorState.readOnly.of(true), EditorView.editable.of(false), lineNumbers(), EditorView.lineWrapping, theme === "light" ? diffThemeLight : diffThemeDark];
        mv = new MergeView({
          parent: ref.current,
          a: { doc: w.original, extensions: ext },
          b: { doc: w.edited, extensions: ext },
          gutter: true,
          highlightChanges: true,
          collapseUnchanged: { margin: 3, minSize: 6 },
        });
      } catch (e: any) {
        if (!cancelled && ref.current) {
          const message = String(e?.message ?? e).slice(0, 500);
          ref.current.textContent = `Diff unavailable: ${message}`;
          onError?.(message);
        }
      }
    })();
    return () => {
      cancelled = true;
      if (mv) mv.destroy();
      if (ref.current) ref.current.replaceChildren();
    };
  }, [fileId, startByte, theme, onError]);

  return <div className="q-merge" ref={ref} />;
}
