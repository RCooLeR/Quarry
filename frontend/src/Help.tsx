import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { MAX_PALETTE_TABLES } from "./productLimits";

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

const FOCUSABLE =
  'button:not([disabled]):not([tabindex="-1"]), input:not([disabled]):not([tabindex="-1"]), [href], [tabindex]:not([tabindex="-1"])';

const TOPICS: Topic[] = [
  {
    id: "overview",
    title: "Overview",
    body: (
      <>
        <p>
          Quarry is an editor and toolkit for <strong>very large</strong> text
          files — including multi-gigabyte SQL dumps, CSV/TSV exports, and
          logs that ordinary editors cannot load safely.
        </p>
        <p>
          The primary viewer requests bounded <em>windows</em> as you scroll
          instead of sending the whole source file to the WebView. Indexes,
          caches, and result sets have explicit caps; selected tools use bounded
          samples, while whole-file scans and exports stream under operation
          budgets. Large-file performance still depends on the file shape,
          storage, and platform, and long operations are cancellable only where
          their job contract exposes cancellation.
        </p>
        <p className="q-help-note">
          <strong>Edited saves are copy-only.</strong> In-place save is
          unavailable until Quarry can retain and verify a durable backup. Use
          a destination distinct from every source when running data tools.
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
          { left: <Kbd k="Ctrl/Cmd+O" />, right: "Open a file" },
          { left: <Kbd k="Ctrl/Cmd+P" />, right: `Command palette (every action, plus up to ${MAX_PALETTE_TABLES} analyzed-table jumps)` },
          { left: <Kbd k="Ctrl/Cmd+F" />, right: "Find — next/prev, ASCII-insensitive or match-case, UTF-8 whole-word, bounded regex, list all" },
          { left: <Kbd k="Ctrl/Cmd+G" />, right: "Go to a line, a 0xHEX byte offset, or a percentage (e.g. 50%)" },
          { left: <Kbd k="Ctrl/Cmd+B" />, right: "Toggle the file sidebar" },
          { left: <Kbd k="Ctrl/Cmd+W" />, right: "Close the current tab" },
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
        <p>Open a file via the toolbar, a pasted path, drag-and-drop, or the File menu’s recent list. Path memory and session restore are off by default; enable each explicitly in the File menu.</p>
        <Defs
          rows={[
            { left: "Plain text", right: "The editor surface with syntax highlighting (20+ languages, SQL, rainbow CSV)." },
            { left: "Hex view", right: "Windowed hex/ASCII for binary or encoding checks." },
            { left: "Grid view", right: "A spreadsheet grid for CSV/TSV, parsed per window." },
            { left: "File X-ray", right: "A minimap rail of analyzed regions (SQL tables); click to seek." },
            { left: "Bookmarks", right: "Mark byte offsets and jump back; kept for this run unless workspace path memory is enabled." },
            { left: "Workspace privacy", right: "File menu controls path memory, automatic restore, and clearing recent/session/bookmark data without closing current tabs." },
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
        <p>Turn on <strong>Edit mode</strong> (the file must be editable), make changes in the current window, then save or discard them:</p>
        <Defs
          rows={[
            { left: "Save copy…", right: "Streams a full edited copy to a new file. The source is never overwritten." },
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
            { left: "Analyze dump", right: "Discover supported top-level CREATE/INSERT/REPLACE regions and DEFINER counts. Ambiguous client, raw-payload, procedural, or dialect syntax fails without caching partial regions." },
            { left: "Extract", right: "Write only the analyzed CREATE/INSERT/REPLACE regions for a table. Session preamble, ALTER/DROP, triggers, and other DML are omitted, so add required SQL before re-import." },
            { left: "Split by table", right: "Write those analyzed regions to one .sql file per table plus a manifest; the slices are not claimed to be standalone dumps." },
            { left: "Dev fixture", right: "A tiny dump: each table’s DDL + only its first N rows." },
            { left: "Lint dump", right: "Re-import hazards: empty tables, DEFINER, mixed charsets, largest tables." },
            { left: "Reshape INSERTs", right: "Opt-in statement regrouping for diffs/import size. It can change trigger, atomicity, rollback, or other statement-level behavior; use only when the target database semantics allow it." },
            { left: "Schema diff", right: "Compare table/column structure against another open, analyzed dump." },
            { left: "Find/replace", right: "Plain replacement writes a new copy and updates SQL string values only. Native PHP/WordPress serialized byte lengths are recalculated; comments, identifiers, routine bodies, regex, and opaque encoded payloads are not rewritten." },
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
        <p>Open the data-tools panel on a <code>.csv</code>/<code>.tsv</code> file. Ranked separator candidates and bounded-sample warnings are shown. Separator/header overrides belong to the current file and are shared by its editor, grid, preview, and transforms.</p>
        <Defs
          rows={[
            { left: "CSV → SQL", right: "Choose table name, include/rename/type each column, insert mode, batch size, NULL tokens; preview before converting." },
            { left: "Column transforms", right: "Drop one selected column or append a constant field to every record. If a header is enabled, the constant is also its new header cell." },
            { left: "Filter / dedupe / sample", right: "Keep matching rows; drop duplicates by key or whole row; keep every Nth row." },
            { left: "Mask / pseudonymize", right: "Create a separate CSV with blank, fixed, email-style, or operation-keyed pseudonym replacements. Pseudonyms intentionally change between exports." },
            { left: "Profile columns", right: "Null %, distinct, min/max, top values, ragged rows (sampled)." },
            { left: "Export", right: "Write JSONL with explicit numeric-typing control to a new file, or copy the bounded preview as a Markdown table." },
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
        <li><strong>Harvest</strong> (the ⤓ in Find) scans a UTF-8 source with a bounded regular expression and streams supported matches to a new file, one per line.</li>
        <li><strong>Go to</strong> accepts <code>50%</code> and seeks by byte offset without loading the whole source into the editor.</li>
        <li>The command palette (<Kbd k="Ctrl/Cmd+P" />) lists every command and up to the first {MAX_PALETTE_TABLES} analyzed tables — type a name to jump to one in that bounded list.</li>
        <li>Choose a new output name distinct from every open source. Do not treat an output as complete until Quarry reports success.</li>
      </ul>
    ),
  },
];

interface Props {
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
}

export default function Help({ onClose, returnFocusTo }: Props) {
  const [sel, setSel] = useState(0);
  const dialogRef = useRef<HTMLDivElement>(null);
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const restoreFocusRef = useRef<HTMLElement | null>(
    returnFocusTo ?? (typeof document !== "undefined" && document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null),
  );

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    tabRefs.current[0]?.focus();
    return () => {
      const previous = restoreFocusRef.current;
      const restore = () => {
        const active = document.activeElement;
        if (
          previous?.isConnected &&
          (active === document.body || (active instanceof Node && dialog?.contains(active)))
        ) {
          previous.focus({ preventScroll: true });
        }
      };
      restore();
      // Chromium may retain `inert` on the workbench until the modal-closing
      // commit completes. Retry after that commit without overriding focus an
      // action deliberately moved elsewhere.
      queueMicrotask(restore);
    };
  }, []);

  const selectAndFocus = (index: number) => {
    const next = Math.max(0, Math.min(TOPICS.length - 1, index));
    setSel(next);
    tabRefs.current[next]?.focus();
  };

  const trapTab = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const focusable = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
    if (focusable.length === 0) {
      event.preventDefault();
      dialogRef.current?.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && (document.activeElement === first || !dialogRef.current?.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (document.activeElement === last || !dialogRef.current?.contains(document.activeElement))) {
      event.preventDefault();
      first.focus();
    }
  };

  const topic = TOPICS[sel];

  return (
    <div
      className="q-cmd-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        className="q-help"
        role="dialog"
        aria-modal="true"
        aria-labelledby="q-help-title"
        tabIndex={-1}
        onKeyDown={(event) => {
          // Keep global workbench shortcuts from acting behind the modal.
          event.stopPropagation();
          if (event.key === "Tab") {
            trapTab(event);
          } else if (event.key === "Escape") {
            event.preventDefault();
            onClose();
          }
        }}
      >
        <div className="q-help-head">
          <h1 id="q-help-title" className="q-help-title">Quarry help</h1>
          <span className="q-spacer" />
          <button type="button" className="q-icon" title="Close (Esc)" aria-label="Close help" onClick={onClose}>×</button>
        </div>
        <div className="q-help-body">
          <nav className="q-help-nav" role="tablist" aria-label="Help topics" aria-orientation="vertical">
            {TOPICS.map((t, i) => (
              <button
                ref={(element) => { tabRefs.current[i] = element; }}
                type="button"
                key={t.id}
                id={`q-help-tab-${t.id}`}
                role="tab"
                aria-selected={i === sel}
                aria-controls="q-help-panel"
                tabIndex={i === sel ? 0 : -1}
                className={"q-help-navitem" + (i === sel ? " q-help-navitem-sel" : "")}
                onClick={() => setSel(i)}
                onKeyDown={(event) => {
                  if (event.key === "ArrowDown" || event.key === "ArrowRight") {
                    event.preventDefault();
                    selectAndFocus((i + 1) % TOPICS.length);
                  } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
                    event.preventDefault();
                    selectAndFocus((i - 1 + TOPICS.length) % TOPICS.length);
                  } else if (event.key === "Home") {
                    event.preventDefault();
                    selectAndFocus(0);
                  } else if (event.key === "End") {
                    event.preventDefault();
                    selectAndFocus(TOPICS.length - 1);
                  }
                }}
              >
                {t.title}
              </button>
            ))}
          </nav>
          <div
            id="q-help-panel"
            className="q-help-content"
            role="tabpanel"
            aria-labelledby={`q-help-tab-${topic.id}`}
            tabIndex={0}
          >
            <h2 className="q-help-h">{topic.title}</h2>
            {topic.body}
          </div>
        </div>
      </div>
    </div>
  );
}
