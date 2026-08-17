# Features & tools

This is the working reference for everything Quarry can do. Edited saves are
currently copy-only. Enabled artifact-producing tools require a new destination
that is distinct from every open source and publish through the shared,
no-clobber output transaction; unsupported output paths fail closed before a
destination prompt.

## Editor & views

- **Streaming editor** — only bounded, line-aligned windows are held in memory;
  scrolling pulls in adjacent windows. The gutter shows real (or approximate,
  until indexed) line numbers, with an **optional per-line byte-offset column**
  (View → *Byte offsets in gutter*, off by default; persisted). The current
  window's byte range is always shown in the status bar regardless.
- **Syntax highlighting** — 20+ programming languages, plus JSON/YAML/TOML/INI
  and other configs, dedicated SQL highlighting, and **rainbow CSV** (per-column
  colors). Highlighting is chosen from the detected type and file extension.
- **Hex view** — windowed hex/ASCII for binary files or encoding checks.
- **Grid view** — a spreadsheet grid for CSV/TSV, parsed per window.
- **Light / dark theme** — toggle from the View menu or palette; applies to the
  app chrome and the editor, and is remembered across sessions.

### Editing

- **Edit mode** stages changes against the current window.
- **Save copy…** — streams a full edited copy to a new file. In-place save is
  disabled until its transaction retains and verifies a durable backup.
- **Diff panel** — review staged edits as a list or side-by-side.

## Search & navigation

- **Find** (Ctrl+F) — next/prev, match-case, whole-word, and regex. Search is
  streaming and chunk-aware. Plain queries are encoded into the source
  encoding; UTF-16 matches are restricted to aligned code-unit offsets.
  Case-insensitive plain search supports ASCII terms. A non-ASCII insensitive
  term is rejected explicitly instead of returning a misleading "not found"
  result. Whole-word mode is available for UTF-8 text; fixed-width and legacy
  encodings require exact plain search. Exact regex search is UTF-8-only and
  accepts only patterns with a finite maximum width contained by the configured
  match window.
- **Results panel** — scan across the whole file and list at most 1,000 matches
  with bounded line previews; a `+` after the count reports that more matches
  exist beyond the displayed cap. Click a result to jump.
- **Harvest** — scan a UTF-8 source with the bounded-regex policy and stream
  each supported match to a new file (one per line), for example to extract
  emails or IDs from a dump.
- **Command palette** (Ctrl+P) — fuzzy access to every command, plus
  jump-to-table entries for at most the first 300 tables in an analyzed SQL
  dump. Use the bounded, paged SQL table list for tables beyond that
  convenience-command cap.
- **Go to** (Ctrl+G) — accepts a **line number**, a **0xHEX byte offset**, or a
  **percentage** (`50%`) of the file.
- **File X-ray** — a minimap rail of analyzed regions (SQL tables); click to
  seek to a region.
- **Bookmarks** — mark the current position and jump back. They are kept only
  for the running app unless workspace path memory is explicitly enabled.
- **Workspace privacy** — local path memory and automatic session restore are
  separate, opt-in File-menu preferences. Disabling path memory clears recent,
  session, and bookmark path records; the clear action leaves current tabs open.
  Restored sessions are schema-validated and capped at 16 paths.
- **Follow tail** — for a growing file (e.g. an active log), poll for growth,
  reload, and jump to the new end. Auto-stops if you start editing.
- **In-app help** — press **F1** (or Help → *Help & shortcuts…*) for a built-in
  guide to the shortcuts and every tool.

## SQL workbench

Open the data tools panel on a `.sql` file.

- **Analyze dump** — a single streaming pass that discovers supported top-level
  CREATE/INSERT/REPLACE table regions (with exact byte offsets) and counts
  DEFINER clauses. Charset/collation tokens feed the lint report; the summary
  does not claim to be a complete per-table charset inventory. The fixed-state
  lexer carries state across chunks and fails without a partial cache when
  client commands, raw payloads, procedural bodies, or unsupported dialect
  syntax make statement ownership ambiguous.
- **Extract** per table — analyzed CREATE/INSERT/REPLACE regions, **schema
  only** (CREATE regions), or **data only** (INSERT/REPLACE regions), streamed
  to a chosen file. Session preamble, ALTER/DROP, triggers, and other DML are
  omitted; these outputs are not promised to be standalone imports until the
  required surrounding SQL is added.
- **Split by table** — those analyzed regions in one `.sql` file per table,
  plus a manifest that records the same limitation.
- **Dev fixture** — a tiny shareable dump: each table's DDL plus only its first
  *N* INSERT rows (tuple-aware sampling, never materializes the whole file).
