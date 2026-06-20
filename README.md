# Quarry

Quarry is a Windows-first desktop editor and toolkit for **very large** text
files — multi-gigabyte (400 GB+) SQL dumps, CSV/TSV exports, and logs that no
ordinary editor can open.

It never reads the whole file into memory. The editor holds only bounded,
line-aligned **windows** that stream in as you scroll and navigate by byte
offset, so a 4 KB file and a 400 GB file cost roughly the same. On top of that
streaming core sits a workbench of data tools: SQL dump analysis and extraction,
flexible CSV↔SQL conversion, streaming search/replace/harvest, row transforms,
and export bridges.

Built with **Go + [Wails v3](https://v3.wails.io/) + React + TypeScript +
[CodeMirror 6](https://codemirror.net/)**.

## Highlights

- **Open anything, instantly** — files are memory-mapped in spirit, not loaded;
  first paint doesn't wait for indexing. Indexing and analysis are background,
  cancellable, and optional.
- **Editing without rewriting the file** — length-preserving edits can be
  *patched in place* (with a backup); anything else streams a full edited copy.
- **SQL dump workbench** — analyze tables/charsets/DEFINER, extract a table or
  its schema/data, split a dump per table, sample a tiny dev fixture, lint for
  re-import problems, reshape extended↔single-row INSERTs, and diff two dumps'
  schemas.
- **CSV workbench** — delimiter detection, schema inference, a spreadsheet grid,
  a convertcsv-style CSV→SQL builder, column transforms, filter/dedupe/sample,
  redact/anonymize, and a column profiler.
- **Export bridges** — CSV → JSONL, SQLite, or Excel (.xlsx); copy a preview as
  a Markdown table.
- **Fast navigation** — command palette, go-to line/offset/percent, a file
  X-ray minimap, bookmarks, and jump-to-table.
- **Ops polish** — cancellable background jobs with a progress toast,
  follow-tail of growing files, recent files, session restore, drag-and-drop
  open, and a light/dark theme.

> **Safety invariant:** every transform and export writes to a **new** file
> (temp file + atomic rename). The only in-place write is the explicit
> length-preserving *patch in place*, which keeps a backup. Quarry never
> silently mutates your source.

## Quick start

Prerequisites: [Go](https://go.dev/) 1.26+, [Node.js](https://nodejs.org/) +
npm, the [Wails v3 CLI](https://v3.wails.io/getting-started/installation/), and
[Task](https://taskfile.dev/) (optional but convenient).

```sh
# run in development (hot reload for Go + frontend)
task dev          # or: wails3 dev -config ./build/config.yml

# production build → bin/quarry.exe
task build        # or: wails3 build

# run the tests
go test ./...
```

See **[docs/getting-started.md](docs/getting-started.md)** for details.

## Project layout

```
.
├── main.go              # Wails app: window, dark title bar, file-drop bridge
├── fileservice*.go      # FileService — the Go↔frontend binding layer
├── jobs.go              # cancellable background-job manager
├── internal/            # the streaming engine (no UI dependencies)
│   ├── document/        # FileDocument: bounded windowed reads + chunk cache
│   ├── lineindex/       # sparse line index (approx → exact line numbers)
│   ├── search/ replace/ # streaming, encoding-aware find / replace
│   ├── exportx/         # byte-range export
│   ├── manualedit/ inplace/ editwindow/   # staged edits & in-place patching
│   └── plugins/
│       ├── csv/         # inspect, schema, convert, transform, export
│       └── sql/         # analyze, extract, preset, reshape, schemadiff
└── frontend/src/        # React UI
    ├── editor/QuarryEditor.ts   # the CodeMirror windowed editor surface
    ├── App.tsx          # workbench shell, menus, panels
    └── Tools.tsx        # the data-tools panel (CSV / SQL)
```

## Documentation

- [Documentation index](docs/index.md)
- [Getting started](docs/getting-started.md) — install, build, run, open a file
- [Features & tools](docs/features.md) — the full editor + workbench reference
- [Architecture](docs/architecture.md) — how streaming, editing, and bindings work
- [Development](docs/development.md) — build, test, bindings, adding a tool

## License

See the repository for license details.
