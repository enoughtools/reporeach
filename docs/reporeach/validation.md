# Beta validation record

This record describes the checks performed for RepoReach 0.1.0-beta.1 on October 4, 2026. It distinguishes source, integration, and packaging checks from installation acceptance. The release manifest records the exact source revision, toolchains, artifact hashes, and signing status.

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

RepoReach uses the official GitHub CLI's shared credential store and OAuth permissions. See [privacy and authentication](privacy-auth.md). The current transport requires macFUSE's kernel backend; its FSKit backend is not implemented.
