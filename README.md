# Quarry

Quarry is a Windows-first desktop editor and toolkit designed for **very large**
text files, including SQL dumps, CSV/TSV exports, and logs.

The editor reads bounded, line-aligned **windows** as you scroll and navigate by
byte offset instead of retaining the complete file in frontend state. This
keeps the interactive working set bounded by configured window and cache limits,
but scans, indexing, hashing, and some tool metadata still cost time or space in
proportion to the input. A 400 GB file is a qualification target, not a
currently validated size claim. The available workbench includes SQL dump
analysis and extraction, configurable CSV→SQL conversion, streaming search and
harvest, row transforms, and export bridges. Generic replacement is not exposed
until its format-safety contract is complete.

Built with **Go + [Wails v3](https://v3.wails.io/) + React + TypeScript +
[CodeMirror 6](https://codemirror.net/)**.

## Highlights

- **Bounded-window opening** — the initial editor window does not wait for a
  whole-file read or full index. Indexing and analysis are background,
  cancellable, and optional; their runtime still scales with the work selected.
- **Editing with source preservation** — staged edits stream to a new full copy;
  in-place save stays unavailable until its backup/recovery contract is durable.
- **SQL dump workbench** — analyze tables and DEFINER usage, extract discovered
  CREATE/INSERT/REPLACE regions, split those regions per table, sample a tiny dev fixture, lint
  re-import problems (including mixed charset/collation use), explicitly
  regroup extended↔single-row INSERT statements with semantic warnings, and
  diff two dumps' schemas.
- **CSV workbench** — delimiter detection, schema inference, a spreadsheet grid,
  a convertcsv-style CSV→SQL builder, column transforms, filter/dedupe/sample,
  redact/anonymize, and a column profiler.
- **Export bridges** — CSV → JSONL, plus copying a bounded preview as a
  Markdown table. SQLite and Excel export remain unavailable until they can use
  the same no-clobber, fail-closed publication guarantees.
- **Fast navigation** — command palette, go-to line/offset/percent, a file
  X-ray minimap, bookmarks, and jump-to-table.
- **Ops polish** — cancellable background jobs with a progress toast,
  follow-tail of growing files, drag-and-drop open, a light/dark theme, and
  built-in **help** (F1). Local recent-path memory and bounded session restore
  are separate, privacy-conservative opt-in preferences.

> **Current edited-save policy:** Save copy is the only enabled save path and
> in-place save is disabled until Quarry can retain and verify a durable,
> user-restorable backup. The broader output-transaction hardening tracked in
> `col-review/` must be completed before treating this branch as release-ready.

## Quick start

Canonical prerequisites: [Go](https://go.dev/) 1.27.0, Node.js 24.20.0 with npm
12.0.2, Wails CLI `v3.0.0-beta.15`, and [Task](https://taskfile.dev/)
(optional but convenient). Compatible developer Node ranges and exact install
commands are listed in [Getting started](docs/getting-started.md).

```sh
# run in development (hot reload for Go + frontend)
task dev          # or: wails3 dev -config ./build/config.yml

# optimized local build (not release-qualified) → bin/quarry.exe
task build        # or: wails3 build

# run the tests
task verify
task verify:race
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
│   ├── manualedit/ inplace/ editwindow/   # staged edits; guarded recovery primitives
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
- [Support & release status](docs/support-and-release.md) — validation matrix,
  installation limits, data locations, and publication blockers

## License

No open-source or redistribution license has been selected yet. Until an
approved `LICENSE` file is added, the repository must not be treated as granting
permission to redistribute Quarry or publish release artifacts.
