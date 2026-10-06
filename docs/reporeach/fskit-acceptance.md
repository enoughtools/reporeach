# Mounted FSKit acceptance harness

`TestFSKitMountedAcceptance` exercises the native macOS 26 filesystem through real mounted paths and the desktop control service. It uses private temporary state, a private mount folder, and local Git fixtures. It does not install an app, activate an extension, update the OS, or change security settings.

This is a narrow acceptance harness, not complete native release qualification. A skipped test, a successful Go compilation, and bridge tests are not mounted FSKit evidence. The complete primary sequence passes at `8807963` with the signed validation module on macOS 27.0.1 ARM64, including native binary/empty extended attributes and persistence across visibility changes, remount and restart. Fresh cached kernel inspection confirms final detachment and both helper exits. This establishes that runtime, not a macOS 26 mounted pass. See [the native backend's remaining release gates](native-fskit.md).

## Prerequisites

- macOS 26 or later, with the `reporeach` FSKit module already installed and enabled in normal System Settings.
- An installed `RepoReach.app` containing `Contents/Extensions/RepoReachFSKit.appex`, whose compiled filesystem implementation matches the verified native source under test. Record separate app and helper revisions and any validation-only bundle-metadata changes as described below. Building the extension requires Xcode 26 or later and a macOS SDK at version 26 or later; Xcode 27 with its macOS 27 SDK is a valid local toolchain while retaining the module's macOS 26 deployment target. Installation also requires the appropriate signing and FSKit entitlement authorization.
- Native Git available on `PATH`, and Go 1.26.8 or toolchain download access.
- The app, Go engine and module claim the same Team-ID-prefixed app group. `AFS_FSKIT_SIGNING_IDENTITY` names the verified local certificate SHA-1 used by that installed module. The harness signs only its disposable copied Go test helper with that identity and group, and strictly verifies it before launch. It obtains the real group container through the installed app’s headless Foundation resolver; it never constructs a Group Containers path.
- Public FSKit discovery must identify the selected module identifier and exact module path as the sole enabled filesystem advertising `reporeach`. The read-only inspector fails on absent candidates, duplicate registrations, competing enabled modules, unreadable metadata, or incomplete output before the harness creates a fixture or asks to mount.
- A disposable test session in which you can retain the private fixture and helper process if normal cleanup is refused.

Set `AFS_FSKIT_APP` to the absolute path of that installed app. `AFS_FSKIT_MODULE_CONFIRMED=1` is your explicit assertion that the selected installed module matches the source being tested and is already enabled. Bundle metadata checks cannot prove source provenance or extension activation; an actual successful mount is required. Do not set the assertion merely because an app bundle exists.

The default module identifier is `com.enoughtools.reporeach.fskit`. An explicitly
approved isolated local experiment can use
`com.enoughtools.reporeach.validation.fskit` to avoid sharing the installed
release app's identity. Select that identity with
`AFS_FSKIT_MODULE_ID=com.enoughtools.reporeach.validation.fskit`; every other
override is rejected. This affects only the acceptance harness's expected
installed module identifier. Production identifiers, packaging and profile
authorization remain unchanged.

For that experiment, confirm the exact validation app path, its verified source,
any local bundle-metadata changes and the selected validation extension's normal
enablement before setting `AFS_FSKIT_MODULE_CONFIRMED=1`. Relabeling bundle
metadata, signing or registration alone does not establish enablement. Record
the native app's CI source revision separately from the test/helper checkout
revision when they differ, including the relevant differences; do not claim
source equality from the presence of an executable or a successful mount.

The harness normally builds [the read-only inspector](../../native/Tools/inspect-fskit-module.swift)
from the current checkout with local Swift, then queries the public
`FSClient.installedExtensions` API. Compilation is limited to 60 seconds and
inspection to 20 seconds; the inspector itself stops after 15 seconds and bounds
its JSON output. On this development host, the SDK 15.5 build reported only three
Apple modules even after owner enablement of RepoReach Validation. Host logs for
the unentitled inspector recorded a Team ID lookup failure before returning the
module set. A separate inspector signed with the existing Developer ID identity
for team `FGXHYHH9MC` also reported only those three Apple modules; the ad-hoc
validation extension has no Team ID. These observations make caller/signing
visibility a possible explanation, not a proven cause or a documented SDK defect.
Apple's [newer public mount API](https://developer.apple.com/documentation/fskit/fsclient/mountsinglevolume%28resource%3Abundleid%3Aoptions%3Acompletionhandler%3A%29)
acknowledges a caller's module-visibility boundary; the
[enumeration documentation](https://developer.apple.com/documentation/fskit/fsclient/fetchinstalledextensions%28completionhandler%3A%29)
does not specify a same-Team-ID rule or an SDK-based visibility difference.
They do not establish that the validation extension is disabled, absent, or
selected. The earlier system mount dispatcher did find the old module and check
its enabled state while public enumeration omitted it, so these are distinct
discovery paths. The incomplete inventory cannot satisfy this harness's
precondition and must not be bypassed.

An independently built inspector can be selected with `AFS_FSKIT_INSPECTOR`, which
must name its absolute executable path. Build it locally with the selected Xcode
26-or-later toolchain, or obtain the standalone SDK 26 executable exported by a
manual `validation_artifacts=true` CI run. A newer SDK is a diagnostic comparison,
not a promised fix for caller visibility. The separate `fskit-module-inspector-arm64`
artifact contains `inspect-fskit-module.zip`, `inspect-fskit-module.json`, and
`SHA256SUMS`. Before executing it, verify the ZIP hash, extracted executable hash,
clean source revision, architecture, SDK/Xcode versions, and
`inspectorSourceSha256` against the exact inspector source in the checkout under
test. Record these facts separately from the installed app's native revision.
This artifact is a read-only diagnostic and does not modify, sign, install, or
enable an app. A newer SDK build must still prove the exact selected module; its
compilation alone is insufficient. Even a positive public inventory is a
precondition rather than proof of which module the system mount dispatcher
actually executed; retain execution and mounted-behavior evidence separately.

For the requested local app build, first confirm that `xcodebuild -version` and
`xcrun --sdk macosx --show-sdk-version` select the newly installed toolchain rather
than Xcode 16.4/SDK 15.5. Xcode 27 and its SDK meet the required minimum of 26.
Then run `scripts/build-macos.sh --backend fskit --compile-only --arch arm64 --unsigned`
to validate the app and module locally. Xcode may register its unsigned app product during compilation. After a successful
build, the script checks the exact parent app and FSKit module registrations,
retires only that product when registered, and verifies both are absent before
returning or packaging. It does not enable the
extension, sign for distribution, or mount a filesystem. Record the actual local
toolchain and source revision; signing authorization and installed execution
remain separate requirements.

## Run

From the repository root:

```sh
GOTOOLCHAIN=go1.26.8 \
AFS_RUN_FSKIT_E2E_TESTS=1 \
AFS_FSKIT_APP=/Applications/RepoReach.app \
AFS_FSKIT_MODULE_CONFIRMED=1 \
AFS_FSKIT_SIGNING_IDENTITY=VERIFIED_LOCAL_CERTIFICATE_SHA1 \
go test -run '^TestFSKitMountedAcceptance$' -count=1 -v -timeout=20m .
```

The test skips unless explicitly opted in, and skips on an older macOS version. On a supported, opted-in host, missing prerequisites or a failed mount are failures, not passing evidence. It launches its own Go desktop helper against private state and uses the installed native extension for the mount.

For the approved isolated experiment, add the exact validation module selection
to the same invocation and use its absolute app path:

```sh
GOTOOLCHAIN=go1.26.8 \
AFS_RUN_FSKIT_E2E_TESTS=1 \
AFS_FSKIT_APP=/absolute/path/to/isolated/RepoReach.app \
AFS_FSKIT_MODULE_ID=com.enoughtools.reporeach.validation.fskit \
AFS_FSKIT_MODULE_CONFIRMED=1 \
AFS_FSKIT_INSPECTOR=/absolute/path/to/verified/inspect-fskit-module \
AFS_FSKIT_SIGNING_IDENTITY=VERIFIED_LOCAL_CERTIFICATE_SHA1 \
go test -run '^TestFSKitMountedAcceptance$' -count=1 -v -timeout=20m .
```

Both identities retain the same platform, source confirmation, installed
executable, extension-point, filesystem-name and path-resource prerequisites.
The harness never installs, enables or signs the selected app or extension; only its disposable Go test helper is signed.

The core sequence checks:

- Adoption of a local repository without GitHub sign-in or a `gh` invocation, with the original checkout preserved.
- Lazy reads of text and binary files, symlink targets, and executable files.
- Native Git status, staging, commits, and branch checkout with warmed file lookups.
- Native binary/empty extended attributes, create/replace/default policies, missing/remove errors, lists and short buffers; lazy catalogue metadata and symlink/gitfile isolation.
- Retained open-file reads and writes across rename and unlink.
- A busy visibility change refusing to publish, preserving the catalogue and mounted view; successful repository and organization hide/show changes using normal reconnects.
- Normal unmount/remount and desktop helper restart using the same FSKit resource directory, with fresh lookups of retained working-tree content and metadata.

Additional release gates include signing/notarization, both architecture slices, complete pin/free behavior, remote refresh, unsupported-file behavior, failure injection, and interrupted-session recovery. Passing this sequence does not establish those gates.

## Cold downloads and clean eviction

After recording the primary result and normal cleanup, run
`TestFSKitMountedColdStorageAcceptance` with the same explicit prerequisites and
environment, changing only the `-run` test selection. This is a separate disposable
fixture with a filtered loopback HTTP Git source. Before Keep, it verifies missing
Git blobs with `GIT_NO_LAZY_FETCH=1` and an empty cache without mounted repo lookups.
It then checks exact unique-blob accounting and offline restart/reads with zero
source requests.

Metadata-only native attributes must allow clean Free. The test inspects eviction
through the API and private state before any mounted lookup can reacquire the repo,
then verifies reacquired bytes, attributes, pin policy and original-source
preservation. The fixture's bounded Git HTTP children are reaped; filesystem
shutdown still follows the ordinary owned-detachment checks below. This case does
not cover dirty/staged Free refusal, Refresh or abrupt failure recovery.

## Cleanup and retained fixtures

The harness closes its own fixture handles, requests normal `/v1/prepare-quit`, and sends `SIGTERM` to its private helper only after a successful detach. It never forces an unmount. The helper uses its own process group to survive test-runner cleanup. If detach or ownership verification is uncertain, the harness fails clearly and preserves its `/tmp/rr-fskit-*` or `/tmp/rr-cold-*` directory and helper process. The failure output identifies the private paths and PID. The overall Go test timeout can bypass deferred cleanup, so inspect the retained session after a timeout as well. Do not delete that directory or terminate the helper while its volume might still be attached.

After closing processes or handles that hold that specific fixture busy, use the private socket path printed by the test to retry the normal operation:

```sh
artifact-fs desktop request \
  --socket /absolute/path/printed/by/the/test \
  --method POST \
  --path /v1/prepare-quit
```

Only after that command succeeds and the fixture volume is confirmed detached should you stop the printed helper PID with `kill -TERM PID` and remove its private fixture. An error or unreachable control socket does not prove a detach. Preserve the fixture for investigation if the normal operation cannot complete; do not use a forced unmount to turn the test into a pass.

## Record evidence

Save the full test output together with the source revision and working-tree state, macOS and Xcode versions, architecture, installed app/module build, and signing/profile facts. Record whether the test completed a real mount and whether cleanup succeeded. Keep historical FUSE release records unchanged; they are evidence for their own backend and release.

Include the inspector's source/build provenance and its successful bounded JSON
selection report. GUI enablement and public discovery are separate evidence;
neither substitutes for the subsequent mounted filesystem checks.
