# RepoReach for macOS

RepoReach is a native SwiftUI management app over the ArtifactFS desktop service. Closing its window keeps the app and service running in the background; reopening RepoReach restores its window. There is no menu-bar item. Explicit Quit first unmounts virtual filesystems, then stops the service. Adopted and kept ordinary local checkouts remain available. If a virtual folder is still in use, the app stays running and explains what needs closing.

## Requirements

- macOS 13 or later for repository management, Apple Silicon or Intel. The bundled native filesystem requires macOS 26 or later.
- Xcode 26 with the real macOS 26 SDK and [XcodeGen](https://github.com/yonaskolb/XcodeGen) for the default source build. The `FSPathURLResource` API is absent from older SDKs; an availability check cannot make an older SDK compile the extension.
- The RepoReach filesystem extension must be signed with its actual FSModule provisioning profile and enabled by the user in System Settings. Complete SDK 26 builds pass in CI; installed extension activation and real mount validation remain release gates. See [native FSKit](../docs/reporeach/native-fskit.md).
- Earlier macFUSE builds require its **kernel backend**; see [platform setup](../docs/reporeach/platform-setup.md). The legacy project is retained for source checks and does not bundle the native filesystem. RepoReach never silently enables extensions or changes startup security settings.
- Bundled `artifact-fs` and the official GitHub CLI under `RepoReach.app/Contents/Helpers`. Release tooling builds/packages these tools; see `scripts/build-macos.sh` at the repository root.

## Build and test

```sh
cd native
xcodegen generate
xcodebuild -project RepoReach.xcodeproj -scheme RepoReachFSKit -configuration Debug -derivedDataPath ../build/native CODE_SIGNING_ALLOWED=NO build
xcodebuild -project RepoReach.xcodeproj -scheme RepoReachFSKit -destination 'platform=macOS,arch=arm64' -derivedDataPath ../build/native CODE_SIGNING_ALLOWED=NO test
```

On an older SDK, `xcodegen generate --spec project-macfuse.yml` generates only the management app, Finder extension, and pure bridge tests. The `RepoReach` scheme in that project checks these sources without substituting fake declarations for FSKit's macOS 26 APIs. Generate `project.yml` again before a production build.

Build the Go engine separately with `go build ./cmd/artifact-fs`. For development only, the app accepts `REPOREACH_ENGINE_PATH` and `REPOREACH_GH_PATH` pointing to local executables. A private `REPOREACH_STATE_DIR` and `REPOREACH_MOUNT_ROOT` can isolate development or lifecycle smoke runs; isolated runs never change normal mount preferences or Finder metadata. Normal release builds use only bundled helpers. No access token is passed on any command line.

A preview fixture is available only when explicitly launching with `--demo`. `--demo --screenshot /absolute/path.png` captures that fixture and exits. It never reads GitHub credentials, starts a service, mounts folders, or persists demo repository state.

The native control smoke exercises the actual compiled Go service with synthetic GitHub CLI output and private temporary state, without signing in or accessing GitHub:

```sh
swiftc -parse-as-library App/Models.swift App/EngineClient.swift Shared/ActionRoute.swift Tools/control-smoke.swift -o ../build/control-smoke
../build/control-smoke /absolute/path/to/artifact-fs
```

## Adding and choosing repositories

**Add Repository** accepts any supported Git remote URL or an existing local checkout, without signing into GitHub. The folder picker accepts ordinary or bare Git repositories. An ordinary checkout is adopted in place: its location, current branch or detached HEAD, index, staged and uncommitted work, untracked files, and Git settings are retained. It is never removed by **Free Up Space**. A remote URL or bare repository creates an on-demand virtual repository instead. GitHub sign-in remains an optional discovery convenience. Remote access uses Git's existing credentials and SSH agent; inline tokens, passwords, and credential-bearing URLs are rejected before the control command runs.

Use **Show in catalogue** in repository details or **Owners & organizations** in Settings to choose which virtual entries appear. Group settings also apply to manually adopted repositories with the same group. Hidden repositories remain registered in the app, retain cached data and local checkouts, and pause background downloading. Hiding an entry does not remove its ordinary local checkout from the host filesystem. Enabling a group does not override an individual repository's hidden setting. The **Hidden from catalogue** filter makes these entries easy to find again.

**Keep Downloaded** converts a virtual repository into an ordinary local checkout at its catalogue path, accessible after RepoReach quits. An adopted checkout is already local; keeping it only records that preference. The app opens registered `localPath` locations directly, including adopted checkouts outside the catalogue, and distinguishes **Adopted checkout** from **Local checkout**. This does not imply complete offline Git history, LFS, or submodule availability.

## Architecture and safety

- `App/RepositoryStore.swift` is the main-actor UI state and control layer.
- `App/EngineClient.swift` uses the engine's UNIX socket control CLI. Timeouts, API error decoding, and credential redaction are centralized.
- `App/EngineService.swift` owns the background subprocess, private state directory, and bounded startup log.
- `Shared/ActionRoute.swift` is the validated Finder-to-app command surface.
- `Shared/FinderStatusCache.swift` publishes metadata only using private atomic writes.
- `FinderExtension` supplies repository-level actions and badges without hydrating file contents. See its README for entitlement and release-validation details.
- `FSKitExtension` provides the bundled `com.enoughtools.reporeach.fskit` ExtensionKit module in `Contents/Extensions/RepoReachFSKit.appex`. A security-scoped path resource grants access to a private connection directory; binary reads and writes cross an authenticated UNIX socket in bounded chunks. The Go catalogue remains the source of filesystem state.

GitHub sign-in delegates to the bundled official `gh` device/web flow. Credentials stay with `gh`; RepoReach never reads or stores tokens. Catalogue metadata and mount settings persist in `~/Library/Application Support/RepoReach`.

The engine checks repository recoverability before freeing data and refuses unsafe operations. UI confirmation is additional context, not the safety boundary. Finder actions are independently validated against the current registered catalogue. The chosen catalogue and organization directories are ordinary host folders; virtual filesystems are scoped to individual managed repository entries and never shadow an adopted checkout.

Launch at login is an explicit user setting through Apple's `SMAppService`. Signing/notarization and installed Finder-extension behavior must be verified on a real macOS release installation. Compiling an extension is not proof that Finder has enabled it.

## Design

The deterministic icon artwork is generated by `Tools/generate-icon.swift`; run it from the native directory after editing the vector geometry.

Native colors mirror [Enough UI](https://github.com/enoughtools/enough-ui): ink, paper, white, indigo, fine rules, square edges, and proportional type. Native accessibility sizes, labels, keyboard navigation and system focus remain in use. This is an open-source Enough Tools product built on Cloudflare ArtifactFS.