- **Lint dump** — reports re-import hazards from the cached analysis: empty
  tables, DEFINER clauses, mixed charsets/collations, and the largest tables.
- **Find / replace** writes a new SQL copy and updates SQL string values only.
  Plain replacement decodes SQL string literals before matching and
  recalculates native PHP/WordPress serialized byte lengths before re-escaping
  the literal. Comments, identifiers, routine bodies, opaque encoded payloads,
  malformed/unsupported serialized values, and regex replacement fail closed or
  remain unchanged instead of publishing a speculative rewrite.
- **Cleanup presets are unavailable** until they use a token-aware SQL
  transformation. Generic byte replacement can alter matching text inside
  comments and string literals, so it is not presented as structural cleanup.
- **Reshape INSERTs** — *explode* extended multi-row INSERTs into one row per
  statement (so a line diff between two dumps is meaningful), or *batch*
  single-row INSERTs back into extended ones. This is an explicit statement-
  regrouping transform, not a semantic-equivalence guarantee: statement-level
  triggers, atomicity, rollback, and database-specific side effects may change.
  Unsupported lexical, procedural, and client-managed payload forms fail closed.
- **Schema diff** — compare the table/column structure of two analyzed dumps:
  added/removed tables, and per-table added/removed/type-changed columns.
  Table and column matching are case-insensitive; backtick-quoted columns named
  like keywords (`` `key` ``) are handled correctly.

## CSV workbench

Open the data tools panel on a `.csv`/`.tsv` file.

- **Delimiter inspect** — detect the most likely delimiter (with ranked
  alternatives) and whether the first row is a header. Quarry distinguishes
  detector output from an explicit override and keeps that dialect per open
  file; editor coloring, grid parsing, previews, and transforms use the same
  current separator/header configuration.
- **Schema & preview** — infer column names and SQL types over a bounded sample;
  preview rows; or open the windowed **grid** view. Detection, truncation,
  partial-record, and schema/preview warnings are shown above the controls.
- **CSV → SQL** (convertcsv-style) — choose the table name, include/rename/type
  each column, pick the insert mode (`INSERT` / `INSERT IGNORE` / `REPLACE`),
  batch size, NULL tokens, and an invalid-byte policy; preview the generated SQL
  before converting the whole file. Generated MySQL uses standard doubled
  apostrophes for safe printable ASCII and explicit `utf8mb4` hex conversion
  for backslashes, controls, and Unicode, so import does not depend on
  `NO_BACKSLASH_ESCAPES` or the connection character set. INSERT batches flush
  at the configured row count or before their complete encoded statement would
  exceed the 64 MiB application ceiling; a single larger encoded row fails
  without publishing an output file.
- **Column transforms** — drop one selected column or append a constant field
  to every record. When the header option is enabled, the same constant is the
  new header cell. CSV→SQL has separate include/rename/type controls for each
  detected source column; the new-CSV transform does not reorder columns.
- **Filter / dedupe / sample**
  - *Filter*: keep rows where a column matches `eq` / `ne` / `contains` /
    `gt` / `lt` / `empty` / `nonempty` (with invert).
  - *Dedupe*: by a key column or the whole row; keyed on exact bytes, so ragged
    rows and hash collisions don't silently drop data.
  - *Sample*: keep every *N*th data row (header preserved).
- **Mask / pseudonymize** — create a separate CSV with chosen columns replaced
  by blank, a fixed value, email-style `a***@domain`, or a keyed 128-bit
  pseudonym. A new random key is generated for each export, so pseudonyms are
  stable only within that output and intentionally cannot be linked across runs.
- **Profile columns** — over a bounded sample: null %, distinct count, min/max,
  top values, and ragged-row count.

Full CSV transforms and SQL conversion parse quote structure strictly. Malformed
records fail without publishing an output file; Quarry does not silently repair
CSV quoting as a side effect of another operation.

## Export bridges

From the CSV workbench:

- **JSONL** — newline-delimited JSON, one object per row; header cells become
  keys (or `col1…` without a header). The UI's numeric-typing control keeps leading-zero
  IDs and over-`int64` integers as strings and never emits NaN/Inf.
- **Copy preview as Markdown** — the current preview as a GitHub-flavored
  Markdown table, to the clipboard.

SQLite and Excel export are intentionally unavailable in this release. Their
libraries require mutable named scratch files, which do not yet meet Quarry's
no-clobber and handle-bound publication guarantees. The service entry points
fail before opening a save dialog or touching the filesystem.

## Background jobs

Long transforms and exports run as **cancellable background jobs**:

- A progress toast shows the operation and a live row count, with a **Cancel**
  button. Cancelling aborts the streaming operation and removes its partial
  output.
- One job runs at a time; a second is rejected with a clear message so the first
  stays cancellable.
