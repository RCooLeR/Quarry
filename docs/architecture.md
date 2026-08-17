# Architecture

Quarry is three layers:

1. a **streaming engine** in `internal/` with no UI dependencies,
2. a **binding layer** (`fileservice*.go`, `jobs.go`, `main.go`) that exposes the
   engine to the frontend over Wails, and
3. a **React UI** in `frontend/src/` whose editor materializes only bounded
   windows.

The guiding constraint is that **no operation may materialize unbounded
whole-file data in memory**. Large-file paths are windowed, streamed, or
sampled. A small-input path may materialize data only behind an explicit hard
byte budget.

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
- **Save copy** streams a full edited copy to a new file. It is the only enabled
  edited-save path and guards against the source changing underneath it.
- `internal/inplace` is a dormant transaction/recovery primitive. The public
  `SavePatch` boundary fails closed until a successful in-place operation can
  retain and verify a durable user-restorable backup and recovery is explicit.

The UI therefore offers *Save copy* for every staged edit and does not advertise
in-place mutation.

## The binding layer

`FileService` (package `main`, split across `fileservice*.go`) is the single
Wails-bound service. It exposes windowed reads, search, editing/staging, and the
CSV/SQL tools as methods that take and return **JSON-friendly structs**. The
whole-file content never crosses the bridge — only bounded windows and bounded
samples do.

`wails3 generate bindings -ts -i` regenerates the TypeScript client into
`frontend/bindings/` (gitignored) whenever a Go method signature changes.

`internal/bridgetransport` is a Wails-independent admission layer in front of
`/wails/runtime`. It owns chunk retention and exact assembly under fixed byte,
ID, concurrency, expiry, and working-memory ledgers, then forwards one bounded
ordinary request to Wails for its normal decoding and response semantics. Only
generated service calls and their cancellation object are admitted; enabling a
new Wails runtime surface requires an intentional policy and contract-test
change. This keeps transport policy testable without linking the engine test
graph to native WebView libraries.

`main.go` wires the Wails application: a single window with a dark custom title
bar (a Windows `COLORREF` theme), the embedded app icon, and
`EnableFileDrop` plus a small Go→frontend event bridge so dropped file paths are
opened by the UI.

## Background jobs

`jobs.go` holds the service job manager. `runServiceJob` (and the
transform-specific `withFileJob` adapter) establishes a cancellable `context`
before the operation acquires its document lease, registers one active job, and
emits `quarry:job-start` / `-progress` / `-end` application events. Producers
report bounded byte or record progress and must honor cancellation through
cleanup and atomic-output publication. `CancelJob` includes the opaque job ID,
so a delayed click cannot cancel a newer operation. The frontend renders the
events as a progress toast with a Cancel button. Only one registered service job
runs at a time. `runServiceJob` also attaches a context-carried publication
boundary consumed by `fileio.AtomicOutput`: the final link/rename and the job's
cancellation decision are serialized under one ownership lock. Cancellation
that wins first leaves the final name unpublished; successful publication (or
a `PublicationError` proving an artifact may already exist) wins and makes
later user, lifecycle, and shutdown cancellation stale. For multi-output jobs
that state is sticky after the first published artifact. A later failure is a
partial-publication failure with explicit output evidence, never reported as a
clean cancellation that removed every output.

## Plugins (the data tools)

The data tools live under `internal/plugins/`, independent of the UI:

- **`csv/`** — delimiter inspection, schema inference, row preview, column
  projection, the flexible CSV→SQL converter, redaction, profiling, the
  filter/dedupe/sample transforms, and the JSONL exporter. Streaming outputs use
  the shared no-clobber atomic-output layer and become visible only after a
  complete write and sync. SQLite/xlsx entry points currently fail closed before
  prompting because their libraries require mutable named scratch files.
- **`sql/analyze`** — a fixed-state, chunked single-pass analyzer. It masks
  literals/comments and **carries lexer state across chunk boundaries**, so a
  quoted blob spanning a chunk cannot become live SQL. Only exact regions for
  supported top-level CREATE/INSERT/REPLACE statements are retained. Ambiguous
  client commands, raw-payload modes, procedural bodies, malformed target
  prefixes, and unsupported dialect syntax fail closed with an empty summary;
  callers therefore cannot cache or extract from a partial analysis.
- **`sql/extract`** — plans the exact owned regions from a successful analysis
  and writes per-table slices (whole / schema / data) plus a JSON manifest.
  Source identity/generation is revalidated before publication and outputs use
  the shared no-clobber atomic-output layer. Session preamble, ALTER/DROP,
  triggers, other DML, and footer SQL are not included, so a slice is not
  promised to be a standalone import.
- **`sql/preset`** — a fail-closed compatibility boundary. Executable legacy
  byte/regex rules were removed; `Build` returns only a stable disabled error
  until a future token-aware design can preserve structured/serialized values.
- **`sql/reshape`** — the opt-in INSERT row-layout rewriter (explode / batch).
  It preserves accepted tuple payload bytes exactly and uses a fixed-state,
  memory-bounded safety guard. Ambiguous client commands, procedural bodies,
  raw-data modes, and unsupported dialect syntax fail the operation before an
  output is published; regrouping is not presented as database-semantic
  equivalence.
- **`sql/schemadiff`** — parses CREATE TABLE column lists and diffs two dumps.

Supporting packages: `search` / `replace` (streaming, incremental regex matcher,
encoding-aware), `exportx` (byte-range export), `encodingx` + `asciifold`
(encoding encode/decode, BOM handling, UTF-16 detection), `regexutil`,
`fileio` (atomic writes), `session` (the open-file registry handed opaque ids),
`settings`, `units`, `buildinfo`, `cachepath`, `logger`.

## Data-flow example: "extract one table"

1. UI calls `SqlAnalyze(fileId)` → `sql/analyze.Analyze` streams the dump once.
   `FileService` caches the exact source-bound `Summary` only after a fully
   successful, unambiguous scan; any analyzer error leaves no partial summary.
2. UI calls `SqlExtractTableViaDialog(fileId, name)`.
3. `FileService` revalidates the source generation, plans all exact regions
   owned by that table, prompts for an output path, and streams them to an
   atomic no-clobber output.
4. The whole dump is read once for analysis; extraction rereads only the
   selected regions. Neither path holds the dump in memory.
