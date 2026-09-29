# Project audit — 20 September 2026

## Scope and conclusion

This review covers the current application under `src/`, its desktop dependency
graph, the source-preserving read/edit/export paths, frontend responsiveness,
and build/release controls. The working tree already contained the move into
`src/`; that work was preserved. This report describes the resulting changes,
not a clean-commit or public-release qualification.

Quarry has a sound large-file architecture: bounded windows cross the native
bridge, file operations are separated from presentation, and destructive or
unqualified capabilities fail closed. The principal problems found were at
representation boundaries: UTF-8 chunk seams, BOMs inside selected ranges,
sampled CSV types applied to later records, and frontend serialization. These
can produce incorrect results even when memory limits and atomic writes work
correctly. Focused regressions now exercise those boundaries.

The desktop runtime and build dependencies were brought to current compatible
releases. This does not make the application release-ready: unsupported
platform publication, licensing/signing, and large-file packaged-app
qualification remain explicit outstanding work.

## Architecture assessment

| Area | Existing design and assessment |
| --- | --- |
| Document reads | `internal/document` uses file handles, bounded range reads, and an LRU cache. Default chunks are 64 KiB and the default per-document cache is 8 MiB. Whole-file frontend state is avoided. |
| Navigation/indexing | Sparse indexing is asynchronous and cancellable. The ordinary anchor budget is 250,000 entries and the stride grows for larger files. Indexing still scans the input; byte navigation need not wait for it. |
| Editing | `manualedit` owns source-bound staged pieces, revisions, undo history, and expected-byte validation. Default budgets include 16 MiB of live inserted text, 48 MiB of history, 192 MiB of transient accounting, and 4,096 edits. Accounting budgets are not measured process-RSS ceilings. |
| Output | Save-copy and transforms use new destinations with source validation and no-clobber atomic publication. In-place save remains disabled. Recovery evidence must not be silently consumed during ordinary reads. |
| Search | Chunked literal/regex scans have explicit pattern, chunk, hit, and preview budgets. Literal patterns are capped at 1 MiB; collected results at 10,000 hits and a separate 64 MiB retained-result budget. |
| Bridge | Quarry retains its own admission guard over Wails: per-body, chunk, pending-upload, concurrency, and aggregate working-memory limits. Wails beta.23's new ordinary-request cap does not replace those aggregate controls. |
| Jobs/lifecycle | A single registered background job, cancellation ownership, document leases, generation checks, and serialized publication boundaries prevent stale work from publishing as if it were current. Existing lifecycle/publication tests remain essential. |
| CSV | Logical records, field counts, samples, and configuration collections are capped. A record may have at most 1,024 fields. Inference is advisory; every later value must satisfy the generated output's type contract. |
| SQL | Streaming analysis retains exact supported statement regions and rejects ambiguous syntax. Extracted regions are not promised to be standalone database imports. Generic replacement and token-unsafe presets remain disabled. |
| Frontend | CodeMirror holds bounded text windows; expensive workbench views load on demand. Generation/epoch checks suppress stale responses. Byte positions and UTF-16 editor positions need explicit conversion. |
| Delivery | Go/npm pins, lock checks, frontend budgets, static/security checks, immutable Action references, and nonpublishing candidate workflows are strong controls. The current Windows/Linux matrix is configured validation, not public support evidence. |

The separation between engine, services, and UI is useful, but the broad
`FileService` and large editor/App modules require careful boundary tests.
Refactoring them solely for size would add risk during a dependency upgrade;
extract responsibilities when a concrete behavior change needs it.

## Confirmed defects and fixes

