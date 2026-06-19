# Architecture

Quarry is three layers:

1. a **streaming engine** in `internal/` with no UI dependencies,
2. a **binding layer** (`fileservice*.go`, `jobs.go`, `main.go`) that exposes the
   engine to the frontend over Wails, and
3. a **React UI** in `frontend/src/` whose editor materializes only bounded
   windows.

The guiding constraint is that **no operation loads a whole file into memory**.
Everything is windowed, streamed, or sampled.

## The streaming document

`internal/document.FileDocument` is the core. On open it stats the file and
keeps an `*os.File`; it does **not** read the contents. Reads go through:

- `ReadAt` / `ReadRange(start, end)` — bounded slices, served from an LRU
  **chunk cache** (`chunk_cache.go`) that loads fixed-size chunks on demand.
  `ReadRange` enforces a bounded-read cap so a single call can't accidentally
  pull a multi-gigabyte span.
- `Size()` and `ApproxOffsetToLine` / line bookkeeping backed by a **sparse line
  index** (`internal/lineindex`).

The line index is built in the **background** (`StartIndexing(ctx)`), and it is
cancellable. Navigation by byte offset works immediately; line numbers start
*approximate* and become *exact* once indexing completes. A sequential
read-ahead hint is issued on huge-file opens to keep scrolling smooth.

`OriginalFileState` / `CurrentFileState` (`external.go`) capture size + mtime so
the app can detect a file that changed on disk (used by Save copy and by
follow-tail / refresh).

## The editor surface

`frontend/src/editor/QuarryEditor.ts` wraps **CodeMirror 6**. It holds a single
**window** of text at a time:

- Navigation calls `loadAt(byte, …)` to fetch a line-aligned window from the
  backend. Viewport-edge detection plus a wheel handler swap in the adjacent
  window as you approach the top/bottom, so scrolling feels continuous over a
  file far larger than the document.
- **Highlighting is per-visible-line** (a `ViewPlugin` + `RangeSetBuilder`)
  rather than a whole-file parser. A collapsed/truncated huge line (a mysqldump
  extended INSERT can be one enormous "line") therefore can't bleed colors
  across the viewport.
- Behavior is swapped through CodeMirror **compartments**: read-only vs editable,
  the language profile (`highlight.ts` / `sqlHighlight.ts`), and the
  light/dark theme.

Huge single lines are displayed truncated (with an ellipsis marker) so one giant
line renders as a compact row instead of breaking layout.

## Editing & saving

Edits are staged, not written immediately:

- `internal/manualedit.Session` records edits as `(offset, oldLen, newBytes)`
  against the original file, tracking the net length delta.
- **Patch in place** (`internal/inplace`) is used **only when the net delta is
  zero** — the edit overwrites just the changed byte ranges. It writes a sidecar
  backup first and can recover a half-applied patch on the next open, so a crash
  mid-write can't corrupt the file.
- **Save copy** streams a full edited copy to a new file (the only correct option
  when length changes). It guards against the source changing underneath it
  mid-session.

This is why the UI only offers *patch in place* for length-preserving edits and
falls back to *save copy* otherwise.

## The binding layer

`FileService` (package `main`, split across `fileservice*.go`) is the single
Wails-bound service. It exposes windowed reads, search, editing/staging, and the
CSV/SQL tools as methods that take and return **JSON-friendly structs**. The
whole-file content never crosses the bridge — only bounded windows and bounded
samples do.

`wails3 generate bindings` regenerates the TypeScript client into
`frontend/bindings/` (gitignored) whenever a Go method signature changes.

`main.go` wires the Wails application: a single window with a dark custom title
bar (a Windows `COLORREF` theme), the embedded app icon, and
`EnableFileDrop` plus a small Go→frontend event bridge so dropped file paths are
opened by the UI.

## Background jobs

`jobs.go` holds a tiny job manager. `withJob(title, fn)` runs a transform inside
a cancellable `context`, registers it as the single active job, and emits
`quarry:job-start` / `-progress` / `-end` application events; the streaming
functions report row-count progress through a callback. `CancelJob` flips the
context so the transform aborts and removes its partial output. The frontend
renders a progress toast with a Cancel button from those events. Only one job
runs at a time, so the active job is always the one Cancel targets.

## Plugins (the data tools)

The data tools live under `internal/plugins/`, independent of the UI:

- **`csv/`** — delimiter inspection, schema inference, row preview, column
  projection, the flexible CSV→SQL converter, redaction, profiling, the
  filter/dedupe/sample transforms, and the JSONL/SQLite/xlsx exporters. Streaming
  transforms write to a temp file and atomically rename on success.
- **`sql/analyze`** — a chunked single-pass analyzer. It masks string literals
  and comments and **carries lexer state across chunk boundaries**, so a quoted
  blob spanning a chunk can't be misread as live SQL (which would produce
  phantom tables or corrupt the byte ranges that extraction depends on). The hot
  `INSERT INTO` path is a hand-rolled scan; rarer statements use precise regexes
  guarded by a cheap keyword pre-scan.
- **`sql/extract`** — plans table byte ranges from the analysis and writes
  per-table slices (whole / schema / data) plus a JSON manifest.
- **`sql/preset`** — named cleanup rules (DEFINER strip, database/charset rename)
  compiled into streaming replaces.
- **`sql/reshape`** — the INSERT row-layout rewriter (explode / batch), with a
  comment/quote/backtick-aware lexer.
- **`sql/schemadiff`** — parses CREATE TABLE column lists and diffs two dumps.

Supporting packages: `search` / `replace` (streaming, incremental regex matcher,
encoding-aware), `exportx` (byte-range export), `encodingx` + `asciifold`
(encoding encode/decode, BOM handling, UTF-16 detection), `regexutil`,
`fileio` (atomic writes), `session` (the open-file registry handed opaque ids),
`settings`, `units`, `buildinfo`, `cachepath`, `logger`.

## Data-flow example: "extract one table"

1. UI calls `SqlAnalyze(fileId)` → `sql/analyze.Analyze` streams the dump once
   and caches a `Summary` (tables + offsets) on the `FileService`.
2. UI calls `SqlExtractTableViaDialog(fileId, name)`.
3. `FileService` plans the table's byte range from the cached summary, prompts
   for an output path, and streams that range with `exportx.ExportByteRange`.
4. The whole dump is read at most once for analysis and once more (only the
   table's range) for extraction — never held in memory.
