# Mounted FSKit acceptance harness

The app is now named EnoughRepos. Historical results below retain the RepoReach
name and exact build identities used for those runs. The filesystem short name
`reporeach`, module identifiers, and private state paths remain compatible. A
rebrand does not transfer these historical passes to a newly compiled app.

`TestFSKitMountedAcceptance` exercises the native macOS 26 filesystem through real mounted paths and the desktop control service. It uses private temporary state, an ordinary chosen catalogue folder, a separate hidden native volume under `state/native-catalogue/volume`, and local Git fixtures. It does not install an app, activate an extension, update the OS, or change security settings.

The current harness tests the hybrid catalogue contract: organization folders are ordinary host directories, virtual repository entries are owned links to the private volume, adopted repositories retain their original checkout, and Keep publishes an ordinary checkout with its own `.git` directory. After normal quit, adopted originals and kept checkouts remain usable without the app. Cached kernel inspection captures the private volume's identity; the chosen catalogue root must never itself be mounted. These assertions now have real mounted passes on the development Mac, recorded below. Historical whole-root passes do not establish this hybrid contract.

This is a narrow acceptance harness, not complete native release qualification. A skipped test, a successful Go compilation, and bridge tests are not mounted FSKit evidence.

## Local16 Finder dispatch acceptance

Installed signed local build 16 uses clean executable source
`aa8021b219532b91b73f5da6536ece1808948369`. Signed packaging, 67 native UI tests,
and production Finder typechecking pass. On macOS 27.0.1 ARM64, actual Finder
Refresh launched the stopped app/service and completed in 1.363 seconds. Runtime
code identity matched installed local16; HEAD and clean Git status were unchanged,
and the checkout remained pinned/materialized with its kept-downloaded badge.
This verifies shared dispatch through Refresh. Keep remains the local15 app-button
result; no extra Finder Keep/Free cycle was run. Unchanged Go/FSKit retains the
local15 passes below without a local16 rerun. Evidence:
`build/fskit-evidence/local16/finder-dispatch-success.json`, `finder-runtime.json`,
and `finder-actions.jsonl` in that directory. Existing release limits remain; no
release was made.

## Historical local15 hybrid acceptance

Installed signed local build 15, with clean executable source
`de3610c581d32bfbd89039bfdd0c5d3e1c4701c8`, passes all three actual mounted
fixtures on macOS 27.0.1 ARM64: `TestFSKitMountedColdKeepAcceptance` in
12.26 seconds, `TestFSKitMountedAcceptance` in 7.96 seconds, and
`TestFSKitMountedColdStorageAcceptance` in 7.67 seconds. Ordered CLI build,
vet, and full Go tests also pass in 8.547, 2.305, and 159.504 seconds respectively.
Evidence: `build/fskit-evidence/local15/installed-mounted-tests.log` and
`build/fskit-evidence/local15/go-checks.json`. These are disposable fixture results;
the separate live conversion below does not substitute for them.

The expanded dormant Keep starts with cached native file/directory attributes,
no writable activation, and unvisited nested contents. It spans 160 descendant
directories with 207 regular files and one symlink: 208 unique blobs totaling
4,341 bytes. Accepted
operation and blocked-source progress status each round to 0 ms. Keep preserves
the tracked index and source checkout, removes active engine/preview sources,
and publishes an ordinary checkout. After the helper stops normally, the checkout
remains readable with its source offline and zero requests. Native Git works
without a GitHub account or GitHub CLI invocation.

This enlarges the regression beyond the native 128-directory-session budget.
The prior implementation retained completed EOF cookie sessions until admission
failed on scan 129, exposing repeated-inventory failure in a live Keep attempt.
The corrected module retires the oldest completed session under pressure while
retaining partial pages, references, and uncertain acknowledgement cleanup.
Separate native regressions cover those ownership and recovery rules. Export
reads also set close-on-exec atomically before concurrent Git subprocesses.

Primary acceptance retains dirty/staged/untracked data, Git state, native metadata,
ordinary placement, symlink operations, and app-off access. Cold storage retains
selected-blob read-only acquisition without writable preparation, five-blob/
107-byte Keep, zero-request offline reads, local-metadata Free refusal, clean
reclamation, and reacquisition. Its initial preview takes 227 ms, cached listing
2 ms, and prepared names-only listing 9 ms with no source requests. The unknown-size
native open takes 487 ms and five requests, acquiring only its selected 20-byte
blob; first read takes 2 ms with zero requests while the other four blobs remain
absent and the writable engine remains unprepared.

