# Quarry documentation

Quarry is a Windows-first desktop editor and toolkit for very large SQL/CSV/log
files. Its editor streams bounded windows instead of materializing an unbounded
source, and it layers a data workbench on top.

## Contents

- **[Project audit, September 2026](project-audit-2026-09-20.md)** — dependency
  updates, confirmed defects, performance evidence, validation, and remaining work.
- **[Getting started](getting-started.md)** — prerequisites, build/run, opening
  files, and the views you can switch between.
- **[Features & tools](features.md)** — the complete reference: editor, search,
  navigation, the SQL and CSV workbenches, exports, and ops features.
- **[Architecture](architecture.md)** — the streaming document model, the
  editor windowing, the edit/save pipeline, the binding layer, and the job
  manager.
- **[Development](development.md)** — repo layout, building, testing,
  regenerating bindings, and how to add a new tool end to end.
- **[Support & release status](support-and-release.md)** — current platform
  validation, installation limits, data locations, and release blockers.

## What Quarry is (and isn't)

Quarry is built for files that are too big to open elsewhere:

- The editor holds only bounded windows; navigation is by byte offset, with line
  numbers resolved from a sparse index. Large-file tools stream or sample their
  input. A small-input path may materialize data only within an explicit hard
  byte budget.
- Full-file indexing runs asynchronously and stops when its file session closes.
  SQL analysis and supported long transforms and exports use the background-job
  slot and expose cancellation. Output-producing tools write **new** files —
  your source is never silently modified.
- It is a focused workbench, not a general IDE: the value is in opening,
  searching, slicing, converting, and exporting huge dumps quickly.

## Conventions used in these docs

- "Window" = a bounded, line-aligned slice of the file currently materialized in
  the editor.
- "Transform" / "export" = a streaming operation that reads the source and
  writes a new output file.
- "Save copy" = the only enabled edited-save path. The in-place implementation
  remains disabled until it can retain and verify a durable backup.
