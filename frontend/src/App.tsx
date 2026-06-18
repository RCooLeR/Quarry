import { useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";
import { QuarryEditor } from "./editor/QuarryEditor";
import type { FileMetaData, WindowStatus } from "./editor/QuarryEditor";
import "./quarry.css";

interface Tab {
  fileId: string;
  meta: FileMetaData;
  startByte: number; // last window start, so re-activating returns you here
}

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

function baseName(p: string): string {
  const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return i >= 0 ? p.slice(i + 1) : p;
}

function App() {
  const hostRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<QuarryEditor | null>(null);

  const [tabs, setTabs] = useState<Tab[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [status, setStatus] = useState<WindowStatus | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [path, setPath] = useState("");

  // search + navigation state
  const [searchOpen, setSearchOpen] = useState(false);
  const [gotoOpen, setGotoOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [regex, setRegex] = useState(false);
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [wholeWord, setWholeWord] = useState(false);
  const [searching, setSearching] = useState(false);
  const [searchInfo, setSearchInfo] = useState("");
  const [gotoValue, setGotoValue] = useState("");
  const lastMatch = useRef<number | null>(null);

  // Refs mirror state so the editor's status callback isn't stale.
  const activeIdRef = useRef<string | null>(null);
  const tabsRef = useRef<Tab[]>([]);
  activeIdRef.current = activeId;
  tabsRef.current = tabs;

  useEffect(() => {
    if (!hostRef.current) return;
    const ed = new QuarryEditor(hostRef.current, (s) => {
      setStatus(s);
      if (s) {
        const t = tabsRef.current.find((t) => t.fileId === activeIdRef.current);
        if (t) t.startByte = s.startByte;
      }
    });
    editorRef.current = ed;
    const onKey = (e: KeyboardEvent) => {
      const mod = e.ctrlKey || e.metaKey;
      if (mod && e.key.toLowerCase() === "o") {
        e.preventDefault();
        void openViaDialog();
      } else if (mod && e.key.toLowerCase() === "f") {
        e.preventDefault();
        setGotoOpen(false);
        setSearchOpen(true);
      } else if (mod && e.key.toLowerCase() === "g") {
        e.preventDefault();
        setSearchOpen(false);
        setGotoOpen(true);
      } else if (e.key === "Escape") {
        setSearchOpen(false);
        setGotoOpen(false);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      ed.destroy();
      editorRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const showMeta = async (meta: FileMetaData) => {
    if (!meta.fileId) return; // cancelled dialog
    const ed = editorRef.current;
    if (!ed) return;
    if (tabsRef.current.some((t) => t.fileId === meta.fileId)) {
      await activate(meta.fileId);
      return;
    }
    const tab: Tab = { fileId: meta.fileId, meta, startByte: 0 };
    setTabs((prev) => [...prev, tab]);
    tabsRef.current = [...tabsRef.current, tab];
    setActiveId(meta.fileId);
    activeIdRef.current = meta.fileId;
    lastMatch.current = null;
    await ed.attach(meta.fileId, 0);
  };

  const openViaDialog = async () => {
    setError("");
    setBusy(true);
    try {
      const meta = (await FileService.OpenViaDialog()) as FileMetaData;
      await showMeta(meta);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const openPath = async () => {
    const p = path.trim();
    if (!p) return;
    setError("");
    setBusy(true);
    try {
      const meta = (await FileService.OpenFile(p)) as FileMetaData;
      await showMeta(meta);
      setPath("");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const activate = async (fileId: string) => {
    const ed = editorRef.current;
    const t = tabsRef.current.find((t) => t.fileId === fileId);
    if (!ed || !t) return;
    setActiveId(fileId);
    activeIdRef.current = fileId;
    lastMatch.current = null;
    await ed.attach(fileId, t.startByte);
  };

  const closeTab = async (fileId: string, e?: React.MouseEvent) => {
    e?.stopPropagation();
    void FileService.CloseFile(fileId).catch(() => {});
    const remaining = tabsRef.current.filter((t) => t.fileId !== fileId);
    setTabs(remaining);
    tabsRef.current = remaining;
    if (activeIdRef.current === fileId) {
      const next = remaining[remaining.length - 1];
      if (next) {
        await activate(next.fileId);
      } else {
        setActiveId(null);
        activeIdRef.current = null;
        editorRef.current?.clear();
        setStatus(null);
      }
    }
  };

  const resetSearch = () => {
    lastMatch.current = null;
  };

  const runFind = async (dir: "next" | "prev") => {
    const q = query;
    const id = activeIdRef.current;
    if (!q.trim() || !id) return;
    const tab = tabsRef.current.find((t) => t.fileId === id);
    if (!tab) return;
    setSearching(true);
    setSearchInfo("Searching…");
    try {
      let hit;
      if (dir === "next") {
        const from = lastMatch.current != null ? lastMatch.current + 1 : tab.startByte;
        hit = await FileService.FindNext(id, q, from, regex, caseSensitive, wholeWord);
      } else {
        const before = lastMatch.current != null ? lastMatch.current : tab.startByte;
        hit = await FileService.FindPrev(id, q, before, regex, caseSensitive, wholeWord);
      }
      if (hit.timedOut) {
        setSearchInfo("Timed out — try a narrower query");
        return;
      }
      if (!hit.found) {
        setSearchInfo(dir === "next" ? "No matches below" : "No matches above");
        return;
      }
      lastMatch.current = hit.offset;
      await editorRef.current?.showMatch(hit.offset, hit.length, q, regex, caseSensitive);
      setSearchInfo(`0x${hit.offset.toString(16)} · line ~${hit.line}`);
    } catch (e: any) {
      setSearchInfo(String(e?.message ?? e));
    } finally {
      setSearching(false);
    }
  };

  const doGoto = async () => {
    const v = gotoValue.trim();
    const id = activeIdRef.current;
    if (!v || !id) return;
    let offset: number | null = null;
    if (/^0x[0-9a-f]+$/i.test(v)) {
      offset = parseInt(v.slice(2), 16);
    } else if (/^\d+$/.test(v)) {
      offset = await FileService.ResolveLine(id, parseInt(v, 10));
    }
    if (offset == null || Number.isNaN(offset)) {
      setSearchInfo("Enter a line number or 0xHEX offset");
      return;
    }
    lastMatch.current = null;
    await editorRef.current?.gotoByte(offset);
    setGotoOpen(false);
  };

  const hasFiles = tabs.length > 0;
  const activeTab = tabs.find((t) => t.fileId === activeId);

  return (
    <div className="q-app">
      <header className="q-top">
        <div className="q-brand">
          <img className="q-brand-mark" src="/logos/symbol.png" alt="Quarry" />
          <span className="q-brand-name">Quarry</span>
        </div>
        <div className="q-spacer" />
        {hasFiles && (
          <>
            <button className="q-btn" title="Find (Ctrl+F)" onClick={() => { setGotoOpen(false); setSearchOpen(true); }}>
              Find
            </button>
            <button className="q-btn" title="Go to line / offset (Ctrl+G)" onClick={() => { setSearchOpen(false); setGotoOpen(true); }}>
              Go to
            </button>
          </>
        )}
        <button className="q-btn q-btn-primary" onClick={() => void openViaDialog()} disabled={busy}>
          {busy ? "Opening…" : "Open file"}
        </button>
      </header>

      {hasFiles && (
        <div className="q-tabs">
          {tabs.map((t) => (
            <div
              key={t.fileId}
              className={"q-tab" + (t.fileId === activeId ? " q-tab-active" : "")}
              onClick={() => void activate(t.fileId)}
              title={t.meta.path}
            >
              <span className="q-tab-name">{baseName(t.meta.path)}</span>
              <span className="q-tab-close" onClick={(e) => void closeTab(t.fileId, e)}>
                ×
              </span>
            </div>
          ))}
        </div>
      )}

      {error && <div className="q-error">{error}</div>}

      <div className="q-stage">
        <div className="q-editor" ref={hostRef} />

        {searchOpen && hasFiles && (
          <div className="q-find">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Find…"
              value={query}
              spellCheck={false}
              onChange={(e) => {
                setQuery(e.target.value);
                resetSearch();
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void runFind(e.shiftKey ? "prev" : "next");
                } else if (e.key === "Escape") {
                  setSearchOpen(false);
                }
              }}
            />
            <button className={"q-toggle" + (caseSensitive ? " on" : "")} title="Match case" onClick={() => { setCaseSensitive((v) => !v); resetSearch(); }}>
              Aa
            </button>
            <button className={"q-toggle" + (wholeWord ? " on" : "")} title="Whole word" onClick={() => { setWholeWord((v) => !v); resetSearch(); }}>
              W
            </button>
            <button className={"q-toggle" + (regex ? " on" : "")} title="Regular expression" onClick={() => { setRegex((v) => !v); resetSearch(); }}>
              .*
            </button>
            <button className="q-icon" title="Previous (Shift+Enter)" disabled={searching} onClick={() => void runFind("prev")}>
              ↑
            </button>
            <button className="q-icon" title="Next (Enter)" disabled={searching} onClick={() => void runFind("next")}>
              ↓
            </button>
            <span className="q-find-info">{searchInfo}</span>
            <button className="q-icon" title="Close (Esc)" onClick={() => setSearchOpen(false)}>
              ×
            </button>
          </div>
        )}

        {gotoOpen && hasFiles && (
          <div className="q-find q-goto">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Go to line, or 0xHEX byte offset"
              value={gotoValue}
              spellCheck={false}
              onChange={(e) => setGotoValue(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void doGoto();
                } else if (e.key === "Escape") {
                  setGotoOpen(false);
                }
              }}
            />
            <button className="q-icon" title="Go" onClick={() => void doGoto()}>
              Go
            </button>
            <button className="q-icon" title="Close (Esc)" onClick={() => setGotoOpen(false)}>
              ×
            </button>
          </div>
        )}

        {!hasFiles && (
          <div className="q-empty">
            <img className="q-empty-logo" src="/logos/wordmark.png" alt="Quarry" />
            <p className="q-empty-tag">Open and edit very large files — SQL dumps, CSVs, logs.</p>
            <button className="q-btn q-btn-primary q-empty-open" onClick={() => void openViaDialog()} disabled={busy}>
              Open file…
            </button>
            <div className="q-empty-path">
              <input
                className="q-path"
                placeholder="…or paste a path, e.g. E:\dumps\big.sql"
                value={path}
                spellCheck={false}
                onChange={(e) => setPath(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void openPath();
                }}
              />
              <button className="q-btn" onClick={() => void openPath()} disabled={busy || !path.trim()}>
                Open
              </button>
            </div>
            <p className="q-empty-hint">The file is never fully loaded — windows stream as you scroll.</p>
          </div>
        )}
      </div>

      <footer className="q-status">
        {status && activeTab ? (
          <>
            <span className="q-file" title={activeTab.meta.path}>
              {activeTab.meta.path}
            </span>
            <span className="q-sep" />
            <span>{fmtBytes(activeTab.meta.size)}</span>
            <span>{activeTab.meta.encoding || "?"}</span>
            <span>{activeTab.meta.detected || "plain"}</span>
            <span className="q-spacer" />
            <span>
              0x{status.startByte.toString(16)}–0x{status.endByte.toString(16)}
            </span>
            <span>
              lines {status.firstLine}–{status.lastLine}
              {status.approx ? " ~" : ""}
            </span>
            {status.atBof && <span className="q-edge">BOF</span>}
            {status.atEof && <span className="q-edge">EOF</span>}
          </>
        ) : (
          <span className="q-muted">Ready</span>
        )}
      </footer>
    </div>
  );
}

export default App;
