# Features & tools

This is the working reference for everything Quarry can do. Tools that produce
output always write a **new** file (temp file + atomic rename); the source is
never modified. The exception is *patch in place*, described under
[Editing](#editing).

## Editor & views

- **Streaming editor** — only bounded, line-aligned windows are held in memory;
  scrolling pulls in adjacent windows. The gutter shows real (or approximate,
  until indexed) line numbers plus byte offsets.
- **Syntax highlighting** — 20+ programming languages, plus JSON/YAML/TOML/INI
  and other configs, dedicated SQL highlighting, and **rainbow CSV** (per-column
  colors). Highlighting is chosen from the detected type and file extension.
- **Hex view** — windowed hex/ASCII for binary files or encoding checks.
- **Grid view** — a spreadsheet grid for CSV/TSV, parsed per window.
- **Light / dark theme** — toggle from the View menu or palette; applies to the
  app chrome and the editor, and is remembered across sessions.

### Editing

- **Edit mode** stages changes against the current window.
- **Patch in place** — enabled only when staged edits keep the file's total
  length unchanged; it overwrites just the affected bytes and writes a backup
  first. Ideal for fixing a charset token or a value in a 400 GB dump without
  rewriting it.
- **Save copy…** — streams a full edited copy to a new file (used whenever
  length changes).
- **Diff panel** — review staged edits as a list or side-by-side.

## Search & navigation

- **Find** (Ctrl+F) — next/prev, match-case, whole-word, and regex. Search is
  **encoding-aware**: the query is encoded into the file's encoding before
  scanning, so a UTF-8 query still matches a UTF-16/Windows-125x dump.
- **Results panel** — list all matches across the whole file with line previews;
  click to jump.
- **Harvest** — stream every regex match to a new file (one per line) — e.g.
  pull all emails or IDs out of a dump.
- **Command palette** (Ctrl+P) — fuzzy access to every command, including
  jump-to-table for analyzed SQL dumps.
- **Go to** (Ctrl+G) — accepts a **line number**, a **0xHEX byte offset**, or a
  **percentage** (`50%`) of the file.
- **File X-ray** — a minimap rail of analyzed regions (SQL tables); click to
  seek to a region.
- **Bookmarks** — mark the current position and jump back; stored per file.
- **Follow tail** — for a growing file (e.g. an active log), poll for growth,
  reload, and jump to the new end. Auto-stops if you start editing.

## SQL workbench

Open the data tools panel on a `.sql` file.

- **Analyze dump** — a single streaming pass that discovers tables (with byte
  offsets and approximate sizes), counts CREATE/INSERT tables, DEFINER clauses,
  and charsets/collations. The lexer is quote/comment-aware and carries state
  across chunks, so blob contents can't produce phantom tables. Results feed the
  table list, the command palette, and the X-ray.
- **Extract** per table — whole table, **schema only** (DDL), or **data only**
  (INSERTs) — streamed to a chosen file.
- **Split by table** — one `.sql` file per table into a folder, with a manifest.
- **Dev fixture** — a tiny shareable dump: each table's DDL plus only its first
  *N* INSERT rows (tuple-aware sampling, never materializes the whole file).
- **Lint dump** — reports re-import hazards from the cached analysis: empty
  tables, DEFINER clauses, mixed charsets/collations, and the largest tables.
- **Find / replace → new file** — plain or regex, case-insensitive, whole-word;
  also used for table-prefix renames.
- **Cleanup presets** — common fixes (strip DEFINER, change database name,
  change charset/collation, …) applied as a streaming replace.
- **Reshape INSERTs** — *explode* extended multi-row INSERTs into one row per
  statement (so a line diff between two dumps is meaningful), or *batch*
  single-row INSERTs back into extended ones (faster re-import). Comment- and
  quote-aware; non-INSERT statements pass through unchanged.
- **Schema diff** — compare the table/column structure of two analyzed dumps:
  added/removed tables, and per-table added/removed/type-changed columns.
  Table and column matching are case-insensitive; backtick-quoted columns named
  like keywords (`` `key` ``) are handled correctly.

## CSV workbench

Open the data tools panel on a `.csv`/`.tsv` file.

- **Delimiter inspect** — detect the most likely delimiter (with ranked
  alternatives) and whether the first row is a header.
- **Schema & preview** — infer column names and SQL types over a bounded sample;
  preview rows; or open the full **grid** view.
- **CSV → SQL** (convertcsv-style) — choose the table name, include/rename/type
  each column, pick the insert mode (`INSERT` / `INSERT IGNORE` / `REPLACE`),
  batch size, NULL tokens, and an invalid-byte policy; preview the generated SQL
  before converting the whole file.
- **Column transforms** — drop a column, reorder/project columns, or append a
  constant column.
- **Filter / dedupe / sample**
  - *Filter*: keep rows where a column matches `eq` / `ne` / `contains` /
    `gt` / `lt` / `empty` / `nonempty` (with invert).
  - *Dedupe*: by a key column or the whole row; keyed on exact bytes, so ragged
    rows and hash collisions don't silently drop data.
  - *Sample*: keep every *N*th data row (header preserved).
- **Redact / anonymize** — mask chosen columns for sharing: blank, a fixed
  value, a stable short hash (pseudonym), or email-style `a***@domain`.
- **Profile columns** — over a bounded sample: null %, distinct count, min/max,
  top values, and ragged-row count.

## Export bridges

From the CSV workbench:

- **JSONL** — newline-delimited JSON, one object per row; header cells become
  keys (or `col1…` without a header). Optional numeric typing keeps leading-zero
  IDs and over-`int64` integers as strings and never emits NaN/Inf.
- **SQLite** — a real `.db` with one table, via the pure-Go driver; numeric
  cells keep their storage class, written in batched transactions.
- **Excel (.xlsx)** — via a streaming writer (memory-bounded); reports when the
  ~1,048,576-row sheet limit truncates the output.
- **Copy preview as Markdown** — the current preview as a GitHub-flavored
  Markdown table, to the clipboard.

## Background jobs

Long transforms and exports run as **cancellable background jobs**:

- A progress toast shows the operation and a live row count, with a **Cancel**
  button. Cancelling aborts the streaming operation and removes its partial
  output.
- One job runs at a time; a second is rejected with a clear message so the first
  stays cancellable.
