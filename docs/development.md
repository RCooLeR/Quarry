# Development

## Repository layout

```
.
├── src/                     # self-contained application workspace
│   ├── go.mod, go.sum       # unchanged Go module/import identity
│   ├── main.go              # Wails window, title bar, file-drop
│   ├── fileservice*.go      # bounded service APIs and adjacent tests
│   ├── jobs.go              # cancellable background-job manager
│   ├── build/               # platform assets, Go build helpers, Taskfiles
│   ├── internal/            # streaming engine (no UI dependencies)
│   │   ├── document/ lineindex/         # windowed reads and sparse line index
│   │   ├── editwindow/ manualedit/ inplace/ # staged edits and recovery primitives
│   │   ├── search/ replace/ regexutil/  # streaming, encoding-aware matching
│   │   ├── exportx/ fileio/             # byte-range export and atomic writes
│   │   └── plugins/csv/ plugins/sql/    # streaming data tools
│   ├── frontend/
│   │   ├── package.json, package-lock.json
│   │   ├── tests/           # UI and source-contract tests
│   │   └── src/             # React components and CodeMirror editor
│   └── Taskfile.yml         # application tasks
├── docs/                    # user and developer documentation
├── bin/                     # generated executables/packages (gitignored)
├── logos/                   # branding assets
├── .github/                 # repository CI and dependency-update policies
└── Taskfile.yml             # forwards tasks into src/
```

Source paths below are relative to `src/` unless explicitly described as
repository-root paths. Direct Go, npm, and Wails commands use `src/` (or
`src/frontend/` for npm); the root Taskfile forwards its tasks with the correct
working directory. Generated executables always go to the root `bin/`.

Module: `github.com/quarry/quarry-wails3` (Go 1.27.1). Stack: Wails v3
`v3.0.0-beta.23` with its matching published frontend runtime
`3.0.0-beta.23`, React + TypeScript + Vite, and CodeMirror 6. The frontend
package is `quarry-frontend`.

## Build & test

```sh
# repository root
task dev                     # hot-reload development
task build                   # optimized local build; not release-qualified
task frontend:check          # bindings + locked UI tests/type-check/build

# direct Go commands run in the module workspace
cd src
go test ./...                 # full module; requires the generated dist above
go test ./internal/...        # engine/plugin tests; no frontend prerequisite
```

For the full local merge gate, run from the repository root or `src/`:

```sh
task verify
task verify:race
```

The first command creates the ignored frontend bundle before whole-module Go
commands, then verifies module and lockfile immutability, default and
production-tag Go tests, frontend tests/type-check/bundle budgets, vet,
staticcheck `v0.8.1`, govulncheck `v1.8.0`, npm audit, actionlint `v1.7.12`, and
GoReleaser `v2.18.2` containment. The second command is separate because the
race suite is materially slower. Scanner versions are pinned in `src/Taskfile.yml`;
CI additionally fixes Go 1.27.1, Node 24.21.0, npm 12.0.2, Wails CLI
`v3.0.0-beta.23`, runner generations, and every GitHub Action to a full
commit SHA.

To prove clean-checkout behavior, use a new clone rather than deleting files
from a working tree. Install only the documented toolchain, then run
`task verify`, `task verify:race`, and `task build`. The final
`git status --porcelain --untracked-files=all` must be empty; generated
bindings, bundles, platform resources, and binaries must remain ignored build
outputs. CI performs this check independently on the Windows and Linux
candidate targets.

Quarry does not ship a network server or server container. The registered
`FileService` is a desktop-local capability surface and is not authenticated or
confined for remote use. Builds made manually with the Wails `server` tag exit
before starting the runtime. `task setup:docker` remains available only for the
desktop cross-OS image. Its Debian 13/Trixie base enforces Wails beta.23's GTK
4.14-or-newer requirement at image-build time. Linux builds through that image
are same-architecture only: native GTK/WebKit libraries and GCC cannot safely
serve another `GOARCH`.

The desktop bridge uses Quarry's bounded HTTP guard ahead of Wails. Keep the Go
and npm Wails versions paired: the frontend runtime's 512 KiB chunk protocol is
part of this contract. The guard owns chunk storage/assembly, caps ordinary and
chunked input plus aggregate pending/concurrent state, and admits only generated
service calls and cancellation. Do not bypass it by installing Wails'
`HTTPTransport` directly in `main.go`; any new runtime object must be reviewed
and added to the guard's frontend-surface contract test.

