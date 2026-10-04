# Beta validation record

This record describes the checks performed for RepoReach 0.1.0-beta.1 on October 4, 2026. It distinguishes source, integration, and packaging checks from installation acceptance. The release manifest records the exact source revision, toolchains, artifact hashes, and signing status.

The beta.1 checks below are historical. Manual source adoption and repository/owner visibility controls are covered separately in the [beta.2 source validation](#beta2-source-validation) appended below.

## Development environment

- macOS 15.4.1 on Apple Silicon, Xcode 16.4, and Go 1.26.8.
- Git 2.53.0 for package checks, including upstream SHA-256 repository tests. The host's older Apple Git does not support those transport tests.
- Disposable Linux ARM64 containers on OrbStack Linux 7.0.5, with a real `/dev/fuse` device, FUSE 3.14.0, Git 2.39.5, local fixtures, and the pinned Go 1.26.8 image. The host did not receive a filesystem driver or security-policy change.
- Node.js 22.22.0, Astro 7.3.2, and Enough UI 0.4.0 from the website lockfile.

## Passed checks

| Check | Evidence and scope |
| --- | --- |
| Engine build, vet, and package tests | Required checks run in order across the full Go module. |
| Race detection | Catalogue, desktop service, daemon, registry, filesystem, Git store, and overlay packages. |
| Mounted filesystem suite | Mount smoke plus all 25 functional end-to-end tests and 33 subtests pass on Linux. Coverage includes lazy catalogue browsing, owner isolation, binary reads, writes, ordinary Git workflows, merge/rebase, branch switches, and restart persistence. The external-repository benchmark remains opt-in. |
| Mounted stress | Twenty catalogue repetitions and twenty concurrent directory-rename repetitions pass. Each rename iteration completes 40 moves with concurrent reads and Git status, preserving content and inode identity. |
| Native builds and tests | Apple Silicon and Intel compile; all 11 native XCTest cases pass. |
| Swift and Go integration | The real local service and Swift client pass status, duplicate-start refusal, fixture device authorization, discovery, folder migration, operation/error decoding, missing-driver handling, and shutdown cleanup. |
| Linux process cancellation | Ten inherited-output regression repetitions finish in 30–40 ms without changing their timeout assertions; the full Linux desktop race suite passes. Child diagnostics connect directly to the null device so orphaned processes cannot retain an internal stderr-copy pipe. |
| Background lifecycle | An isolated live native app keeps its service responding after the window closes, restores the window on reopening, and cleans up its owned service/socket after explicit Quit. |
| Website | Locked build, formatting check, desktop/mobile visual inspection, and interactive demo controls. |
| Engine vulnerability scan | `govulncheck` 1.8.0 on Go 1.26.8 reports no known vulnerabilities in the engine source's reachable code. |
| Bundled GitHub CLI review | Exact 2.102.0 source scan finds no imported or called vulnerabilities. Its stripped binary produces a conservative module-level advisory; [the raw finding and disposition](github-cli-security.md) are retained. |
| Website dependency scan | `npm audit` reports zero known vulnerabilities for the locked dependency tree. |
| Dependency notices | Engine notices collected; both official GitHub CLI architectures yield matching sets covering 165 modules, 219 module notice files, and five Go standard-library/vendor notices. |

Archive hashes, source provenance, signatures, and notarization flags are recorded in each release's manifest. Dependency scans describe the database at the time of the scan and do not establish absence of unknown vulnerabilities.

## Remaining installation acceptance

The development Mac has no approved macFUSE kernel backend. The following checks remain unperformed and must not be inferred from Linux mounts, compilation, unit tests, or code signing:

- A real macOS mount and ordinary editor/Git workflows on that mount.
- Installed, signed Finder extension registration, sandbox access, badges, and actions.
- A downloaded, quarantined build's first launch through Gatekeeper on a clean Mac.
- Real GitHub OAuth authorization and first private-repository acquisition with that account.
- Runtime coverage across every supported macOS version and both architectures.

These are beta acceptance tasks. Follow [platform setup](platform-setup.md) and use disposable repositories before valuable local work. Developer ID signing is separate from notarization; the first beta is signed but unnotarized.

## Product limits

Keep Downloaded covers the current committed tree. It refuses submodules and checkout filters such as Git LFS and does not guarantee complete history offline. File previews, indexing, and metadata requests for unknown file sizes can trigger downloads. Remote refresh is explicit and preserves the current branch and staged index. Free Up Space conservatively refuses local work it cannot prove recoverable, and temporarily disconnects the catalogue during reclamation; a busy mount is refused.

GitHub discovery uses the official CLI's shared credential store and OAuth permissions. Later manual-source support uses native Git/SSH authentication without requiring that authorization. See [privacy and authentication](privacy-auth.md). The current transport requires macFUSE's kernel backend; its FSKit backend is not implemented.

## Beta.2 source validation

The following checks passed on October 4, 2026 for the source intended for `0.1.0-beta.2`. They add evidence for manual sources, persistent visibility controls, and their native UI. They do not change the beta.1 results above or establish that beta.2 packaging is complete.

| Check | Evidence and scope |
| --- | --- |
| Engine checks | Final required checks pass in order: `go build ./cmd/artifact-fs`, `go vet ./...`, and `go test ./...`, using Go 1.26.8 and Git 2.53.0. |
| Actual Linux FUSE suite | In a disposable OrbStack Linux container using pinned Go 1.26.8 and a real `/dev/fuse`, the mandatory mount smoke passes in 0.850 seconds. All 26 functional top-level tests and 33 filesystem/Git subtests pass in 37.668 seconds. The external-repository benchmark is skipped. |
| Manual adoption and visibility integration | The new `TestE2EDesktopAdoptionAndVisibility` passes in 1.43 seconds through an external desktop CLI process, its private Unix socket API, and a real Linux mount. It runs signed out without invoking the GitHub CLI. A local checkout containing dirty, staged, untracked, and ignored files remains unchanged, including file bytes/modes, index, refs, and configuration. The separate virtual checkout exposes committed text/binary content with its own Git `HEAD`, index, and clean status. |
| Lazy preparation and retained handles | The same integration test checks catalogue/owner/repo browsing without acquisition, Keep Downloaded pinning, hidden repo/group entries on fresh lookups, continued reads through an already open file, group restoration preserving individual exclusions, and policy/mount restoration after service restart. |
| Mount cancellation regression | The integration suite exposed a parent-context cancellation race in mount ownership. After the fix, the full mounted suite above passes, including external-service restart. The existing timestamp-restoration test retries only bounded POSIX `EINTR` and retains exact content, timestamp, and dirty-status assertions. |
| Native app | The universal native build and all 33 XCTest cases pass. An actual Swift-client/Go-service API smoke and an isolated native UI smoke also pass. These exercise the control service and UI without a macOS filesystem mount. |
| Race detection | Narrow race tests for metadata alias rejection and inherited Git repository-binding environment safety pass. The complete desktop package race suite also passes after the final resolver changes in 11.456 seconds. |
| Website release rendering | The rendered release regression passes for the production release version, exact Apple Silicon/Intel DMG and ZIP links, and signing states. Missing metadata retains the preview; malformed metadata refuses the build. The test restores real release metadata and leaves existing production HTML unchanged. |

Release completion still requires packaging verification against the frozen source revision. The beta.1 results above remain a separate historical record.

The signed-out integration uses local Git fixtures. It does not establish real HTTPS/SSH credential acceptance against a private server, real GitHub OAuth, or first private-repository acquisition. No macFUSE driver or host security-policy change was made. A real macOS mount, installed Finder extension behavior, clean-Mac Gatekeeper launch, and runtime coverage across supported macOS versions/architectures remain unverified, as listed above.

Beta.2 is intended to be Developer ID signed and unnotarized. Final artifact hashes, source revision, signatures, and notarization flags must be checked in its release manifest; source tests and native compilation do not establish those packaging facts.