| Priority | Trigger and previous behavior | Correction |
| --- | --- | --- |
| High | A UTF-8/UTF-16 edit window or copied range began at a literal U+FEFF inside a source. Generic decoding treated it as a file BOM and removed it. | Source-offset-aware decoding strips BOM metadata only at byte zero. Mid-file U+FEFF survives decoding and unchanged-window round trips. |
| High | Malformed UTF-16 surrogate sequences entered editable windows, or invalid UTF-8 strings entered the encoding writer. A conversion could silently replace malformed text. | Strict edit decoding and encoding reject malformed input. Best-effort read-only display retains its separate behavior. |
| High | Invalid UTF-8 occurred in a CSV header or value exported to JSONL. Go's JSON encoder could replace bytes silently; distinct invalid headers could become duplicate JSON keys. | Export rejects invalid UTF-8 before publication and preserves existing source/output safety guarantees. |
| High | A CSV integer exceeded signed 64-bit range. Inference fell through to floating point, permitting precision loss when imported. | Out-of-range integer values are inferred as text. Go-only numeric literal forms are not advertised as portable SQL numbers. |
| High | Rows after the bounded inference sample contained invalid or out-of-range numeric values. Generated BIGINT/DOUBLE output could be rejected or coerced by a database. | Full streaming conversion validates inferred numeric types on every record and aborts atomic publication on mismatch. |
| High | Leading-zero identifiers appeared after a numeric inference sample. They could be imported as BIGINT and lose their zeros. | Automatically inferred BIGINT refuses late leading-zero values; explicitly configured numeric types retain the caller's chosen behavior. |
| Medium | Whole-word literal search ended before punctuation or emoji split across two reads. | Boundary evaluation retains enough UTF-8 seam context to return the valid match. |
| Medium | A forward literal scan reached EOF before the reader's declared source size. | It reports the short read rather than reporting a successful complete search. |
| Medium | Cancellation arrived while many literal matches were being emitted within one chunk. | Candidate loops check cancellation and return the cancellation error promptly. |
| Medium | ASCII-insensitive search saw a long repeated-prefix pattern in repetitive data. The naive matcher repeatedly compared the same bytes. | Adaptive KMP fallback bounds the expensive matching path; common searches retain their inexpensive allocation-free path. |
| Medium | CSV profiling sorted and retained all distinct value/count pairs just to return ten. A short subslice retained the full backing allocation. | Deterministic bounded top-ten selection avoids the full sort and full result allocation. Distinct-count maps retain their existing caps. |
| Medium | A CSV report stopped at its row budget, or the service had already cut a byte sample, without marking the result incomplete. | Row and source-sample truncation are propagated through profile/schema/preview results. Ragged late columns also account for earlier missing cells. |
| Medium | A direct CSV plugin caller supplied an oversized list of null sentinels, bypassing service-level normalization bounds. | Schema normalization applies collection/string limits at the plugin boundary. |
| Medium | CodeMirror ingested carriage returns in an LF edit session and normalized them when serializing the full window. | Explicit LF line separation preserves CR code units in pasted or late source content. This does not broaden the backend's supported editing encodings/newlines. |
| Medium | End navigation in an edit session read the original file tail and ignored staged changes. | Edit-tail navigation uses staged size and a bounded edit window, with regression coverage for edits near EOF. |
| Medium | A short final hex window fit entirely inside its viewport, so no scroll event could navigate back to the previous window. | A pinned-edge wheel handler requests the adjacent bounded hex window. |
| Medium | A deferred CSV grid scroll-reset callback ran after a user scroll and jumped back to row one. A fresh-install build reproduced this race. | Scroll anchoring runs during the committed layout phase before input/paint, with a deterministic queued-frame regression. |
| Performance | Match highlighting converted UTF-8 positions with repeated TextEncoder allocation and two codepoint passes. | One allocation-free codepoint pass maps byte boundaries to CodeMirror's UTF-16 positions, including supplementary characters. |

These are confirmed issues, not a claim that every possible bug has been found.
Individual changes and their focused tests live beside the affected source.

## Dependency updates

Versions were checked against the Go module proxy, npm registry, upstream
release documentation, and container registries on the audit date. Exact pins
and integrity checks remain in place.

| Dependency/tool | Before | After |
| --- | --- | --- |
| Go toolchain | 1.27.0 | 1.27.1 |
| Node.js, canonical LTS | 24.20.0 | 24.21.0 |
| Wails Go module, CLI, frontend runtime | 3.0.0-beta.15 | 3.0.0-beta.23 |
| `golang.org/x/sys` | 0.47.0 | 0.48.0 |
| `golang.org/x/text` | 0.41.0 | 0.42.0 |
| CodeMirror commands | 6.11.0 | 6.11.1 |
| CodeMirror state | 6.7.1 | 6.7.5 |
| CodeMirror view | 6.43.9 | 6.43.12 |
| React / React DOM | 19.2.8 | 19.3.0 |
| React type definitions | 19.2.18 | 19.3.0 |
| React DOM type definitions | 19.2.5 | 19.3.0 |
| jsdom | 30.0.1 | 30.1.0 |
| Vite | 8.2.2 | 8.3.0 |
| Vitest | 4.1.11 | 5.0.1 |
| govulncheck | 1.7.0 | 1.8.0 |
| GoReleaser | 2.18.0 | 2.18.2 |
| Go cross-build image | 1.27.0-trixie | 1.27.1-trixie, registry digest pinned |
| Ownership helper image | Alpine 3.23.5 | Alpine 3.24.2, registry digest pinned |

Already current and retained: npm 12.0.2, TypeScript 7.0.2, CodeMirror language
6.12.4 and merge 6.12.2, React Vite plugin 6.1.1, axe-core 4.13.0,
staticcheck 0.8.1, actionlint 1.7.12, Zig 0.16.0, and the existing latest
tagged macOS SDK archive 26.1. The six remaining explicitly required indirect
Go runtime modules are current. Wails no longer requires `go-winloader`; tidy
removes it from Quarry's required graph. The npm lockfile was refreshed with
the pinned npm release.