The frontend dependency graph is intentionally npm-only and pinned by
`package-lock.json` and npm `12.0.2`; alternate package managers are not supported. Set
`WAILS_VITE_PORT` when the default dev port (`9245`) is already occupied.

Obfuscated builds are temporarily disabled. Garble `v0.18.0` now supports Go
1.27, but Quarry's generated bindings and cross-platform obfuscated builds
have not been qualified with it. Normal builds remain supported. Re-enabling
obfuscation requires a pinned tool plus binding and packaged-app verification.

The production frontend build enforces the startup and total JavaScript budgets
in `frontend/scripts/check-bundle-budget.mjs`. Optional workbench views must
remain lazy-loaded. The application entry is capped at 280 KiB uncompressed
and 90 KiB gzip; the complete initial module graph, including module preloads,
is capped at 352 KiB uncompressed and 110 KiB gzip. The initial stylesheet is
capped at 64 KiB, every JavaScript chunk at 320 KiB, and all JavaScript chunks
together at 768 KiB. Vite 8's Rolldown chunk groups isolate the React 19 runtime
from Quarry's entry, preserving the tighter entry limits and a separately
cacheable framework chunk. Change a budget only with a reviewed bundle analysis
and an explanation of the startup impact. The September 2026 dependency audit
raised only the raw initial and total budgets to accommodate React 19.3's
framework growth; gzip and individual-chunk budgets remain unchanged. See the
[audit report](project-audit-2026-09-20.md) for measurements and qualification limits.

Android and iOS are not supported release targets and are intentionally absent
from the root task inventory. Their generated platform Taskfiles support only
local debug/emulator development: explicit production requests are rejected,
package/release/deploy tasks exit before dependencies or output work, and the
Android Gradle release variant is disabled so a direct `assembleRelease` or
`bundleRelease` invocation cannot fall back to debug signing. The remaining
mobile scaffolding is not approved for distribution.

## CI & releases

GitHub Actions runs the desktop build on Windows and Linux. Linux uses Ubuntu
24.04 plus GTK/WebKit development packages so the native Wails build has the
same libraries that the Linux package metadata declares. macOS is excluded from
the verification and candidate matrices because `fileio.OpenAtomicOutput`
currently fails closed there; a compiled app would not support Quarry's
artifact-producing workflows.

Both branch CI and release-candidate validation call the same reusable
verification workflow. The caller supplies one full commit ID; every candidate
platform checks out that ID, verifies it did not change, runs the test/build
gates, and emits a verification record containing the commit, tree, commit
date, exact tool versions, and dependency-input hashes.

Release-candidate validation is tag-driven:

```sh
git tag -a v0.0.1 -m "Quarry v0.0.1"
git push origin v0.0.1
```

The release helper accepts only a canonical, annotated `v` semantic-version
tag in a clean checkout. It dereferences and records the tag object, commit,
tree, commit timestamp, version, and numeric workflow build number, and fails
if the tag does not point at checked-out `HEAD`. Candidate jobs start only after
the reusable verification succeeds for that exact commit, then check out the
recorded commit rather than a moving ref.

The workflow builds native Wails artifacts on the Windows and Linux runners and
retains them for seven days as unqualified, non-release CI evidence. Sidecar
SHA-256 files are useful for transfer-integrity diagnosis but are not
authenticated publisher proof.
Separate provenance and verification JSON records are retained for 30 days.
The workflow deliberately cannot create a public GitHub release. Publication
remains disabled until the repository has an approved license, candidate
signing and platform verification, authenticated checksums or attestations, and
packaged-app data-safety tests on clean supported machines.

Linux candidate archives normalize ordering, ownership, timestamps, and gzip
headers from the tagged commit timestamp. Native OS toolchains and the current
unsigned Windows and Linux candidates are not yet claimed to be bit-for-bit
reproducible. The digest-pinned cross-OS container is developer infrastructure,
not a substitute for native signed release qualification. Its Linux path
requires the requested CPU architecture to match the host/image architecture.

The root `bin/`, `src/.task/`, `src/frontend/dist/`, `src/frontend/node_modules/`,
generated Windows `.syso` resources, and `src/frontend/bindings/` are gitignored,
as are GoReleaser's root `dist/` and `release-artifacts/` scratch directories. Platform tasks
generate versioned resources from tracked templates without rewriting those
templates.

