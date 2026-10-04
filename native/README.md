# RepoReach for macOS

RepoReach is a native SwiftUI management app over the ArtifactFS desktop service. Closing its window keeps the app and service running in the background; reopening RepoReach restores its window. There is no menu-bar item. Explicitly quitting the app gracefully stops its own service and unmounts its filesystem.

## Requirements

- macOS 13 or later, Apple Silicon or Intel.
- Xcode 16 or later and [XcodeGen](https://github.com/yonaskolb/XcodeGen) for source builds; release builds were validated with Xcode 16.4.
- [macFUSE](https://macfuse.github.io/) with its **kernel backend** installed and approved by the user before mounting. FSKit alone is not supported by this beta; see [platform setup](../docs/reporeach/platform-setup.md), including Apple silicon setup requirements. A detected installation does not prove its kernel backend is active. RepoReach never silently installs a filesystem driver or changes startup settings.
- Bundled `artifact-fs` and the official GitHub CLI under `RepoReach.app/Contents/Helpers`. Release tooling builds/packages these tools; see `scripts/build-macos.sh` at the repository root.

## Build and test

```sh
cd native
xcodegen generate
xcodebuild -project RepoReach.xcodeproj -scheme RepoReach -configuration Debug -derivedDataPath ../build/native CODE_SIGNING_ALLOWED=NO build
xcodebuild -project RepoReach.xcodeproj -scheme RepoReach -destination 'platform=macOS,arch=arm64' -derivedDataPath ../build/native CODE_SIGNING_ALLOWED=NO test
```

Build the Go engine separately with `go build ./cmd/artifact-fs`. For development only, the app accepts `REPOREACH_ENGINE_PATH` and `REPOREACH_GH_PATH` pointing to local executables. A private `REPOREACH_STATE_DIR` and `REPOREACH_MOUNT_ROOT` can isolate development or lifecycle smoke runs; isolated runs never change normal mount preferences or Finder metadata. Normal release builds use only bundled helpers. No access token is passed on any command line.

A preview fixture is available only when explicitly launching with `--demo`. `--demo --screenshot /absolute/path.png` captures that fixture and exits. It never reads GitHub credentials, starts a service, mounts folders, or persists demo repository state.

The native control smoke exercises the actual compiled Go service with synthetic GitHub CLI output and private temporary state, without signing in or accessing GitHub:

```sh
swiftc -parse-as-library App/Models.swift App/EngineClient.swift Tools/control-smoke.swift -o ../build/control-smoke
../build/control-smoke /absolute/path/to/artifact-fs
```

## Adding and choosing repositories

**Add Repository** accepts any supported Git remote URL or an existing local checkout, without signing into GitHub. The folder picker accepts ordinary or bare Git repositories. Adopting local committed data creates a separate virtual checkout; RepoReach does not move or edit the original folder or its staged and uncommitted work. GitHub sign-in remains an optional discovery convenience. Remote access uses Git's existing credentials and SSH agent; inline tokens, passwords, and credential-bearing URLs are rejected before the control command runs.

Use **Show repository in Finder** in repository details or **Owners & organizations** in Settings to choose what appears in the filesystem. Group settings also apply to manually adopted repositories with the same group. Hidden repositories remain in the app's catalogue, retain cached data and local work, and pause background downloading. Enabling a group does not override an individual repository's hidden setting. The **Hidden from Finder** filter makes these entries easy to find again.

## Architecture and safety

- `App/RepositoryStore.swift` is the main-actor UI state and control layer.
- `App/EngineClient.swift` uses the engine's UNIX socket control CLI. Timeouts, API error decoding, and credential redaction are centralized.
- `App/EngineService.swift` owns the background subprocess, private state directory, and bounded startup log.
- `Shared/ActionRoute.swift` is the validated Finder-to-app command surface.
- `Shared/FinderStatusCache.swift` publishes metadata only using private atomic writes.
- `FinderExtension` supplies repository-level actions and badges without hydrating file contents. See its README for entitlement and release-validation details.

GitHub sign-in delegates to the bundled official `gh` device/web flow. Credentials stay with `gh`; RepoReach never reads or stores tokens. Catalogue metadata and mount settings persist in `~/Library/Application Support/RepoReach`.

The engine checks repository recoverability before freeing data and refuses unsafe operations. UI confirmation is additional context, not the safety boundary. Finder actions are independently validated against the current registered catalogue. Changing mount roots requires an empty destination; mounting never shadows an existing workspace.

Launch at login is an explicit user setting through Apple's `SMAppService`. Signing/notarization and installed Finder-extension behavior must be verified on a real macOS release installation. Compiling an extension is not proof that Finder has enabled it.

## Design

The deterministic icon artwork is generated by `Tools/generate-icon.swift`; run it from the native directory after editing the vector geometry.

Native colors mirror [Enough UI](https://github.com/enoughtools/enough-ui): ink, paper, white, indigo, fine rules, square edges, and proportional type. Native accessibility sizes, labels, keyboard navigation and system focus remain in use. This is an open-source Enough Tools product built on Cloudflare ArtifactFS.