The current GitHub Actions releases already match the repository pins:
checkout 7.0.1, setup-go 7.0.0, setup-node 7.0.0, upload-artifact 7.0.1, and
goreleaser-action 7.2.3. Their full commit SHA references were retained.

### New capabilities used

- Wails beta.23's Linux `ApplicationID` now uses `com.rcooler.quarry`, matching
  product metadata. Explicit `ProgramName: "quarry"` preserves association with
  the existing `quarry.desktop` file. New Wails request-cancellation, native
  memory-lifetime, and Windows request-failure fixes are inherited by the update.
- Vite 8.3's explicit `tsconfig` configuration is used for deterministic project
  configuration selection.
- Vitest 5 runs the existing and expanded test suite. An upgrade is not treated
  as sufficient reason to replace working test semantics or introduce a new
  frontend compiler. Its VM worker environment reuse is enabled with at most
  four concurrent workers and per-file isolation. In local comparisons this
  reduced the upgraded suite from approximately 9.77 to 2.88 seconds.

The Go module, Wails generator, and browser runtime were upgraded together;
bindings were regenerated. The runtime still uses the 512 KiB chunk protocol
expected by Quarry's guard. Aggregate transport limits remain necessary.

### Deliberate exclusions

There is no blanket claim that every version string in dormant scaffolding is
now the newest release. Android/iOS are unsupported and cannot produce release
artifacts through the project tasks. Android still contains AGP 8.7.3, Gradle
9.2.1, and old AndroidX/Material pins; current AGP is a major migration and needs
Android SDK/emulator/JDK validation. These are a documented maintenance backlog,
not dependencies loaded by the desktop application.

Garble 0.18.0 now supports Go 1.27. The previous claim that no compatible tagged
release existed was corrected. Obfuscation remains disabled pending generated
binding and native-package qualification. The new release was not used as a
reason to enable an untested distribution path.