## Regenerating bindings

The TypeScript client in `frontend/bindings/` is generated from the Go
`FileService` methods. After changing any bound method's signature or any struct
it returns:

```sh
cd src
wails3 generate bindings -ts -i
```

Then import the regenerated client from the frontend (e.g.
`FileService.SqlSchemaDiff(...)`). Because the directory is gitignored, anyone
building from a clean checkout runs this once before a direct frontend build.
Standard Wails/Task builds generate bindings automatically before frontend tests
and bundling.

## Updating build metadata

Product metadata lives in `src/build/config.yml`. After changing `info` or file
association values, regenerate the committed platform assets:

```sh
task common:update:build-assets  # from the repository root
```

Review the generated files under `src/build/` before committing so package names,
bundle identifiers, installer text, and desktop entries stay consistent.
Use the task rather than invoking `wails3 update build-assets` directly. Wails
beta.23 regenerates the Darwin plists with its macOS 12 default; the task's
cross-platform policy step validates both generated plists and reapplies
Quarry's required macOS 13 minimum.

## How to add a new data tool

A tool is usually four small steps:

1. **Engine** — add the streaming function to the right plugin package
   (`internal/plugins/csv` or `internal/plugins/sql/...`). Read with
   `document`/`os`, honor the passed `context` for cancellation, reject aliases
   of every open source before prompting, and publish a **new**, no-clobber file
   with `fileio.OpenAtomicOutput` plus `CommitContext` and an explicit
   confidentiality-preserving mode (default `0600` unless deliberately
   preserving source access). Join cleanup errors instead of
   deleting a pathname by name. Add no-clobber, cancellation, short-read, mode,
   alias, and publication-race tests. Never replace the service job context with
   `context.Background()` before commit: its context-carried publication
   boundary linearizes the final filesystem publication with cancellation.
   Multi-output engines must return an explicit partial-publication summary if
   any later output fails after an earlier one became visible.
2. **Binding** — add a `FileService` method in the relevant `fileservice_*.go`.
   Run long work through `runServiceJob` (or the `s.withFileJob` transform
   adapter), acquire document leases inside its callback with the passed
   context, and propagate that context through the engine and atomic commit.
   Prompt for the output path with `saveDialog` before registering the job, so a
   cancelled dialog never occupies the global job slot. Once any atomic output
   is published, user/lifecycle cancellation is intentionally stale; allow the
   callback to finish and preserve its success or partial-failure evidence.
3. **Bindings** — run `wails3 generate bindings -ts -i` from `src/`.
4. **UI** — add a control in `Tools.tsx` (CSV or SQL branch) that calls the new
   method through the generated client and reports the result.

## Testing conventions

- Pure engine logic (parsers, transforms, the analyzer, exporters) is covered by
  Go unit tests next to the code (`*_test.go`). Exporters verify complete output,
  no-clobber publication, cancellation, and cleanup rather than only asserting on
  byte fragments.
- Prefer table/streaming tests with small fixtures written to `t.TempDir()`.
- The frontend is validated by Vitest, `tsc` (no emit), the Vite production
  build, and focused in-browser smoke checks.

## Notes & gotchas

- **Source is never silently modified** is a required invariant. In-place save
  remains disabled until it has a durable, verified user-restorable backup and
  explicit recovery workflow; remaining writers must migrate to the shared
  output transaction before release.
- **Windowed everything.** When adding a feature, reach for `ReadRange` /
  sampling / streaming — never read `Size()` bytes into memory.
- **Encoding.** Search and replace operate on raw bytes; the query is encoded
  into the file's encoding first (`encodingx`). Regex is UTF-8 only.
- **SQL analysis caches** a `Summary` per file id on the `FileService`; tools
  that need offsets (extract, split, fixture, schema diff) require the dump to be
  analyzed first.
- **Always build the GUI exe with root `task build` or `wails3 build` from `src/`**, not a bare
  `go build`. The production task links with `-ldflags="-H windowsgui"`, which
  sets the Windows GUI subsystem; a plain `go build` produces a *console*
  subsystem binary that pops an extra terminal window next to the app. The task
  generates an ignored, versioned `.syso` resource from the tracked icon,
  manifest, and metadata templates and removes only that exact generated file.
