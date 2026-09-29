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
> user-restorable backup. The remaining output-transaction hardening must be
> completed before treating this branch as release-ready.

## Quick start

Canonical prerequisites: [Go](https://go.dev/) 1.27.1, Node.js 24.21.0 with npm
12.0.2, Wails CLI `v3.0.0-beta.23`, and [Task](https://taskfile.dev/)
(optional but convenient). Compatible developer Node ranges and exact install
commands are listed in [Getting started](docs/getting-started.md).

```sh
# run from the repository root in development (hot reload for Go + frontend)
task dev

# optimized local build (not release-qualified) → bin/quarry.exe
task build

# run the tests
task verify
task verify:race
```

The root Taskfile forwards these commands to the `src/` workspace. Run direct
Go and Wails commands from `src/`; build output remains in the root `bin/`.
See **[docs/getting-started.md](docs/getting-started.md)** for details.

## Project layout

```
.
├── src/                 # self-contained Go/Wails application workspace
│   ├── go.mod, go.sum   # Go module and locked dependency checksums
│   ├── main.go         # Wails window, title bar, and file-drop bridge
│   ├── fileservice*.go # Go↔frontend binding layer and adjacent tests
│   ├── jobs.go         # cancellable background-job manager
│   ├── internal/       # streaming engine and CSV/SQL plugins
│   ├── frontend/       # React UI, generated bindings, tests, and npm manifest
│   ├── build/          # platform assets, build helpers, and cross-build image
│   └── Taskfile.yml    # application build and verification tasks
├── docs/                # user and developer documentation
├── bin/                 # generated executables (gitignored)
├── logos/               # branding assets
├── .github/             # CI, candidate validation, and dependency updates
└── Taskfile.yml         # root entry point for application tasks
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
