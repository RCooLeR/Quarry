import { useEffect, useRef } from "react";
import { MergeView } from "@codemirror/merge";
import { EditorState } from "@codemirror/state";
import { EditorView, lineNumbers } from "@codemirror/view";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface DiffWindowData { startByte: number; nextByte: number; original: string; edited: string; atBof: boolean; atEof: boolean; }

const DIFF_BYTES = 64 * 1024;

const diffTheme = EditorView.theme(
  {
    "&": { fontSize: "12px", backgroundColor: "#0e1217", color: "#cdd6e4" },
    ".cm-scroller": { fontFamily: "'JetBrains Mono',Consolas,monospace" },
    ".cm-gutters": { backgroundColor: "#0b0f14", color: "#56627a", border: "none" },
  },
  { dark: true },
);

interface Props {
  fileId: string;
  startByte: number;
}

export default function DiffView({ fileId, startByte }: Props) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let mv: MergeView | null = null;
    let cancelled = false;
    (async () => {
      try {
        const w = (await FileService.GetDiffWindow(fileId, startByte, DIFF_BYTES)) as DiffWindowData;
        if (cancelled || !ref.current) return;
        const ext = [EditorState.readOnly.of(true), EditorView.editable.of(false), lineNumbers(), EditorView.lineWrapping, diffTheme];
        mv = new MergeView({
          parent: ref.current,
          a: { doc: w.original, extensions: ext },
          b: { doc: w.edited, extensions: ext },
          gutter: true,
          highlightChanges: true,
          collapseUnchanged: { margin: 3, minSize: 6 },
        });
      } catch (e: any) {
        if (!cancelled && ref.current) ref.current.textContent = String(e?.message ?? e);
      }
    })();
    return () => {
      cancelled = true;
      if (mv) mv.destroy();
      if (ref.current) ref.current.innerHTML = "";
    };
  }, [fileId, startByte]);

  return <div className="q-merge" ref={ref} />;
}
