## Project Instructions

Quarry is a professional, production-facing desktop editor for huge files. Treat
the codebase as software intended for broad public use, where reliability,
predictable behavior, and data safety matter more than clever shortcuts.

- Preserve the core invariant: user source files are never silently mutated.
  Transforms and exports write new files; in-place writes must remain explicit,
  length-preserving, and backed up.
- Keep large-file behavior memory-bounded. Avoid whole-file reads, whole-file
  frontend state, or parser paths that scale with file size unless the caller
  has explicitly chosen a bounded sample.
- Prefer conservative, well-tested changes. Add or update focused tests when
  touching streaming, encoding, editing, replace/search, or export behavior.
- Keep the app tone serious and utilitarian. UI and documentation should read
  like a tool for people handling important data, not a demo shell.
