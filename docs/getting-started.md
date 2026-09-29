# Getting started

## Prerequisites

- **Go** 1.27.1 or newer. CI and release-candidate validation use exactly
  1.27.1 from `src/go.mod`.
- **Node.js** `^22.22.2`, `^24.15.0`, or `>=26.0.0` with **npm** `12.0.2` (the
  frontend install is locked to npm and `src/frontend/package-lock.json`). The canonical CI
  runtime is Node 24.21.0, also recorded in `.node-version`.
- **Wails v3 CLI** `v3.0.0-beta.23`, matching `src/go.mod`:
  `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.23`
- **[Task](https://taskfile.dev/)** (optional) — the `Taskfile.yml` wraps the
  common Wails commands

Quarry targets Windows first (the build embeds a Windows icon and a dark title
bar via the Windows API), but the Go engine and the React UI are
platform-neutral.

There is not yet a supported public binary release. See
[Support, installation, and release status](support-and-release.md) before
using or distributing a validation build.

## Build & run

Using Task from the repository root (the root Taskfile forwards to `src/`):

```sh
task dev      # development mode, hot reload for Go + frontend
task build    # optimized local build; not a release-qualified artifact
task run      # run the built executable
task package  # local unsigned/unqualified package for validation only
```

Direct Wails CLI commands run from the application workspace:

```sh
cd src
wails3 dev -config ./build/config.yml
wails3 build
```

Frontend-only build (tests, type-check, and bundle), from the repository root:

```sh
task frontend:check
```

That task generates TypeScript bindings before running `npm ci`, Vitest,
TypeScript, and the production Vite build. For a manual equivalent, use
`wails3 generate bindings -ts -i` from `src/` before entering `src/frontend/`.
Executables and local packages are written to the repository's `bin/`, not
inside `src/`.

Quarry supports the committed npm lockfile as its sole frontend dependency
graph. `task dev WAILS_VITE_PORT=9246` changes the development port without
changing dependencies.

## Running the tests

The engine/internal packages can be tested without a frontend bundle; the full
module embeds `src/frontend/dist`, so generate the ignored bundle first in a fresh
checkout:

```sh
cd src
go test ./internal/... ./build/releasemeta
wails3 task frontend:check
go test ./...  # complete module, after frontend:check
```

The canonical local gate, run from the repository root, is:

```sh
task verify       # unit/production-tag/UI/static/dependency/workflow gates
task verify:race  # race-enabled suite, run separately because it is slower
```

Both canonical tasks build/check the frontend before whole-module Go commands,
so they work from a fresh checkout where `src/frontend/dist` is absent. `task verify`
downloads exact scanner releases without adding them to
`src/go.mod`. It requires network access for the Go vulnerability database and npm
advisory service. CI runs the same checks from a fresh checkout and records the
verified commit, tree, tool versions, and lockfile hashes.

The streaming primitives, the CSV/SQL plugins, the search/replace engine, and
the binding helpers all have tests. Standard Wails builds generate bindings,
run the frontend test suite, type-check, and build the Vite production bundle.

## Opening a file

- **Open file…** (Ctrl+O) — native file dialog; any file type is allowed.
- **Paste a path** on the empty-state screen and press Enter.
- **Drag and drop** a file onto the window.
- **Recent files** — the File menu lists paths opened during the current run.
  Path persistence is off on a fresh install. Turn on **File → Remember
  workspace paths** only if local path history is appropriate for the machine.
- **Session restore** — after path memory is enabled, separately enable **File
  → Restore remembered tabs on launch**. Quarry stores and restores at most 16
  validated paths. It never enables restore implicitly.

The File menu can clear recent paths, the saved session, and all path-keyed
bookmarks in one operation without closing current tabs. Turning workspace path
memory off performs the same storage cleanup and prevents later path writes;
bookmarks and recent entries created afterward remain available only until the
app exits. When upgrading from a version that stored paths automatically,
legacy recent/session/bookmark data is cleared unless the new versioned opt-in
preference already exists.

Opening is cheap: the file is stat'd and a sparse line index starts building in
the background. You can scroll and navigate immediately; exact line numbers
"sharpen" as the index completes.

> **In-app help:** press **F1** (or Help → *Help & shortcuts…*) for a built-in
> guide to the keyboard shortcuts and every tool.

## Views

Switch views from the **View** menu or the command palette (Ctrl+P):

| View | What it shows |
| --- | --- |
| **Plain text** | The CodeMirror editor surface with syntax highlighting. |
| **Hex view** | A byte/hex/ASCII window for binary or encoding inspection. |
| **Grid view** | A spreadsheet grid for CSV/TSV (rows × columns), windowed. |
| **Diff panel** | Staged edits as a list or side-by-side view. |

Other surfaces:

- **Data tools panel** — the CSV or SQL workbench for the active file
  (see [Features & tools](features.md)).
- **File X-ray** — a minimap rail showing analyzed regions (e.g. SQL tables);
  click to seek.
- **Bookmarks** — mark byte offsets and jump back to them.

## Editing

1. Toggle **Edit mode** (the file must be editable — text, a supported
   encoding).
2. Make changes in the current window.
3. Finish with one of:
   - **Save copy…** — streams a full edited copy to a new file. In-place save
     is disabled until Quarry can retain and verify a durable backup.
   - **Discard edits** — drop staged changes.

See [Architecture → Editing & saving](architecture.md#editing--saving) for the
source-safety rationale.
