# Quarry documentation

Quarry is a Windows-first desktop editor and toolkit for very large SQL/CSV/log
files. It streams bounded windows instead of loading whole files, and layers a
data workbench on top.

## Contents

- **[Getting started](getting-started.md)** — prerequisites, build/run, opening
  files, and the views you can switch between.
- **[Features & tools](features.md)** — the complete reference: editor, search,
  navigation, the SQL and CSV workbenches, exports, and ops features.
- **[Architecture](architecture.md)** — the streaming document model, the
  editor windowing, the edit/save pipeline, the binding layer, and the job
  manager.
- **[Development](development.md)** — repo layout, building, testing,
  regenerating bindings, and how to add a new tool end to end.

## What Quarry is (and isn't)

Quarry is built for files that are too big to open elsewhere:

- It **never** loads a whole file into memory. The editor holds a few bounded
  windows; navigation is by byte offset, with line numbers resolved from a
  sparse index.
- Heavy work (full-file indexing, SQL analysis, transforms, exports) runs in the
  background, is cancellable, and writes results to **new** files — your source
  is never silently modified.
- It is a focused workbench, not a general IDE: the value is in opening,
  searching, slicing, converting, and exporting huge dumps quickly.

## Conventions used in these docs

- "Window" = a bounded, line-aligned slice of the file currently materialized in
  the editor.
- "Transform" / "export" = a streaming operation that reads the source and
  writes a new output file.
- "Patch in place" = the one operation that edits the original file's bytes,
  only when the edit preserves total length, and always after writing a backup.
