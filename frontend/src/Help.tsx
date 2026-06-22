import { useEffect, useState, type ReactNode } from "react";

// In-app help. Self-contained topic pages (no markdown runtime) shown in a
// modal with a topic list on the left. Keep content in sync with docs/.

const Kbd = ({ k }: { k: string }) => <kbd className="q-help-kbd">{k}</kbd>;

interface Row {
  left: ReactNode;
  right: ReactNode;
}

const Defs = ({ rows }: { rows: Row[] }) => (
  <dl className="q-help-defs">
    {rows.map((r, i) => (
      <div className="q-help-def" key={i}>
        <dt>{r.left}</dt>
        <dd>{r.right}</dd>
      </div>
    ))}
  </dl>
);

interface Topic {
  id: string;
  title: string;
  body: ReactNode;
}

const TOPICS: Topic[] = [
  {
    id: "overview",
    title: "Overview",
    body: (
      <>
        <p>
          Quarry is an editor and toolkit for <strong>very large</strong> text
          files — multi-gigabyte SQL dumps, CSV/TSV exports, and logs that
          ordinary editors can't open.
        </p>
        <p>
          It never reads the whole file into memory. The editor holds only
          bounded <em>windows</em> that stream in as you scroll, so a 4&nbsp;KB
          file and a 400&nbsp;GB file cost about the same. Indexing and analysis
          run in the background and are cancellable.
        </p>
        <p className="q-help-note">
          <strong>Your source is never silently changed.</strong> Every
          transform and export writes a new file. The only in-place write is the
          explicit “Patch in place”, which keeps a backup.
        </p>
      </>
    ),
  },
  {
    id: "shortcuts",
    title: "Keyboard shortcuts",
    body: (
      <Defs
        rows={[
          { left: <Kbd k="Ctrl+O" />, right: "Open a file" },
          { left: <Kbd k="Ctrl+P" />, right: "Command palette (every action, plus jump-to-table)" },
          { left: <Kbd k="Ctrl+F" />, right: "Find — next/prev, case, whole-word, regex, list all" },
          { left: <Kbd k="Ctrl+G" />, right: "Go to a line, a 0xHEX byte offset, or a percentage (e.g. 50%)" },
          { left: <Kbd k="Ctrl+B" />, right: "Toggle the file sidebar" },
          { left: <Kbd k="Ctrl+W" />, right: "Close the current tab" },
          { left: <Kbd k="F1" />, right: "Open this help" },
          { left: <Kbd k="Esc" />, right: "Close the open overlay (find, palette, help…)" },
        ]}
      />
    ),
  },
  {
    id: "open-view",
    title: "Opening & viewing",
    body: (
      <>
        <p>Open a file via the toolbar, a pasted path, drag-and-drop, or the File menu’s recent list. The previous session’s files are restored on launch.</p>
        <Defs
          rows={[
            { left: "Plain text", right: "The editor surface with syntax highlighting (20+ languages, SQL, rainbow CSV)." },
            { left: "Hex view", right: "Windowed hex/ASCII for binary or encoding checks." },
            { left: "Grid view", right: "A spreadsheet grid for CSV/TSV, parsed per window." },
            { left: "File X-ray", right: "A minimap rail of analyzed regions (SQL tables); click to seek." },
            { left: "Bookmarks", right: "Mark byte offsets and jump back; stored per file." },
            { left: "Follow tail", right: "For a growing log: reload and jump to the new end (auto-stops on edit)." },
            { left: "Byte offsets", right: "Per-line offset gutter is optional — View → “Byte offsets in gutter”." },
            { left: "Theme", right: "Light/dark toggle in the View menu; remembered across sessions." },
          ]}
        />
      </>
    ),
  },
  {
    id: "editing",
    title: "Editing & saving",
    body: (
      <>
        <p>Turn on <strong>Edit mode</strong> (the file must be editable), make changes in the current window, then save one of two ways:</p>
        <Defs
          rows={[
            { left: "Patch in place", right: "Enabled only when edits keep the total length unchanged. Overwrites just the changed bytes after writing a backup — ideal for a one-token fix in a 400 GB dump." },
            { left: "Save copy…", right: "Streams a full edited copy to a new file (used whenever length changes)." },
            { left: "Diff panel", right: "Review staged edits as a list or side-by-side before saving." },
          ]}
        />
      </>
    ),
  },
  {
    id: "sql",
    title: "SQL tools",
    body: (
      <>
        <p>Open the data-tools panel on a <code>.sql</code> file. Most tools need <strong>Analyze dump</strong> first (it finds tables and their byte ranges in one streaming pass).</p>
        <Defs
          rows={[
            { left: "Analyze dump", right: "Discover tables, CREATE/INSERT counts, DEFINER clauses, charsets/collations." },
            { left: "Extract", right: "Write a table’s whole content, schema only (DDL), or data only (INSERTs)." },
            { left: "Split by table", right: "One .sql file per table into a folder, with a manifest." },
            { left: "Dev fixture", right: "A tiny dump: each table’s DDL + only its first N rows." },
            { left: "Lint dump", right: "Re-import hazards: empty tables, DEFINER, mixed charsets, largest tables." },
            { left: "Reshape INSERTs", right: "Explode extended INSERTs to one row each (line-diff friendly) or batch them back (faster import)." },
            { left: "Schema diff", right: "Compare table/column structure against another open, analyzed dump." },
            { left: "Find/replace · Presets", right: "Streaming replace to a new file; presets cover DEFINER strip, charset/db rename." },
          ]}
        />
      </>
    ),
  },
  {
    id: "csv",
    title: "CSV tools",
    body: (
      <>
        <p>Open the data-tools panel on a <code>.csv</code>/<code>.tsv</code> file. The delimiter and header are auto-detected; override them at the top of the panel.</p>
        <Defs
          rows={[
            { left: "CSV → SQL", right: "Choose table name, include/rename/type each column, insert mode, batch size, NULL tokens; preview before converting." },
            { left: "Column transforms", right: "Drop, reorder/project, or append a constant column." },
            { left: "Filter / dedupe / sample", right: "Keep matching rows; drop duplicates by key or whole row; keep every Nth row." },
            { left: "Redact / anonymize", right: "Mask columns for sharing: blank, fixed, stable hash, or email-style." },
            { left: "Profile columns", right: "Null %, distinct, min/max, top values, ragged rows (sampled)." },
            { left: "Export", right: "JSONL, SQLite (.db), or Excel (.xlsx); or copy the preview as a Markdown table." },
          ]}
        />
      </>
    ),
  },
  {
    id: "tips",
    title: "Tips",
    body: (
      <ul className="q-help-tips">
        <li>Long transforms and exports run as <strong>cancellable jobs</strong> — a progress toast shows row counts with a Cancel button.</li>
        <li><strong>Harvest</strong> (the ⤓ in Find) streams every regex match to a new file, one per line — great for pulling IDs or emails out of a dump.</li>
        <li><strong>Go to</strong> accepts <code>50%</code> to jump to the middle of a huge file instantly.</li>
        <li>The command palette (<Kbd k="Ctrl+P" />) lists every command and, for an analyzed dump, every table — type a name to jump to it.</li>
        <li>Outputs are written to a temp file and atomically renamed, so a cancelled or failed run never leaves a half-written file or destroys an existing one.</li>
      </ul>
    ),
  },
];