`go list -m -u all` also includes Wails CLI/optional-module graph entries that
Quarry does not import. They are governed by the pinned upstream CLI/module;
forcing all of them into Quarry's `go.mod` would not update `go install
wails3@version` and would create an artificial application dependency graph.

Upstream-constrained npm transitive major versions also remain within their
parents' declared ranges. No blanket overrides were added to force, for
example, Vitest's tinybench/tinyexec or PostCSS's nanoid onto incompatible
majors. The complete installed graph is audited, including these packages.

## Performance evidence and limits

The ASCII-insensitive matcher benchmark used 1 MiB of repeated `A` bytes and a
256-byte missing `a…ab` pattern. On this machine the old forward/backward scans
took approximately 87.5/90 ms; the bounded fallback took 1.24/1.28 ms, about
70 times faster for this deliberately adverse case. This is a matcher
microbenchmark, not a disk throughput or whole-application claim.

The CSV profile result now retains at most ten value/count entries per column
instead of a full distinct-entry result allocation. It still intentionally
retains bounded distinct-count maps while scanning. The frontend byte mapping
removes per-character temporary encodings; it still walks the bounded text
window once.

The top-ten selector benchmark with 50,000 distinct values measured about
0.57 ms, 296 bytes and 11 allocations per operation. The old value/count array
alone required approximately 1.2 MB and its returned subslice retained that
array. A separate frontend 1 MiB text/emoji mapping microbenchmark improved
from a median 508.06 ms to 1.885 ms over five runs on Node 24.20. These are
local microbenchmarks, not end-to-end UI latency guarantees.

Two intentional costs remain: reverse regex scans the prefix to preserve Go's
leftmost/nonoverlapping match semantics, and edit/undo/redo can replay active
history while repacking inserted text. Both are bounded in memory but can be
expensive in CPU or I/O. Exact source-generation hashing also scans input;
removing it would weaken save-copy safety. Measure these paths with realistic
workloads before changing their algorithms.

React 19.3 increases the framework bundle. Existing optional panels already
load lazily; splitting the mandatory startup render into another immediately
requested chunk would conceal the cost. The raw initial-JavaScript budget is
therefore revised from 320 to 352 KiB and total JavaScript from 720 to 768 KiB.
The initial gzip budget remains 110 KiB, individual chunks 320 KiB, entry
280 KiB/90 KiB gzip, and initial CSS 64 KiB. These limits are enforced by the
production build, not only documented here.

Final production bundle: entry 134,353 bytes (39,915 gzip), initial module
graph 353,902 bytes (107,726 gzip), largest chunk 243,111 bytes, all JavaScript
749,294 bytes, and initial CSS 29,496 bytes. The gzip margin is narrow and
should be watched on future framework upgrades.

No 400 GB workload was run. The architecture supports bounded working sets,
but scan time, hashing, storage behavior, multi-file aggregate memory, and
platform cache effects still need representative cold/warm measurements.

## Validation

Tests ran locally on Windows x64 with Go 1.27.1, Node 24.21.0, npm 12.0.2,
and Wails beta.23. Native Linux/macOS behavior and signed installer lifecycle
are outside this machine's completed checks.

| Check | Result |
| --- | --- |
| Go default tests | All 37 packages containing tests passed. |
| Go production-tag tests | All 37 packages containing tests passed. |
| Go race tests | All 37 packages containing tests passed. |
| Frontend | 283 tests across 43 files passed; baseline was 267 across 41. |
| Frontend production build | TypeScript, privileged-WebView policy, Vite build, and all bundle budgets passed. |
| Windows native build | Production AMD64 executable with generated version/icon resources built successfully at `bin/quarry.exe` (15,015,936 bytes), using Go 1.27.1. This is an unsigned local validation binary. |
| Go static checks | `go vet` and staticcheck 0.8.1 passed for default and production builds. |
| Dependency integrity | `go mod verify` and `go mod tidy -diff` passed. |
| Vulnerability scans | govulncheck 1.8.0 found no vulnerabilities in default/production scans; npm audit reported zero vulnerabilities. |
| Workflow/release configuration | actionlint 1.7.12 and GoReleaser 2.18.2 containment checks passed. |
| Generator compatibility | Wails beta.23 regenerated two services and 67 methods successfully. |
| Fuzzing | ASCII matching: 3,545,222 executions; encoding round trips: 592,689 executions, both passed. |

New regressions cover the corrected UTF-8/UTF-16, source-generation, CSV
publication, navigation, and cancellation cases. A second independent review
of the new edited-tail backend/frontend path found no remaining concrete defect.
After the final CSV inference and tail refinements, the affected CSV
default/production/race tests and the complete root-service race suite were
rerun successfully. The final fresh-install frontend suite also passed after
the CSV scroll race fix.

This host intermittently leaves newly created temporary Go executables unable
to start. The reliable workaround was `go test -c -o <persistent directory>`
followed by direct execution of each binary from its package directory, with
explicit timeouts. Production and race binaries were compiled separately.
The same test packages were run; failures were not suppressed. Packages with
no tests were checked through compilation/static analysis. Locally installed
scanner binaries avoided the analogous `go run` launcher issue. Generated
test/tool outputs and per-package logs are under ignored `bin/`.

The root `task build` frontend/install/binding stages also passed after a fresh
`npm ci`. Its `go run ./build/releasemeta` stage encountered the same temporary
launcher issue. The helper was compiled to a persistent path and the identical
metadata/resource/native-build steps were then executed directly. No build
guard or source-template policy was bypassed. The global Go/Node/npm installs
were left intact; isolated audited tools were used through a process-local PATH.

## Remaining work, in priority order

1. Qualify the packaged Windows app with clean-machine open/edit/save-copy,
   cancellation, external modification, power-loss/recovery, denied access,
   full disk, and WebView2 lifecycle scenarios. Unit tests do not establish this.
2. Run native Linux CI and manual GTK/WebKit checks after the Wails update,
   including request cancellation and the configured application identity.
3. Establish realistic large-file performance baselines: first viewport latency,
   cold/warm sequential search, p95 navigation latency, peak RSS, cancellation
   latency, pathological single lines, UTF-16, many CSV columns, and multiple
   simultaneously open files. Record hardware and dataset generation commands.
4. Resolve the existing license, payload/installer signing, authenticated
   provenance, and supported upgrade policy before public distribution.
5. Implement and qualify a native macOS handle-bound atomic-output backend
   before admitting macOS to the supported artifact-producing matrix.
6. Treat Android dependency modernization, obfuscation, SQLite/XLSX exports,
   and in-place save as separate qualification tasks. Their existing guards
   should only be lifted with evidence for the complete workflow.

## Upstream references

- [Go release history](https://go.dev/doc/devel/release)
- [Node.js 24.21.0 LTS](https://nodejs.org/en/blog/release/v24.21.0)
- [Wails beta.23](https://github.com/wailsapp/wails/releases/tag/v3.0.0-beta.23)
- [Wails beta.20 request cancellation and loader changes](https://github.com/wailsapp/wails/releases/tag/v3.0.0-beta.20)
- [Vite 8.3](https://github.com/vitejs/vite/releases/tag/v8.3.0)
- [Vitest migration guide](https://vitest.dev/guide/migration.html)
- [Garble 0.18.0](https://github.com/burrowers/garble/releases/tag/v0.18.0)
- [Android Gradle plugin release notes](https://developer.android.com/build/releases/gradle-plugin)