An authorized Keep through the management app on `enoughtools/reporeach` also
completes, with an observed accepted-to-complete interval of 10.113 seconds.
All 188 blobs (2,491,698 bytes) were already cached. The selected path becomes an
ordinary checkout with an ordinary `.git` directory and clean native Git status;
Finder identifies it as a folder kept downloaded, and the app shows a local
checkout. After normal GUI quit, the app and engine are absent. Git status remains
clean, and HEAD and the README blob remain readable with `GIT_NO_LAZY_FETCH=1`.
Evidence: `build/fskit-evidence/local15/user-keep-success.json`. This qualifies
conversion through the app and the recorded app-off Git checks; its already
cached contents do not measure network hydration.

Finder callbacks now read current official
selected/targeted URLs, revalidate eligibility, and explicitly dispatch to the
containing app. The live local15 Keep callback reached its handler but rejected
the containing-app identity read under the extension sandbox. Local16 replaces
that read with exact OS-registered-path validation and the compiled application
identifier; its live Refresh proof is recorded above. Prior menu visibility and
management-app conversion alone did not establish that execution.

Normal fixture shutdown is part of the passing sequences. Independent complete
cached global mount inspections after the fixtures and after normal GUI quit
both report no RepoReach volumes. The latter also confirms app and engine absence
alongside the app-off checkout checks above. These
passes do not qualify real-network cold Finder latency, macOS 26, Intel, remaining
failure/recovery gates, or release readiness. No release was made.

## Historical local12 hybrid acceptance

Historical hybrid result (2026-10-06): clean source `7d17d837b5d2bfa9186abc855969cdc7e5f06e02`, with the signed and installed local build 12 app/module (`CFBundleShortVersionString=0.1.0`, `CFBundleVersion=12`), passes the combined mounted sequence on macOS 27.0.1 ARM64. Primary acceptance completes in 7.78 seconds, including creation, native permission/readlink/content checks and removal of a new writable symlink. Cold storage acceptance completes in 7.39 seconds. Evidence: `build/fskit-evidence/local12/installed-mounted-tests.log`.

The cold pass records a 199 ms initial preview phase, a 2 ms cached names-only listing and a 5 ms prepared directory listing. Ordinary organization browsing creates no writable checkout or source requests. Native traversal of the nested preview directory succeeds while all five committed blobs remain missing. The native unknown-size file open takes 431 ms and makes five source requests to acquire only its selected 20-byte blob; the first read takes 2 ms and makes zero additional source requests. The repository remains virtual with no writable engine, four unselected blobs remain absent from preview content Git, metadata-preview Git remains entirely blobless, and the canonical shared cache contains exactly one blob. Git pointer activation then leaves all five main-engine Git blobs missing; directory enumeration generates no additional source requests and adds no cached blobs.

Keep materializes five unique blobs (107 bytes). Ordinary checkout reads with the app stopped and after an offline restart generate zero source requests. Extended-metadata Free refusal, clean Free, removal of active preview and owned recovery storage, restored virtual policy and reacquisition all pass, followed by normal helper shutdown and cleanup. The duplicate nested file does not increase unique-blob or byte accounting.

An actual Finder check on local build 12 traverses the ordinary root, organization, repository and nested directories with normal blue folder presentation. Cached metadata for an existing user repository exposes 49 root entries while the repository stays virtual, its writable engine remains absent and the global engine registration count remains unchanged at 114. Finder thumbnails acquire four selected image blobs totaling 18,741 bytes. Keep, Free and Refresh context-menu actions are visible on public catalogue paths and resolved private-volume paths; no user repository was converted during this check.

These loopback fixture timings and cached Finder checks do not qualify real cold GitHub Finder network performance. macOS 26 and Intel runtime remain unqualified. This is local development validation; no release was made.

Historical hybrid result: the primary sequence passed in 6.94 seconds and cold storage in 6.83 seconds using installed local build 8 with uncommitted Go helpers based on `aedad10` and incremental fixes between runs. Evidence: `build/fskit-evidence/local8/mounted-symlink-mode.log` and `build/fskit-evidence/local9/mounted-cold-verified.log` (the latter directory records a development cycle; its module was still build 8). The local build 11 run additionally exposed the native unknown-size-open acquisition now covered by the passing build 12 regression; its earlier overly strict assertion failure is recorded in `build/fskit-evidence/local11/installed-mounted-tests.log`.

