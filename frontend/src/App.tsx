import { useEffect, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";
import { QuarryEditor } from "./editor/QuarryEditor";
import type { FileMetaData, StagingStateData, WindowStatus } from "./editor/QuarryEditor";
import Tools from "./Tools";
import CsvGrid from "./CsvGrid";
import HexView from "./HexView";
import DiffView from "./DiffView";
import Sidebar from "./Sidebar";
import MenuBar from "./MenuBar";
import type { MenuDef, MenuItem } from "./MenuBar";
import CommandPalette from "./CommandPalette";
import type { Command } from "./CommandPalette";
import XRay from "./XRay";
import type { XRayRegion } from "./XRay";
import "./quarry.css";

interface Tab {
  fileId: string;
  meta: FileMetaData;
  startByte: number;
}

interface StagedEditData {
  start: number;
  end: number;
  line: number;
  old: string;
  new: string;
}

interface SqlTableInfo { name: string; createOffset: number; insertOffset: number; bytes: number; }
interface SqlSummaryResult { tables: SqlTableInfo[]; createTables: number; insertTables: number; definerCount: number; header: boolean; }

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

function snippet(s: string): string {
  return s.replace(/\n/g, "⏎").slice(0, 120);
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

  const [searchOpen, setSearchOpen] = useState(false);
  const [gotoOpen, setGotoOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [regex, setRegex] = useState(false);
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [wholeWord, setWholeWord] = useState(false);
  const [searching, setSearching] = useState(false);
  const [searchInfo, setSearchInfo] = useState("");
  const [results, setResults] = useState<{ offset: number; length: number; line: number; preview: string }[] | null>(null);
  const [resultsInfo, setResultsInfo] = useState("");
  const [gotoValue, setGotoValue] = useState("");
  const lastMatch = useRef<number | null>(null);

  const [editMode, setEditMode] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [staging, setStaging] = useState<StagingStateData | null>(null);
  const [diffOpen, setDiffOpen] = useState(false);
  const [diffMode, setDiffMode] = useState<"list" | "side">("list");
  const [stagedEdits, setStagedEdits] = useState<StagedEditData[]>([]);
  const [notice, setNotice] = useState("");
  const [toolsOpen, setToolsOpen] = useState(false);
  const [gridView, setGridView] = useState(false);
  const [hexView, setHexView] = useState(false);
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [analyses, setAnalyses] = useState<Record<string, SqlSummaryResult>>({});
  const [xrayOn, setXrayOn] = useState(true);
  const [theme, setTheme] = useState<string>(() => localStorage.getItem("quarry.theme") || "dark");
  const [recent, setRecent] = useState<string[]>(() => {
    try { return JSON.parse(localStorage.getItem("quarry.recent") || "[]"); } catch { return []; }
  });
  const restoredRef = useRef(false);
  const [bookmarks, setBookmarks] = useState<Record<string, { offset: number; label: string }[]>>(() => {
    try { return JSON.parse(localStorage.getItem("quarry.bookmarks") || "{}"); } catch { return {}; }
  });
  const [bookmarksOpen, setBookmarksOpen] = useState(false);
  const [following, setFollowing] = useState(false);

  const activeIdRef = useRef<string | null>(null);
  const tabsRef = useRef<Tab[]>([]);
  activeIdRef.current = activeId;
  tabsRef.current = tabs;

  useEffect(() => {
    if (!hostRef.current) return;
    const initialTheme = localStorage.getItem("quarry.theme") || "dark";
    const ed = new QuarryEditor(hostRef.current, {
      onStatus: setStatus,
      onDirty: setDirty,
      onStaging: (s) => setStaging(s),
      onMode: (on) => setEditMode(on),
    }, initialTheme);
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
      } else if (mod && e.key.toLowerCase() === "b") {
        e.preventDefault();
        setSidebarCollapsed((v) => !v);
      } else if (mod && e.key.toLowerCase() === "w") {
        if (activeIdRef.current) {
          e.preventDefault();
          void closeTab(activeIdRef.current);
        }
      } else if (mod && e.key.toLowerCase() === "p") {
        e.preventDefault();
        setPaletteOpen(true);
      } else if (e.key === "Escape") {
        setSearchOpen(false);
        setGotoOpen(false);
        setPaletteOpen(false);
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

  // Apply the theme to the document root + editor whenever it changes.
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("quarry.theme", theme);
    editorRef.current?.setTheme(theme);
  }, [theme]);

  const toggleTheme = () => setTheme((t) => (t === "light" ? "dark" : "light"));

  // Tail/follow: poll the active file's size; when it grows, reload from disk
  // and jump to the new end. Stops automatically when editing (reload would
  // discard staged edits) or when the active file changes.
  useEffect(() => {
    if (!following) return;
    const id = activeId;
    if (!id) return;
    let lastSize = tabsRef.current.find((t) => t.fileId === id)?.meta.size ?? 0;
    let stop = false;
    const tick = async () => {
      if (stop) return;
      try {
        const size = (await FileService.FileSize(id)) as number;
        if (size > lastSize) {
          lastSize = size;
          const m = (await FileService.RefreshFile(id)) as FileMetaData;
          setTabs((prev) => prev.map((t) => (t.fileId === id ? { ...t, meta: { ...t.meta, size: m.size } } : t)));
          tabsRef.current = tabsRef.current.map((t) => (t.fileId === id ? { ...t, meta: { ...t.meta, size: m.size } } : t));
          await editorRef.current?.gotoByte(m.size);
        }
      } catch { /* file may be momentarily locked; retry next tick */ }
    };
    const h = window.setInterval(() => void tick(), 1500);
    return () => { stop = true; window.clearInterval(h); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [following, activeId]);

  // Following reloads from disk, so turn it off as soon as the user starts editing.
  useEffect(() => { if (editMode && following) setFollowing(false); }, [editMode]); // eslint-disable-line react-hooks/exhaustive-deps

  // Open a file by path (used by drag-drop and session restore).
  const openFilePath = async (p: string) => {
    if (!p.trim()) return;
    setError("");
    setBusy(true);
    try {
      const meta = (await FileService.OpenFile(p)) as FileMetaData;
      await showMeta(meta);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };
  const openFileRef = useRef(openFilePath);
  openFileRef.current = openFilePath;

  // Native file drops (Wails) → open each dropped path.
  useEffect(() => {
    const off = Events.On("quarry:files-dropped", (e: any) => {
      const files = (e?.data ?? []) as string[];
      void (async () => { for (const f of files) await openFileRef.current(f); })();
    });
    return () => { try { off(); } catch { /* ignore */ } };
  }, []);

  const pushRecent = (p: string) => {
    if (!p) return;
    setRecent((prev) => {
      const next = [p, ...prev.filter((x) => x !== p)].slice(0, 12);
      localStorage.setItem("quarry.recent", JSON.stringify(next));
      return next;
    });
  };

  const saveBookmarks = (next: Record<string, { offset: number; label: string }[]>) => {
    setBookmarks(next);
    localStorage.setItem("quarry.bookmarks", JSON.stringify(next));
  };
  const addBookmark = () => {
    const t = tabsRef.current.find((x) => x.fileId === activeIdRef.current);
    if (!t) return;
    const offset = status?.startByte ?? 0;
    const label = `line ~${status?.firstLine ?? "?"}`;
    const list = bookmarks[t.meta.path] ?? [];
    if (list.some((b) => b.offset === offset)) { setBookmarksOpen(true); return; }
    const next = { ...bookmarks, [t.meta.path]: [...list, { offset, label }].sort((a, b) => a.offset - b.offset) };
    saveBookmarks(next);
    setBookmarksOpen(true);
    setNotice(`Bookmarked 0x${offset.toString(16)}`);
  };
  const removeBookmark = (path: string, offset: number) => {
    const list = (bookmarks[path] ?? []).filter((b) => b.offset !== offset);
    const next = { ...bookmarks };
    if (list.length) next[path] = list; else delete next[path];
    saveBookmarks(next);
  };

  // Persist the open-file set so a relaunch can restore it.
  useEffect(() => {
    if (!restoredRef.current) return; // don't clobber saved session before restore
    const paths = tabs.map((t) => t.meta.path).filter(Boolean);
    localStorage.setItem("quarry.session", JSON.stringify(paths));
  }, [tabs]);

  // Restore last session's files once, on first mount.
  useEffect(() => {
    let saved: string[] = [];
    try { saved = JSON.parse(localStorage.getItem("quarry.session") || "[]"); } catch { saved = []; }
    void (async () => {
      for (const p of saved) await openFileRef.current(p);
      restoredRef.current = true;
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const refreshStaging = async (fileId: string) => {
    try {
      const s = (await FileService.GetStagingState(fileId)) as StagingStateData;
      setStaging(s);
      if (diffOpen) await loadDiff(fileId);
    } catch {
      /* ignore */
    }
  };

  const loadDiff = async (fileId: string) => {
    try {
      const edits = (await FileService.GetStagedEdits(fileId)) as StagedEditData[] | null;
      setStagedEdits(edits ?? []);
    } catch {
      setStagedEdits([]);
    }
  };

  // Run (or re-run) SQL analysis and cache it per file; shared by the Tools
  // panel, the command palette (jump-to-table), and the file X-ray.
  const analyze = async (fileId: string): Promise<SqlSummaryResult> => {
    const s = (await FileService.SqlAnalyze(fileId)) as SqlSummaryResult;
    setAnalyses((prev) => ({ ...prev, [fileId]: s }));
    return s;
  };

  const showMeta = async (meta: FileMetaData) => {
    if (!meta.fileId) return;
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
    await ed.attach(meta.fileId, meta.detected, meta.path, 0);
    await refreshStaging(meta.fileId);
    pushRecent(meta.path);
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
    await ed.attach(fileId, t.meta.detected, t.meta.path, t.startByte);
    await refreshStaging(fileId);
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
        setStaging(null);
      }
    }
  };

  // ---- search / goto ----
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
      if (hit.unsupported) {
        setSearchInfo(hit.message || "Search not supported for this file's encoding");
        return;
      }
      if (hit.timedOut) {
        setSearchInfo("Timed out — narrow the query");
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

  const runSearchAll = async () => {
    const q = query;
    const id = activeIdRef.current;
    if (!q.trim() || !id) return;
    setSearching(true);
    setResultsInfo("Searching whole file…");
    setResults([]);
    try {
      const r = (await FileService.SearchAll(id, q, regex, caseSensitive, wholeWord, 1000)) as any;
      if (r.unsupported) { setResultsInfo(r.message || "Unsupported for this encoding"); setResults(null); return; }
      if (r.timedOut) { setResultsInfo("Timed out — narrow the query"); }
      setResults(r.hits ?? []);
      setResultsInfo(`${(r.hits ?? []).length}${r.truncated ? "+" : ""} matches`);
    } catch (e: any) {
      setResultsInfo(String(e?.message ?? e));
      setResults(null);
    } finally {
      setSearching(false);
    }
  };

  const harvest = async () => {
    const q = query;
    const id = activeIdRef.current;
    if (!q.trim() || !id) return;
    setBusy(true);
    setNotice("");
    try {
      const r = await FileService.HarvestMatchesViaDialog(id, q, !caseSensitive);
      if (r.outputPath) setNotice(`Extracted ${r.recordsWritten} matches → ${r.outputPath}`);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const doGoto = async () => {
    const v = gotoValue.trim();
    const id = activeIdRef.current;
    if (!v || !id) return;
    const tab = tabsRef.current.find((t) => t.fileId === id);
    let offset: number | null = null;
    const pct = v.match(/^(\d{1,3}(?:\.\d+)?)\s*%$/);
    if (pct && tab) {
      const p = Math.max(0, Math.min(100, parseFloat(pct[1])));
      offset = Math.floor((tab.meta.size * p) / 100);
    } else if (/^0x[0-9a-f]+$/i.test(v)) offset = parseInt(v.slice(2), 16);
    else if (/^\d+$/.test(v)) offset = await FileService.ResolveLine(id, parseInt(v, 10));
    if (offset == null || Number.isNaN(offset)) {
      setSearchInfo("Enter a line number, 0xHEX offset, or NN%");
      return;
    }
    lastMatch.current = null;
    await editorRef.current?.gotoByte(offset);
    setGotoOpen(false);
  };

  // ---- editing / save ----
  const toggleEdit = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    setNotice("");
    await ed.setEditMode(!editMode);
    await refreshStaging(id);
  };

  const toggleDiff = async () => {
    const id = activeIdRef.current;
    const next = !diffOpen;
    setDiffOpen(next);
    if (next && id) await loadDiff(id);
  };

  const discardEdits = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    try {
      const s = (await FileService.DiscardEdits(id)) as StagingStateData;
      setStaging(s);
      setStagedEdits([]);
      await ed.refresh();
      setNotice("Edits discarded");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const saveInPlace = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    setBusy(true);
    setNotice("");
    try {
      await ed.flush();
      const r = await FileService.SavePatch(id);
      await ed.setEditMode(false);
      await ed.refresh();
      await refreshStaging(id);
      setNotice(`Patched in place — ${fmtBytes(r.bytesWritten)} written`);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const saveCopy = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    setBusy(true);
    setNotice("");
    try {
      await ed.flush();
      const r = await FileService.SaveCopyViaDialog(id);
      if (r.mode === "copy") {
        setNotice(`Saved copy → ${r.outputPath} (${fmtBytes(r.bytesWritten)})`);
        await refreshStaging(id);
      }
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const hasFiles = tabs.length > 0;
  const activeTab = tabs.find((t) => t.fileId === activeId);
  const editCount = staging?.editCount ?? 0;
  const hasEdits = editCount > 0 || dirty;
  const detected = (activeTab?.meta.detected ?? "").toLowerCase();
  const isCsv = detected === "csv" || detected === "tsv";
  const isSql = detected === "sql";
  const openSearch = () => { setGotoOpen(false); setSearchOpen(true); };
  const openGoto = () => { setSearchOpen(false); setGotoOpen(true); };
  const showText = () => { setHexView(false); setGridView(false); };

  const toolsMenuItems = (): MenuItem[] => {
    if (isCsv) {
      return [
        { label: "CSV → SQL converter…", onClick: () => setToolsOpen(true) },
        { label: "CSV column tools…", onClick: () => setToolsOpen(true) },
      ];
    }
    if (isSql) {
      return [
        { label: "Analyze & extract tables…", onClick: () => setToolsOpen(true) },
        { label: "Find & replace…", onClick: () => setToolsOpen(true) },
        { label: "Cleanup presets…", onClick: () => setToolsOpen(true) },
      ];
    }
    return [{ label: "No data tools for this file type", disabled: true }];
  };

  const menus: MenuDef[] = [
    {
      label: "File",
      items: [
        { label: "Open file…", shortcut: "Ctrl+O", onClick: () => void openViaDialog() },
        { label: "Close tab", shortcut: "Ctrl+W", disabled: !activeTab, onClick: () => activeId && void closeTab(activeId) },
        ...(recent.length > 0
          ? [
              { separator: true } as MenuItem,
              { label: "Recent files", disabled: true } as MenuItem,
              ...recent.slice(0, 10).map((p) => ({
                label: "  " + p.replace(/^.*[\\/]/, ""),
                onClick: () => void openFilePath(p),
              })),
              { label: "Clear recent", onClick: () => { setRecent([]); localStorage.removeItem("quarry.recent"); } },
            ]
          : []),
        { separator: true },
        { label: "Patch in place", disabled: !staging?.inPlaceEligible, onClick: () => void saveInPlace() },
        { label: "Save copy…", disabled: !hasEdits, onClick: () => void saveCopy() },
        { label: "Discard edits", disabled: !hasEdits, onClick: () => void discardEdits() },
      ],
    },
    {
      label: "Edit",
      items: [
        { label: "Find…", shortcut: "Ctrl+F", disabled: !hasFiles, onClick: openSearch },
        { label: "Go to line / offset…", shortcut: "Ctrl+G", disabled: !hasFiles, onClick: openGoto },
        { separator: true },
        { label: "Edit mode", checked: editMode, disabled: !activeTab?.meta.editable, onClick: () => void toggleEdit() },
        { label: "Diff panel", checked: diffOpen, disabled: !hasEdits, onClick: () => void toggleDiff() },
      ],
    },
    {
      label: "View",
      items: [
        { label: "Sidebar", shortcut: "Ctrl+B", checked: !sidebarCollapsed, onClick: () => setSidebarCollapsed((v) => !v) },
        { label: "File map (X-ray)", checked: xrayOn, onClick: () => setXrayOn((v) => !v) },
        { label: "Bookmarks", disabled: !hasFiles, checked: bookmarksOpen, onClick: () => setBookmarksOpen((v) => !v) },
        { label: "Add bookmark here", disabled: !activeTab, onClick: addBookmark },
        { label: "Follow tail (live)", disabled: !hasFiles || hasEdits, checked: following, onClick: () => setFollowing((v) => !v) },
        { label: "Command palette…", shortcut: "Ctrl+P", onClick: () => setPaletteOpen(true) },
        { label: "Light theme", checked: theme === "light", onClick: toggleTheme },
        { separator: true },
        { label: "Plain text", checked: !hexView && !gridView, disabled: !hasFiles, onClick: showText },
        { label: "Hex view", checked: hexView, disabled: !hasFiles, onClick: () => { setGridView(false); setHexView((v) => !v); } },
        { label: "Grid view", checked: gridView, disabled: !isCsv, onClick: () => { setHexView(false); setGridView((v) => !v); } },
        { separator: true },
        { label: "Data tools panel", checked: toolsOpen, disabled: !(isCsv || isSql), onClick: () => setToolsOpen((v) => !v) },
      ],
    },
    { label: "Tools", items: toolsMenuItems() },
    {
      label: "Help",
      items: [
        { label: "About Quarry", onClick: () => setNotice("Quarry — a streaming editor and toolkit for very large SQL/CSV dumps. Files are never fully loaded; windows stream as you scroll.") },
      ],
    },
  ];

  const commands: Command[] = [
    { id: "open", group: "File", label: "Open file…", hint: "Ctrl+O", run: () => void openViaDialog() },
    { id: "close", group: "File", label: "Close tab", hint: "Ctrl+W", run: () => activeId && void closeTab(activeId) },
    { id: "patch", group: "File", label: "Patch in place", run: () => void saveInPlace() },
    { id: "savecopy", group: "File", label: "Save copy…", run: () => void saveCopy() },
    { id: "discard", group: "File", label: "Discard edits", run: () => void discardEdits() },
    { id: "find", group: "Edit", label: "Find…", hint: "Ctrl+F", run: openSearch },
    { id: "goto", group: "Edit", label: "Go to line / offset…", hint: "Ctrl+G", run: openGoto },
    { id: "edit", group: "Edit", label: editMode ? "Turn editing off" : "Turn editing on", run: () => void toggleEdit() },
    { id: "sidebar", group: "View", label: "Toggle sidebar", hint: "Ctrl+B", run: () => setSidebarCollapsed((v) => !v) },
    { id: "theme", group: "View", label: theme === "light" ? "Switch to dark theme" : "Switch to light theme", run: toggleTheme },
    { id: "bookmark", group: "View", label: "Add bookmark here", run: addBookmark },
    { id: "bookmarks", group: "View", label: "Toggle bookmarks panel", run: () => setBookmarksOpen((v) => !v) },
    { id: "follow", group: "View", label: following ? "Stop following tail" : "Follow tail (live)", run: () => setFollowing((v) => !v) },
    { id: "text", group: "View", label: "Plain text view", run: showText },
    { id: "hex", group: "View", label: "Toggle hex view", run: () => { setGridView(false); setHexView((v) => !v); } },
    { id: "grid", group: "View", label: "Toggle grid view", run: () => { setHexView(false); setGridView((v) => !v); } },
    { id: "tools", group: "Tools", label: (isCsv || isSql) ? "Toggle data tools panel" : "Data tools (CSV/SQL only)", run: () => (isCsv || isSql) && setToolsOpen((v) => !v) },
  ];

  // File X-ray regions + table-jump commands, from cached analysis of the active file.
  const analysis = activeId ? analyses[activeId] : null;
  const xrayRegions: XRayRegion[] = [];
  if (analysis && activeTab) {
    const sorted = analysis.tables
      .map((t) => ({ off: t.createOffset >= 0 ? t.createOffset : t.insertOffset, name: t.name, bytes: t.bytes }))
      .filter((t) => t.off >= 0)
      .sort((x, y) => x.off - y.off);
    for (let i = 0; i < sorted.length; i++) {
      const t = sorted[i];
      const end = t.bytes > 0 ? t.off + t.bytes : sorted[i + 1]?.off ?? activeTab.meta.size;
      xrayRegions.push({ start: t.off, end, name: t.name });
    }
  }
  const seekTo = (byte: number) => void editorRef.current?.gotoByte(byte);
  if (analysis) {
    for (const t of analysis.tables.slice(0, 300)) {
      const off = t.createOffset >= 0 ? t.createOffset : t.insertOffset;
      if (off >= 0) {
        commands.push({ id: "tbl:" + t.name, group: "Table", label: "Jump to " + t.name, hint: fmtBytes(t.bytes), run: () => seekTo(off) });
      }
    }
  }

  return (
    <div className="q-app" data-file-drop-target>
      <header className="q-top">
        <div className="q-brand">
          <img className="q-brand-mark" src="/logos/symbol.png" alt="Quarry" />
          <span className="q-brand-name">Quarry</span>
        </div>
        <MenuBar menus={menus} />
        <div className="q-spacer" />
        {hasFiles && (
          <>
            <button className="q-btn" title="Find (Ctrl+F)" onClick={openSearch}>Find</button>
            {activeTab?.meta.editable && (
              <button className={"q-btn" + (editMode ? " q-btn-on" : "")} title="Toggle editing" onClick={() => void toggleEdit()}>
                {editMode ? "Editing" : "Edit"}
              </button>
            )}
            {hasEdits && (
              <>
                <button className={"q-btn" + (diffOpen ? " q-btn-on" : "")} onClick={() => void toggleDiff()}>
                  Diff ({editCount})
                </button>
                <button
                  className="q-btn q-btn-primary"
                  disabled={busy || !staging?.inPlaceEligible}
                  title={staging?.inPlaceEligible ? "Overwrite changed bytes in place (with backup)" : "Length changed — use Save copy"}
                  onClick={() => void saveInPlace()}
                >
                  Patch in place
                </button>
                <button className="q-btn q-btn-primary" disabled={busy} title="Stream a full edited copy to a new file" onClick={() => void saveCopy()}>
                  Save copy…
                </button>
              </>
            )}
          </>
        )}
        <button className="q-btn q-btn-primary" onClick={() => void openViaDialog()} disabled={busy}>
          {busy ? "Working…" : "Open file"}
        </button>
      </header>

      {error && <div className="q-error">{error}</div>}
      {notice && <div className="q-notice">{notice}</div>}

      <div className="q-body">
        {hasFiles && (
          <Sidebar
            tabs={tabs}
            activeId={activeId}
            collapsed={sidebarCollapsed}
            onActivate={(id) => void activate(id)}
            onClose={(id, e) => void closeTab(id, e)}
            onToggle={() => setSidebarCollapsed((v) => !v)}
          />
        )}
        <div className="q-stage">
          <div className={"q-editor" + (xrayOn && hasFiles ? " q-editor-xray" : "")} ref={hostRef} />

          {xrayOn && activeTab && status && (
            <XRay
              size={activeTab.meta.size}
              regions={xrayRegions}
              vpStart={status.startByte}
              vpEnd={status.endByte}
              onSeek={seekTo}
            />
          )}

        {gridView && activeTab && ["csv", "tsv"].includes(activeTab.meta.detected.toLowerCase()) && (
          <CsvGrid key={activeTab.fileId} fileId={activeTab.fileId} onError={setError} />
        )}

        {hexView && activeTab && (
          <HexView key={activeTab.fileId} fileId={activeTab.fileId} onError={setError} />
        )}

        {searchOpen && hasFiles && (
          <div className="q-find">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Find…"
              value={query}
              spellCheck={false}
              onChange={(e) => { setQuery(e.target.value); lastMatch.current = null; }}
              onKeyDown={(e) => {
                if (e.key === "Enter") { e.preventDefault(); void runFind(e.shiftKey ? "prev" : "next"); }
                else if (e.key === "Escape") setSearchOpen(false);
              }}
            />
            <button className={"q-toggle" + (caseSensitive ? " on" : "")} title="Match case" onClick={() => { setCaseSensitive((v) => !v); lastMatch.current = null; }}>Aa</button>
            <button className={"q-toggle" + (wholeWord ? " on" : "")} title="Whole word" onClick={() => { setWholeWord((v) => !v); lastMatch.current = null; }}>W</button>
            <button className={"q-toggle" + (regex ? " on" : "")} title="Regex" onClick={() => { setRegex((v) => !v); lastMatch.current = null; }}>.*</button>
            <button className="q-icon" title="Previous (Shift+Enter)" disabled={searching} onClick={() => void runFind("prev")}>↑</button>
            <button className="q-icon" title="Next (Enter)" disabled={searching} onClick={() => void runFind("next")}>↓</button>
            <button className="q-icon" title="List all matches" disabled={searching} onClick={() => void runSearchAll()}>≡</button>
            <button className="q-icon" title="Extract all regex matches → file" disabled={busy} onClick={() => void harvest()}>⤓</button>
            <span className="q-find-info">{searchInfo}</span>
            <button className="q-icon" title="Close (Esc)" onClick={() => { setSearchOpen(false); setResults(null); }}>×</button>
          </div>
        )}

        {searchOpen && results && hasFiles && (
          <div className="q-results">
            <div className="q-results-head">
              <span>{resultsInfo}</span>
              <span className="q-spacer" />
              <button className="q-icon" title="Close" onClick={() => setResults(null)}>×</button>
            </div>
            <div className="q-results-body">
              {results.length === 0 && <div className="q-results-empty">{resultsInfo || "No matches"}</div>}
              {results.map((r, i) => (
                <div
                  key={i}
                  className="q-result"
                  title={`0x${r.offset.toString(16)}`}
                  onClick={() => { lastMatch.current = r.offset; void editorRef.current?.showMatch(r.offset, r.length, query, regex, caseSensitive); }}
                >
                  <span className="q-result-line">{r.line}</span>
                  <span className="q-result-text">{r.preview}</span>
                </div>
              ))}
            </div>
          </div>
        )}

        {bookmarksOpen && activeTab && (
          <div className="q-results q-bookmarks">
            <div className="q-results-head">
              <span>Bookmarks · {(bookmarks[activeTab.meta.path] ?? []).length}</span>
              <span className="q-spacer" />
              <button className="q-icon" title="Add current position" onClick={addBookmark}>＋</button>
              <button className="q-icon" title="Close" onClick={() => setBookmarksOpen(false)}>×</button>
            </div>
            <div className="q-results-body">
              {(bookmarks[activeTab.meta.path] ?? []).length === 0 && (
                <div className="q-results-empty">No bookmarks. Use ＋ or “Add bookmark here”.</div>
              )}
              {(bookmarks[activeTab.meta.path] ?? []).map((b, i) => (
                <div key={i} className="q-result" title={`0x${b.offset.toString(16)}`}>
                  <span className="q-result-text" onClick={() => void editorRef.current?.gotoByte(b.offset)}>
                    {b.label} · 0x{b.offset.toString(16)}
                  </span>
                  <button className="q-icon" title="Remove" onClick={() => removeBookmark(activeTab.meta.path, b.offset)}>×</button>
                </div>
              ))}
            </div>
          </div>
        )}

        {gotoOpen && hasFiles && (
          <div className="q-find q-goto">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Go to line, 0xHEX byte offset, or NN%"
              value={gotoValue}
              spellCheck={false}
              onChange={(e) => setGotoValue(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") { e.preventDefault(); void doGoto(); }
                else if (e.key === "Escape") setGotoOpen(false);
              }}
            />
            <button className="q-icon" onClick={() => void doGoto()}>Go</button>
            <button className="q-icon" onClick={() => setGotoOpen(false)}>×</button>
          </div>
        )}

        {diffOpen && hasFiles && (
          <div className={"q-diff" + (diffMode === "side" ? " q-diff-wide" : "")}>
            <div className="q-diff-head">
              <span>Staged edits ({stagedEdits.length})</span>
              <span className="q-spacer" />
              <button className={"q-toggle" + (diffMode === "list" ? " on" : "")} onClick={() => setDiffMode("list")}>List</button>
              <button className={"q-toggle" + (diffMode === "side" ? " on" : "")} onClick={() => setDiffMode("side")}>Side</button>
              <button className="q-icon" onClick={() => setDiffOpen(false)}>×</button>
            </div>
            {diffMode === "side" ? (
              <DiffView key={(activeId ?? "") + ":" + (status?.startByte ?? 0)} fileId={activeId ?? ""} startByte={status?.startByte ?? 0} />
            ) : (
              <div className="q-diff-body">
                {stagedEdits.length === 0 ? (
                  <div className="q-diff-empty">No staged edits yet. Turn on Edit and change the text.</div>
                ) : (
                  stagedEdits.map((e, i) => (
                    <div className="q-diff-item" key={i}>
                      <div className="q-diff-loc">line ~{e.line} · 0x{e.start.toString(16)}</div>
                      <div className="q-diff-old">- {snippet(e.old)}</div>
                      <div className="q-diff-new">+ {snippet(e.new)}</div>
                    </div>
                  ))
                )}
              </div>
            )}
          </div>
        )}

        {toolsOpen && activeTab && (
          <Tools
            key={activeTab.fileId}
            fileId={activeTab.fileId}
            detected={activeTab.meta.detected}
            analysis={activeId ? analyses[activeId] ?? null : null}
            otherFiles={tabs.filter((t) => t.fileId !== activeTab.fileId).map((t) => ({ id: t.fileId, name: t.meta.path.replace(/^.*[\\/]/, ""), detected: t.meta.detected }))}
            onAnalyze={analyze}
            onNotice={setNotice}
            onError={setError}
            onClose={() => setToolsOpen(false)}
          />
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
      </div>

      <footer className="q-status">
        {status && activeTab ? (
          <>
            <span className="q-file" title={activeTab.meta.path}>{activeTab.meta.path}</span>
            <span className="q-sep" />
            <span>{fmtBytes(activeTab.meta.size)}</span>
            <span>{activeTab.meta.encoding || "?"}</span>
            <span>{activeTab.meta.detected || "plain"}</span>
            <span className="q-spacer" />
            {following && <span className="q-edit-flag" title="Following file tail">● live</span>}
            {editMode && <span className="q-edit-flag">{dirty ? "● editing" : "editing"}</span>}
            {hasEdits && (
              <span className="q-edit-flag">
                {editCount} staged{staging && !staging.inPlaceEligible ? " · len±" + staging.netDelta : ""}
              </span>
            )}
            <span>0x{status.startByte.toString(16)}–0x{status.endByte.toString(16)}</span>
            <span>lines {status.firstLine}–{status.lastLine}{status.approx ? " ~" : ""}</span>
            {status.atBof && <span className="q-edge">BOF</span>}
            {status.atEof && <span className="q-edge">EOF</span>}
          </>
        ) : (
          <span className="q-muted">Ready</span>
        )}
      </footer>

      {paletteOpen && <CommandPalette commands={commands} onClose={() => setPaletteOpen(false)} />}
    </div>
  );
}

export default App;
