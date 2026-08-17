# Support, installation, and release status

## Current release status

Quarry does not currently have a supported public production release. The
configured tag workflow is for release-candidate validation only. When
produced, Windows and Linux outputs are unqualified, non-release GitHub Actions
evidence and are not published to a GitHub release. macOS is excluded because
the secure handle-bound atomic-output backend required by artifact-producing
workflows is not implemented there.
This containment stays in place until all of the following exist:

- an approved repository `LICENSE`;
- signing and platform verification for every candidate target;
- authenticated checksums or provenance attestations;
- packaged-application safety tests on clean target machines; and
- a reviewed upgrade and compatibility policy.

Do not redistribute internal validation artifacts as a Quarry release.

## Configured validation matrix

| Target | Configured CI/development checks | Public support status |
| --- | --- | --- |
| Windows 10/11 x64 | Primary development target; CI is configured to run native tests and a Wails build and, for eligible tags, retain an unqualified `quarry_VERSION_windows_amd64_nsis.exe` candidate | Not yet supported for public distribution; the installer and payload are intentionally unsigned and have not passed clean-machine lifecycle qualification |
| macOS | No release-candidate build or retained artifact is configured; secure atomic output currently fails closed on this platform | Unsupported for artifact-producing use; it must not enter the candidate matrix until a native backend and destructive-lifecycle tests exist |
| Ubuntu 24.04 x64 | CI is configured to run a native GTK 4/WebKitGTK 6 build and race tests and, for eligible tags, retain an unqualified `quarry_VERSION_linux_amd64.tar.gz` candidate | Not yet supported; package installation and broader distribution coverage remain |
| Windows ARM64 and other Linux distributions | Build scaffolding exists, but no release-validation job is configured | Unsupported |
| Android or iOS | Not part of the desktop product; root tasks do not expose them, direct platform package/release/deploy tasks fail before output work, and Android release variants are disabled at the Gradle layer | Unsupported; debug/emulator scaffolding is development-only and must not be distributed |
| Network-server mode | Server-tag builds exit before starting the runtime | Unsupported |

The matrix records configured checks, not evidence that any check ran or passed
for the current commit, and not a promise that a candidate is ready for
important data. Qualification requires retained, reviewed run evidence. Only
platforms with a functional source-safety backend, signed artifacts, and
clean-machine tests will move to public support.

## Windows NSIS validation-candidate contract

The retained Windows artifact is deliberately limited to an NSIS machine-wide
installer carrying an AMD64 payload. Its installer guard permits Windows 10 and
Windows 11 on native x64 machines; this is the range to be exercised during
qualification, not a declaration that either operating system is publicly
supported. Windows ARM64, 32-bit Windows, MSIX, portable ZIP installation, and
per-user installation are outside this candidate workflow.

The candidate installs the program under
`%ProgramFiles%\RCooLeR\Quarry`, creates Start-menu and desktop shortcuts, and
adds an Apps & Features uninstall entry. Quarry does not currently register any
file association or custom URL protocol, and `quarry.exe` does not accept a
source path from the command line. Launch Quarry first and use **File > Open**;
Windows **Open with**, drag-to-executable, and shell/terminal file-open flows are
not advertised or qualified.

Quarry requires the Microsoft Edge WebView2 Evergreen Runtime. The candidate
contains Microsoft's online bootstrapper, not an offline runtime. The build
rejects the bootstrapper unless Windows validates its Microsoft Authenticode
signature and records its SHA-256 and signer in the candidate evidence JSON. On
a machine without WebView2, installation therefore requires network access and
permission to install the runtime. If the bootstrapper fails or the runtime is
still not detectable afterward, the installer exits with an error before
copying Quarry, creating shortcuts, or writing Quarry uninstall metadata. The
candidate does not remove the shared WebView2 Runtime during uninstall.
The runtime registry sentinel `0.0.0.0` is treated as absent in both machine and
user scopes before and after bootstrap, rather than as a usable installation.
The moving online bootstrapper is not yet pinned to an approved digest
allowlist, so Authenticode verification is containment rather than final
release provenance.

