# Development

## Repository layout

```
.
├── main.go                  # Wails app setup (window, title bar, file-drop)
├── fileservice.go           # FileService: open/close, windows, search
├── fileservice_csv.go       # CSV tools (inspect/schema/convert/transform/export)
├── fileservice_sql.go       # SQL tools (analyze/extract/reshape/schemadiff/…)
├── fileservice_edit.go      # staging, patch-in-place, save-copy
├── fileservice_hex.go       # hex window
├── jobs.go                  # cancellable background-job manager
├── build/                   # Wails build config + per-OS Taskfiles + icons
├── internal/                # the streaming engine (no UI deps)
│   ├── document/ lineindex/ # windowed reads, chunk cache, sparse line index
│   ├── editwindow/ manualedit/ inplace/   # staged edits + in-place patching
│   ├── search/ replace/ regexutil/        # streaming, encoding-aware matching
│   ├── exportx/ fileio/                    # byte-range export, atomic writes
│   ├── encodingx/ asciifold/               # encodings, BOM, case-folding
│   ├── session/ settings/ units/ logger/   # registry + misc support
│   └── plugins/
│       ├── csv/             # inspect, schema, convert, transform, export
│       └── sql/             # analyze, extract, preset, reshape, schemadiff, highlight
└── frontend/
    └── src/
        ├── App.tsx          # workbench shell: menus, panels, state
        ├── Tools.tsx        # the CSV / SQL data-tools panel
        ├── CsvGrid.tsx HexView.tsx DiffView.tsx Sidebar.tsx
        ├── MenuBar.tsx CommandPalette.tsx XRay.tsx
        └── editor/          # QuarryEditor.ts + highlight profiles
```

Module: `github.com/quarry/quarry-wails3` (Go 1.26). Stack: Wails v3 (alpha2),
React + TypeScript + Vite, CodeMirror 6.

## Build & test

```sh
task dev            # hot-reload dev (or: wails3 dev)
task build          # production build → bin/quarry.exe
go test ./...       # engine + plugin + binding tests
cd frontend && npm run build   # type-check + bundle the UI
```

`bin/`, `frontend/dist/`, `frontend/node_modules/`, and `frontend/bindings/` are
gitignored. `wails_windows_amd64.syso` (the Windows icon resource) **is**
committed so `go build` embeds the icon without a separate generate step.

## Regenerating bindings

The TypeScript client in `frontend/bindings/` is generated from the Go
`FileService` methods. After changing any bound method's signature or any struct
it returns:

```sh
wails3 generate bindings
```

Then import the regenerated client from the frontend (e.g.
`FileService.SqlSchemaDiff(...)`). Because the directory is gitignored, anyone
building from a clean checkout runs this once before the first frontend build.

## How to add a new data tool

A tool is usually four small steps:

1. **Engine** — add the streaming function to the right plugin package
   (`internal/plugins/csv` or `internal/plugins/sql/...`). Read with
   `document`/`os`, honor the passed `context` for cancellation, and write to a
   **new** file via a temp path + atomic rename (see `tempOutputPath` and
   `rowTransform` in `internal/plugins/csv/transform.go` for the pattern). Add a
   unit test.
2. **Binding** — add a `FileService` method in the relevant `fileservice_*.go`.
   For a long-running transform, wrap the work in `s.withJob(title, fn)` so it
   gets cancellation + a progress toast; prompt for the output path with
   `saveDialog` before starting the job.
3. **Bindings** — run `wails3 generate bindings`.
4. **UI** — add a control in `Tools.tsx` (CSV or SQL branch) that calls the new
   method through the generated client and reports the result.

## Testing conventions

- Pure engine logic (parsers, transforms, the analyzer, exporters) is covered by
  Go unit tests next to the code (`*_test.go`). Exporters round-trip their output
  (re-query the SQLite DB, re-open the xlsx) rather than asserting on bytes.
- Prefer table/streaming tests with small fixtures written to `t.TempDir()`.
- The frontend is validated by `tsc` (no emit) and the Vite production build;
  there is no browser test harness.

## Notes & gotchas

- **Source is never silently modified.** Every transform/export writes a new
  file. Only *patch in place* touches the original, only for length-preserving
  edits, and only after writing a backup.
- **Windowed everything.** When adding a feature, reach for `ReadRange` /
  sampling / streaming — never read `Size()` bytes into memory.
- **Encoding.** Search and replace operate on raw bytes; the query is encoded
  into the file's encoding first (`encodingx`). Regex is UTF-8 only.
- **SQL analysis caches** a `Summary` per file id on the `FileService`; tools
  that need offsets (extract, split, fixture, schema diff) require the dump to be
  analyzed first.
- **Always build the GUI exe with `task build` / `wails3 build`**, not a bare
  `go build`. The production task links with `-ldflags="-H windowsgui"`, which
  sets the Windows GUI subsystem; a plain `go build` produces a *console*
  subsystem binary that pops an extra terminal window next to the app. (`task
  build` regenerates `wails_windows_amd64.syso` for the icon and deletes it
  afterward; the committed copy is what lets a bare `go build` still embed the
  icon — don't commit its deletion.)
