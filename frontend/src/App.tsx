import { useEffect, useRef, useState } from "react";
import { QuarryEditor } from "./editor/QuarryEditor";
import type { FileMetaData, WindowStatus } from "./editor/QuarryEditor";
import "./quarry.css";

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}

function App() {
  const hostRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<QuarryEditor | null>(null);
  const [path, setPath] = useState("");
  const [meta, setMeta] = useState<FileMetaData | null>(null);
  const [status, setStatus] = useState<WindowStatus | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!hostRef.current) return;
    const ed = new QuarryEditor(hostRef.current, setStatus);
    editorRef.current = ed;
    return () => {
      ed.destroy();
      editorRef.current = null;
    };
  }, []);

  const open = async () => {
    const p = path.trim();
    if (!p || !editorRef.current) return;
    setError("");
    setBusy(true);
    try {
      const m = await editorRef.current.open(p);
      setMeta(m);
    } catch (e: any) {
      setError(String(e?.message ?? e));
      setMeta(null);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="q-app">
      <header className="q-top">
        <span className="q-brand">Quarry</span>
        <input
          className="q-path"
          placeholder="Paste a file path, e.g. E:\dumps\big.sql"
          value={path}
          spellCheck={false}
          onChange={(e) => setPath(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void open();
          }}
        />
        <button className="q-open" onClick={() => void open()} disabled={busy}>
          {busy ? "Opening…" : "Open"}
        </button>
      </header>

      {error && <div className="q-error">{error}</div>}

      <div className="q-editor" ref={hostRef} />

      <footer className="q-status">
        {meta ? (
          <>
            <span className="q-file" title={meta.path}>
              {meta.path}
            </span>
            <span>{fmtBytes(meta.size)}</span>
            <span>{meta.encoding || "?"}</span>
            <span>{meta.detected || "plain"}</span>
            {status && (
              <span>
                bytes 0x{status.startByte.toString(16)}–0x{status.endByte.toString(16)}
              </span>
            )}
            {status && (
              <span>
                lines {status.firstLine}–{status.lastLine}
                {status.approx ? " (approx)" : ""}
              </span>
            )}
            {status?.atBof && <span className="q-edge">BOF</span>}
            {status?.atEof && <span className="q-edge">EOF</span>}
          </>
        ) : (
          <span className="q-hint">
            Open a file to begin — windows stream on scroll; the file is never fully loaded into memory.
          </span>
        )}
      </footer>
    </div>
  );
}

export default App;