Uninstall removes only the Quarry executable, Quarry shortcuts, and Quarry's
uninstall registration. It deliberately preserves backend state under
`QUARRY_HOME` or `~/.quarry`, the operating-system-managed WebView profile and
origin-local preferences, source files, source-adjacent recovery evidence, and
any unknown files found in the install directory. The install directory is
removed only when empty. Clearing preserved state is a separate, explicit user
operation after inspection; the uninstaller has no delete-user-data option.

The validation workflow verifies the installer and embedded payload product and
numeric versions, asserts an AMD64 payload, asserts that both remain unsigned,
pins and records the NSIS compiler version, binds evidence to the verified tag
commit, and emits an individual SHA-256 sidecar. This improves candidate
traceability but is not a substitute for signing or clean-machine tests with
WebView2 present, absent, offline, and installation-denied.
The `windows:sign:installer` task fails before build or signing work: an outer-
only signature would leave the installed payload unsigned. It must stay
disabled until a qualified pipeline signs the payload, rebuilds/finalizes the
installer, and then signs and timestamps the outer installer.

## Linux validation boundary

The raw `linux:build` task remains available for local and CI validation, but
its target CPU architecture must match the host. The Docker fallback contains
native GCC and GTK/WebKit libraries rather than a cross-architecture sysroot,
so the task rejects mismatched `ARCH` values before frontend or container work.
Every Linux package or signing entry point (`package`, AppImage, deb, rpm, Arch,
and their generate/sign helpers) fails before build, download, or output work.
The legacy AppImage helper also exits immediately; it no longer downloads or
executes the mutable `linuxdeploy` `/continuous/` artifact.

Do not treat the raw Linux tar candidate as an installable package or supported
distribution. Packaging can be re-enabled only after the complete package
toolchain is pinned to reviewed immutable digests, the project has an approved
license and signing/provenance policy, and AppImage/deb/rpm installation,
upgrade, uninstall, dependency, and signature behavior pass on the declared
clean-machine distro matrix.

## macOS distribution boundary

The raw `darwin:build` task and the local `darwin:run` developer wrapper remain
available for source-level validation. Every macOS package, application-bundle,
signing, and notarization task fails before build, output, credential, or
network work. The root `package` task dispatches to that same fail-closed
platform guard when it runs on macOS; it cannot construct an `.app` bundle as a
fallback. The local run wrapper's ad-hoc-signed `.dev.app` is not a package or a
redistributable artifact.

Do not distribute a raw macOS binary or the local `.dev.app`. Distribution can
be reconsidered only after the secure atomic-output backend is implemented and
qualified on macOS, an approved license and signing/provenance policy exist,
and package signing, notarization, Gatekeeper behavior, installation, upgrade,
and uninstall pass on the declared clean-machine macOS matrix.

## Contributor installation

The only documented installation path today is a source build using the pinned
toolchain in [Getting started](getting-started.md):

```sh
task build
```

This produces a development/validation binary under `bin/`. On Linux, GTK 4 and
WebKitGTK 6 runtime libraries are required. A macOS source build is not a
supported Quarry application: secure artifact publication is unavailable and
write-producing operations fail closed. Do not distribute it or advise users to
bypass operating-system security controls.
The retained Windows NSIS file remains unsigned CI evidence, not a supported
installer. Linux and macOS package and signing tasks intentionally fail closed
as described above; only raw builds and local development run wrappers are
available for validation.

To remove a source build, close Quarry and delete only the binary or app bundle
that you built. App-owned state is intentionally not removed automatically; see
the data-location section before deleting it.

## Upgrades

There is no supported cross-version upgrade contract yet. For development
builds, close Quarry before replacing the executable and preserve
the app-data directory until the new build has opened successfully. Quarry does
not manage, relocate, or rewrite source files during an upgrade.

