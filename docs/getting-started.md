# Getting started

## Prerequisites

- **Go** 1.26 or newer
- **Node.js** + **npm** (the frontend is built with Vite)
- **Wails v3 CLI** — see the
  [Wails installation guide](https://v3.wails.io/getting-started/installation/)
- **[Task](https://taskfile.dev/)** (optional) — the `Taskfile.yml` wraps the
  common Wails commands

Quarry targets Windows first (the build embeds a Windows icon and a dark title
bar via the Windows API), but the Go engine and the React UI are
platform-neutral.

## Build & run

Using Task:

```sh
task dev      # development mode, hot reload for Go + frontend
task build    # production build → bin/quarry.exe
task run      # build and run
task package  # packaged production build
```

Equivalent Wails CLI commands:

```sh
wails3 dev -config ./build/config.yml
wails3 build
```

Frontend-only build (type-check + bundle), useful in CI:

```sh
cd frontend
npm install
npm run build
```

The Taskfiles default to `npm`, but can run the frontend through another
supported package manager:

```sh
task build PACKAGE_MANAGER=pnpm
task dev WAILS_VITE_PORT=9246
```

## Running the tests

The engine is covered by Go unit tests:

```sh
go test ./...
```

The streaming primitives, the CSV/SQL plugins, the search/replace engine, and
the binding helpers all have tests. The frontend is validated with `tsc` and the
Vite production build.

## Opening a file

- **Open file…** (Ctrl+O) — native file dialog; any file type is allowed.
- **Paste a path** on the empty-state screen and press Enter.
- **Drag and drop** a file onto the window.
- **Recent files** — the File menu lists recently opened paths; the previous
  session's open files are restored on launch.

Opening is cheap: the file is stat'd and a sparse line index starts building in
the background. You can scroll and navigate immediately; exact line numbers
"sharpen" as the index completes.

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
3. Save with one of:
   - **Patch in place** — only enabled when the staged edits preserve total
     file length; overwrites just the changed bytes and keeps a backup.
   - **Save copy…** — streams a full edited copy to a new file.
   - **Discard edits** — drop staged changes.

See [Architecture → Editing & saving](architecture.md#editing--saving) for why
length-preserving edits can be patched in place while others stream a copy.
