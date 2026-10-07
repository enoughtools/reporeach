# Contributing to EnoughRepos

EnoughRepos is an [Enough Tools](https://enoughtools.com) project built on Cloudflare ArtifactFS. Work happens at [github.com/enoughtools/reporeach](https://github.com/enoughtools/reporeach); product information lives at [enoughrepos.reb.run](https://enoughrepos.reb.run).

Read [AGENTS.md](AGENTS.md), [the architecture](docs/reporeach/architecture.md), and [the beta's actual behavior](docs/reporeach/user-guide.md) before changing lifecycle, hydration, or storage release. [UPSTREAM.md](UPSTREAM.md) identifies the ArtifactFS baseline, reused packages, and modified files. Keep attribution, change notices, and third-party license notices intact; mark further modified upstream files and update that map. The source uses Apache 2.0; bundled dependencies have their own licenses.

## Development requirements

- Go 1.26.8 and Git for the engine. Release tooling and CI pin this patch version; `go.mod` declares the minimum language version.
- Full Xcode 26 or later with the macOS 26 SDK or later, and XcodeGen, for the native app and bundled FSKit extension. The management app deployment target is macOS 13; virtual repositories require macOS 26.
- An installed, signed and enabled native app for [macOS FSKit acceptance](docs/reporeach/fskit-acceptance.md), or a usable `/dev/fuse` device for Linux mounted tests. Ordinary package tests do not need a mount.
- Node.js 22.14 or later and npm for the Astro website. Use its committed lockfile.
- Python 3 and macOS packaging tools for release scripts. [Release instructions](docs/reporeach/releasing.md) cover the bundled official GitHub CLI and signing requirements.

The native project is generated from `native/project.yml`; edit that specification instead of relying on local Xcode project changes. A complete app package must include its engine, official GitHub CLI, Finder extension, and FSKit extension. Building only the Swift target does not package those helpers. The Xcode project and schemes retain their internal `RepoReach` names; the compiled app is `EnoughRepos.app`.

## Validate a change

Start with the smallest relevant test, for example:

```sh
go test ./internal/catalogfs
go test ./internal/desktop
go test -run TestFreeRepositorySpaceRefusesLocalWork ./internal/daemon
```

For nontrivial engine changes, reproduce the core checks in order:

```sh
go build ./cmd/artifact-fs
go vet ./...
go test ./...
go test -race ./internal/gitstore ./internal/daemon ./internal/fusefs ./internal/overlay
```

Race testing requires a supported Go race toolchain and C compiler. New catalogue/control-plane concurrency changes should also run `go test -race ./internal/catalogfs ./internal/desktop`.

For native changes:

```sh
xcodegen generate --spec native/project.yml --project native
xcodebuild -project native/RepoReach.xcodeproj -scheme RepoReach \
  -configuration Debug -destination 'platform=macOS' \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO test
```

Some installation, extension, and Gatekeeper behavior must be validated on the packaged app; unit tests alone do not establish it. See [the release checklist](docs/reporeach/releasing.md#release-validation-checklist).

For website changes:

```sh
npm ci --prefix site
npm run build --prefix site
```

For a mounted filesystem change, use the opt-in end-to-end tests. They use a local disposable Git remote by default:

```sh
AFS_RUN_E2E_TESTS=1 go test -count=1 -run '^TestFUSEMountSmoke$' -timeout=2m -v .
AFS_RUN_E2E_TESTS=1 go test -count=1 -run '^TestE2E' -timeout=20m -v .
```

`scripts/fuse-e2e.sh` runs those checks on Linux with `/dev/fuse`, or in a disposable Docker container with the FUSE device and `SYS_ADMIN` capability. Its Docker path copies source from a read-only bind mount and installs FUSE in the container; it does not install macFUSE on the host. Do not set `AFS_E2E_REPO` to a real account's repo unless deliberately testing that remote. Benchmarks are opt-in; use the flags documented in [AGENTS.md](AGENTS.md).

## Meaningful regression coverage

Test the observable contract affected by your change. Useful regression cases include:

- Root and owner browsing never activates repos or downloads blobs; colliding repo names keep independent content and handles.
- Writes, deletes, renames, staging, committing, and branch switches preserve overlay and index state across restart.
- Downloading preserves binary content, deduplicates blobs, validates cached bytes, responds to cancellation, and refuses completion after a changed `HEAD`.
- Free Up Space refuses specified local work, unpushed/recovered history, locks, unsupported metadata, and external or symlinked storage; failures retain data.
- Malformed GitHub responses, unsafe paths, ambiguous action URLs, invalid status-cache files, and credential-like diagnostics are rejected or redacted.

Use local repos and mocked external calls wherever possible. Never commit access tokens or private repo fixtures, and never make routine tests require the contributor's GitHub account. Keep downloaded blob content binary-safe, use `model.CleanPath`, and preserve canonical `model` interfaces and `GIT_NO_LAZY_FETCH=1` size-resolution behavior.

## Pull requests

Explain the problem, final behavior, and validation performed. Report checks that were skipped and why, especially mounted macOS, Finder extension, and signed installation checks. Keep product copy aligned with the implemented beta behavior: do not describe current-tree downloads as full backups or imply that local edits are pushed automatically.

For a security issue or potential data-loss exploit, follow [SECURITY.md](SECURITY.md) before opening a public report. Maintainers should use [Building and releasing](docs/reporeach/releasing.md) when preparing distributable artifacts.