Never use an upgrade procedure that deletes adjacent `.qrp`, manifest, backup,
or temporary artifacts without first determining whether they are recovery
evidence for an interrupted operation.

## Interrupted atomic writes

An adjacent `.quarry.atomic.json` file and its operation-bound temporary or
backup files are recovery evidence, not instructions that an ordinary read may
execute. Journal checksums detect corruption but do not authenticate who created
an adjacent file. Public recovery inspection, application settings/rule/manifest
reads, and document opens therefore preserve the evidence and block until the
state is resolved; they never rename or delete those paths automatically.

Only the same still-running atomic-write call that published the complete
journal and retains its expected payload in memory may finish that transaction.
For an overwrite, it keeps the old destination pathname present, creates a
verified same-filesystem hard-link backup, and performs one atomic replacement.
Journaled generations and recovery-artifact hashing are limited to 64 MiB; this
primitive is deliberately for bounded editor/settings data, not huge-file
streaming output. Oversized or growing adjacent artifacts fail closed.
If hard links are unsupported, if pre-publication revalidation observes a
competing generation, if the destination is missing, or if evidence no longer
matches, the operation fails closed and keeps the artifacts for inspection. The portable
final verify-to-rename interval still has a narrow hostile path-replacement race;
platform conditional-replace/handle-relative qualification remains required.

Quarry does not yet provide a qualified keep-old/keep-new recovery workflow for
these states. Do not delete, rename, or restore adjacent recovery artifacts based
only on their filename or checksum. Preserve the entire set for inspection. A
five-phase killed-process regression covers the observable atomic-overwrite
namespace states, but a complete fine-grained kill/power-loss and filesystem
compatibility matrix is still required before post-crash automatic recovery can
be supported.

## Data locations and privacy

Backend settings, bounded index caches, and logs live below `QUARRY_HOME` when
that environment variable is set; otherwise the root is `~/.quarry` on every
platform. Known entries include:

- `settings.json` for validated backend preferences;
- `cache/indexes/` for bounded, disposable line-index caches; and
- `quarry.log` plus one rotated log for bounded operational diagnostics.

The desktop webview also has an operating-system-managed profile. Quarry stores
theme preferences there and, only after explicit opt-in, may store recent-file,
bookmark, and session paths in origin-local storage. Path memory and automatic
session restore are separate controls. The privacy controls in the **File** menu
can disable future path persistence or clear the path records Quarry can access.
Current open tabs are not closed by clearing stored history.

Opted-in path records are plaintext local data, not encrypted secrets. If the
webview or operating system denies storage deletion, Quarry reports incomplete
cleanup and prevents stale callbacks from writing new path records, but it
cannot guarantee erasure of browser-managed bytes it cannot access.

Source-adjacent outputs, backups, recovery manifests, and `.qrp` evidence do not
belong to the disposable cache. Inspect them before deletion. Deleting
`cache/indexes/` is safe while Quarry is closed; deleting the full app-data root
also removes settings and diagnostics and should be an explicit user action.

## Artifact verification

Unqualified workflow evidence is retained for CI diagnosis. A future
public release must provide an authenticated verification method; a bare
`SHA256SUMS.txt` served beside unsigned binaries is not sufficient to establish
publisher identity. The release workflow therefore has no public publication
credential while signing, notarization, licensing, and provenance remain open.

Each non-release candidate currently includes a `.sha256` sidecar, and the workflow
stores separate release-provenance and exact-commit verification JSON records.
These identify the annotated tag object, source commit/tree, tagged commit date,
toolchain pins, and dependency-input hashes. They make internal candidates
traceable and catch transfer corruption; they are deliberately not described as
authenticated attestations. Signing identities, protected publication
environments, an approved license/notices bundle, and clean-VM
install/upgrade/uninstall evidence remain external release gates.
Every downloaded candidate bundle also contains
`UNQUALIFIED-NON-RELEASE-CI-EVIDENCE.txt`; workflow artifact names begin with
`unqualified-non-release-ci-`. These labels are part of containment, not a support
or release-readiness claim.