export default function Help({ onClose }: { onClose: () => void }) {
  const [sel, setSel] = useState(0);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.preventDefault(); onClose(); }
      else if (e.key === "ArrowDown") { e.preventDefault(); setSel((i) => Math.min(i + 1, TOPICS.length - 1)); }
      else if (e.key === "ArrowUp") { e.preventDefault(); setSel((i) => Math.max(i - 1, 0)); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="q-cmd-backdrop" onMouseDown={onClose}>
      <div className="q-help" onMouseDown={(e) => e.stopPropagation()}>
        <div className="q-help-head">
          <span className="q-help-title">Quarry help</span>
          <span className="q-spacer" />
          <button className="q-icon" title="Close (Esc)" onClick={onClose}>×</button>
        </div>
        <div className="q-help-body">
          <nav className="q-help-nav">
            {TOPICS.map((t, i) => (
              <button
                key={t.id}
                className={"q-help-navitem" + (i === sel ? " q-help-navitem-sel" : "")}
                onClick={() => setSel(i)}
              >
                {t.title}
              </button>
            ))}
          </nav>
          <div className="q-help-content">
            <h2 className="q-help-h">{TOPICS[sel].title}</h2>
            {TOPICS[sel].body}
          </div>
        </div>
      </div>
    </div>
  );
}