Historical whole-root record: The complete primary sequence passes at `8807963` with the signed validation module on macOS 27.0.1 ARM64, including native binary/empty extended attributes and persistence across visibility changes, remount and restart. Fresh cached kernel inspection confirms final detachment and both helper exits. This establishes that runtime, not a macOS 26 mounted pass. See [the native backend's remaining release gates](native-fskit.md).

Historical combined whole-root result: clean helper `f47c1fd` with unchanged installed beta.5 native app `6c77f89` passes primary (6.04 seconds) and cold storage (3.38 seconds) on macOS 27.0.1 ARM64. Independent complete cached inspection confirms all seven disposable mount identities and four helpers absent; the separate GUI catalogue remains live. This historical result does not qualify subsequent beta.6 directory-performance changes, the current hybrid contract, macOS 26, or Intel runtime.

## Prerequisites

- macOS 26 or later, with the `reporeach` FSKit module already installed and enabled in normal System Settings.
- An installed `EnoughRepos.app` containing `Contents/Extensions/RepoReachFSKit.appex`, whose compiled filesystem implementation matches the verified native source under test. Record separate app and helper revisions and any validation-only bundle-metadata changes as described below. Building the extension requires Xcode 26 or later and a macOS SDK at version 26 or later; Xcode 27 with its macOS 27 SDK is a valid local toolchain while retaining the module's macOS 26 deployment target. Installation also requires the appropriate signing and FSKit entitlement authorization.
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
AFS_FSKIT_APP=/Applications/EnoughRepos.app \
AFS_FSKIT_MODULE_CONFIRMED=1 \
AFS_FSKIT_SIGNING_IDENTITY=VERIFIED_LOCAL_CERTIFICATE_SHA1 \
go test -run '^TestFSKitMountedAcceptance$' -count=1 -v -timeout=20m .
```

The test skips unless explicitly opted in, and skips on an older macOS version. On a supported, opted-in host, missing prerequisites or a failed mount are failures, not passing evidence. It launches its own Go desktop helper against private state and uses the installed native extension for the mount.

To run all three current hybrid fixtures under the same prerequisites, replace
the `-run` value with
`^TestFSKitMounted(Acceptance|ColdKeepAcceptance|ColdStorageAcceptance)$`.

For the approved isolated experiment, add the exact validation module selection
to the same invocation and use its absolute app path:

```sh
GOTOOLCHAIN=go1.26.8 \
AFS_RUN_FSKIT_E2E_TESTS=1 \
AFS_FSKIT_APP=/absolute/path/to/isolated/EnoughRepos.app \
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

- Adoption of an existing local checkout without GitHub sign-in or a `gh` invocation; its original staged, unstaged and untracked work remains in place, and its catalogue entry points to that same checkout.
- A separate bare-source virtual repository exercising lazy reads of text and binary files, symlink targets, and executable files.
- Creation of a writable native symlink with readable permissions, correct readlink target and readable target content, followed by removal before later Git and Keep assertions.
- Native Git status, staging, commits, and branch checkout with warmed file lookups.
- Native binary/empty extended attributes, create/replace/default policies, missing/remove errors, lists and short buffers; lazy catalogue metadata and symlink/gitfile isolation.
- Retained open-file reads and writes across rename and unlink.
- A busy virtual visibility change refusing to publish, preserving the catalogue and mounted view; successful repository and organization hide/show changes using normal reconnects. Empty ordinary organization directories are retained, and hiding adopted entries never changes their original checkout.
- Normal unmount/remount and desktop helper restart using the same FSKit resource directory, with fresh lookups of retained virtual working-tree content and metadata.
- Keep materializing a dirty virtual checkout with identical HEAD, index, staged/unstaged/untracked binary files and native file metadata; the ordinary checkout remains readable and Git reports the same status after the private daemon stops.

Each release candidate needs its own complete package, signing/notarization and architecture checks. Remaining mounted gates include explicit read-only mutation refusal, Refresh, dirty/staged Free refusal, unsupported-file behavior, failure injection and interrupted-session recovery. macOS 26 and Intel runtime remain unqualified. Passing the primary sequence does not establish those gates.

## Cold downloads and clean eviction

Historical whole-root cold-storage record: this sequence passed on macOS 27.0.1 ARM64 with the clean `a708aba` Go test helper and preserved `8807963` signed native validation app. Independent comparison confirms all native source inputs and compiled module bytes before the signature are identical between those revisions. It downloads three initially missing unique blobs (39 bytes), restarts and serves them with zero remote requests, and verifies clean Free/reacquisition with native metadata retained. Cached kernel inspection confirms all three captured mount identities and both helper exits after normal cleanup.

Two attempts with the newly registered `a708aba` validation app failed during helper dispatch before mounting, despite public discovery reporting the exact enabled module. Narrow logs show registration/IPC error codes; the cause is unproven. Restoring the preserved app through normal exact-path registration allowed the sequence to pass. This result establishes storage behavior with that installed app, not activation of the newer app or the production distribution.

After recording the primary result and normal cleanup, run
`TestFSKitMountedColdStorageAcceptance` with the same explicit prerequisites and
environment, changing only the `-run` test selection. This is a separate disposable
fixture with a filtered loopback HTTP Git source and two commits of history. Ordinary root and organization listings must generate no source requests and no writable engine registration. Missing-child metadata probes acquire at most an immutable shallow preview; repository names include legitimately committed `.DS_Store` and `Icon\r` files, and preview Git contains one commit with five unique blobs still missing under `GIT_NO_LAZY_FETCH=1`. A nested directory must be readable and traversable through native lookup and enumeration without source requests. The synthesized `.git` pointer appears in this cold listing; lookup/stat returns its exact metadata without writable engine preparation. Initial preview acquisition and cached names-only listing are timed separately. This tests mounted metadata and enumeration behavior; it does not substitute for measuring cold Finder navigation.

A readonly native open of the nested file must leave the repository virtual with no writable engine registration. The native kernel may request an exact size during open; when the source metadata does not supply a size, that request may acquire the selected blob before the first read. Open and read together may request only the pinned commit and selected blob; the separate shallow preview source must contain that blob with the other four unique committed blobs absent. If open acquires the blob, the first read must reuse it without additional source requests. The canonical shared cache must contain exactly that one 20-byte blob, while metadata-preview Git remains entirely blobless. Open and first-read timing and source request counts are recorded separately. A cached reread with the source offline must generate no requests. This native unknown-size allowance does not alter the catalogue API's known-size readonly-open no-fetch contract.

Reading the `.git` pointer's contents must promote the same retained preview item to the authoritative writable checkout, with pointer bytes agreeing with its earlier exact size. This preparation must still leave all five main-engine Git blobs missing and retain exactly the selected blob in the shared canonical cache. Names-only directory enumeration must generate zero additional source requests or blob acquisitions. Keep must report exact unique-blob accounting, publish an ordinary checkout containing its own `.git` directory, and remove its active engine registration and preview Git source. Offline reads are verified both with the app stopped and after a normal restart, with zero source requests.

Extended file attributes are local data that Git cannot recover. A materialized Free with fixture attributes must fail while retaining the ordinary checkout and attribute bytes. The test removes only the attributes it created and then verifies clean Free, restored virtual link, durable policy, removed private engine storage, reacquired file bytes and original-source preservation. The fixture's bounded Git HTTP children are reaped; filesystem
shutdown still follows the ordinary owned-detachment checks below. This case does
not cover dirty/staged Free refusal, Refresh or abrupt failure recovery.

## Cleanup and retained fixtures

Source `f47c1fd` permits reuse of a backing folder containing only an OS-created `.fseventsd` when a private receipt proves prior empty-folder preparation followed by a successful native mount, with matching folder/volume identity and strict system-directory checks. This does not authorize arbitrary existing directories or delete their contents. Record normal remount and final cached mount absence independently.

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

For a disposable diagnostic run, `AFS_FSKIT_TRACE_GIT_COMMANDS=1` enables a private Git wrapper only in the fixture daemon's environment. The already copied helper records command/flag categories, fixture location classes and whether `GIT_NO_LAZY_FETCH=1` is present, then replaces itself with the original absolute Git binary. Argument values, URLs, credentials, HTTP headers and blob/pack output are excluded. On failure, bounded command categories and the loopback source's wanted-object metadata are included in the test output. This option adds process overhead; leave it unset when measuring cold-browsing latency.
