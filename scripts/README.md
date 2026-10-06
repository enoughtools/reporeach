# RepoReach builds and releases

The default build includes RepoReach's FSKit ExtensionKit module and requires
Xcode 26 plus the macOS 26 SDK. The management app retains a macOS 13 deployment
target; virtual mounts through the bundled module require macOS 26. No external
macFUSE installation is required by the FSKit backend. The existing published
beta 3 downloads still use the earlier backend and are immutable.

The current FSKit work is source validation. GitHub's `macos-26` runner includes
Xcode 26.6 at `/Applications/Xcode_26.6.app`; CI selects that toolchain explicitly
and compiles both architecture slices. Runner inventory:
https://github.com/actions/runner-images/blob/main/images/macos/macos-26-arm64-Readme.md
Compile checks and unit tests do not establish extension activation or a working
mount. Automatic release publication is disabled until a signed, enabled module
passes mounted filesystem tests on macOS 26.

Builds also require a Go launcher, Python 3, and network access for pinned
XcodeGen 2.46.0. Distribution packaging bundles verified official GitHub CLI
2.102.0. The engine compiler is pinned to Go 1.26.8 and fetched through Go's
toolchain mechanism without replacing the host's Go installation.

From the repository root on a compatible Mac, validate compilation without
creating release archives or changing any website download metadata:

```sh
python3 scripts/test-fskit-packaging.py
scripts/build-macos.sh --backend fskit --compile-only --arch arm64 --unsigned
scripts/build-macos.sh --backend fskit --compile-only --arch x86_64 --unsigned
```

Xcode can register a compiled app even when signing is disabled. After a
successful build, the script checks public app discovery and the exact FSKit
module registration. It retires only that build product when registered and
requires fresh proof of absence before exporting it. An already absent product
needs no registration change. Build products stay in their separate output
directory; installing and registering a validation app remain explicit steps.

For legacy management/source checks with Xcode 16, use the explicit
`--backend macfuse --compile-only` option. This generates the legacy project
specification, excludes the FSKit target, and never creates release archives.
The legacy and FSKit derived builds use separate private output directories.
Legacy distribution packaging is disabled because the current Darwin engine
uses the bundled FSKit module.

To obtain a complete compiled app for **local validation** when the local Mac
has macOS 26 or later but an older Xcode, dispatch the RepoReach workflow from
the selected committed branch with `validation_artifacts=true`:

```sh
gh workflow run reporeach.yml --repo enoughtools/reporeach --ref codex/native-fskit -f validation_artifacts=true
gh run list --repo enoughtools/reporeach --workflow reporeach.yml --branch codex/native-fskit --event workflow_dispatch
# Use the selected run ID after the macOS 26 job succeeds.
gh run download RUN_ID --repo enoughtools/reporeach --name fskit-local-validation-arm64 --dir build/fskit-validation/download
```

The optional artifacts contain the compiled app, bundled FSKit module, real Go
engine, verified GitHub CLI and dependency notices. The `--validation-artifact`
build option writes them under `build/fskit-validation/products/<architecture>` (or a generation-specific absolute `--validation-root` directory)
with source identity, toolchain versions, SHA-256 checksums and an explicit
local-validation marker. They require a clean committed source checkout.
They are unprovisioned and unsigned for distribution. Local signing may require
repeated user approval; production distribution requires the actual matching
FSKit profile and Developer ID signing. Registration or signing does not prove
that the filesystem mounts successfully.
Keep signing credentials on the local Mac. Use isolated test data, preserve the
installed app, and complete real mounted tests before preparing a release.
This mode cannot sign, notarize, stage website downloads or publish releases.
For local Developer ID signing with an authorized profile and independently
verified CI inputs, follow the [local FSKit signing guide](../docs/reporeach/local-fskit-signing.md).

After real backend validation and source freeze, distribution packaging requires
an existing Developer ID identity and the actual extension-specific Developer
ID provisioning profile in `REPOREACH_FSKIT_PROFILE`. Choose a new release
version in `VERSION`; existing archive filenames cannot be overwritten:

```sh
: "${VERSION:?Choose a new frozen release version}"
: "${REPOREACH_SIGN_IDENTITY:?Developer ID identity required}"
: "${REPOREACH_FSKIT_PROFILE:?Matching FSKit provisioning profile path required}"
scripts/build-macos.sh --backend fskit --arch arm64 --version "$VERSION"
scripts/build-macos.sh --backend fskit --arch x86_64 --version "$VERSION"
python3 scripts/release-manifest.py stage \
  --directory "dist/releases/$VERSION" --version "$VERSION"
```

RepoReach's production guard requires the profile embedded in the FSKit module's
own `Contents/embedded.provisionprofile` before signing. It authenticates the CMS
signature using public macOS Security APIs, explicit Apple roots from the system
keychain, the profile-authority and WWDR certificate markers, certificate
validity and OCSP policy. Decoding an unsigned CMS payload is insufficient.
Validation rejects expired, development, wildcard, host-app,
Finder, wrong-team, wrong-certificate, and missing-FSKit-capability profiles.
The final signed claims must match the profile's module App ID and team, retain
sandbox and hardened runtime, and disable debug task access. Production FSKit
archives cannot use ad-hoc signing. `codesign --verify` is a packaging check;
system policy assessment, extension enablement and an actual mount remain
required for release validation. Apple documentation:
https://developer.apple.com/documentation/technotes/tn3125-inside-code-signing-provisioning-profiles
https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac

The two architecture builds contain the native app, Finder extension, FSKit module, ArtifactFS
engine, and verified official GitHub CLI. Authorized distribution builds each produce a ZIP and DMG. The
staging command checks their SHA-256 hashes and Cloudflare's 25 MiB asset limit,
then copies them into `site/public/releases/<version>` and writes `latest.json`.
The manifest records the backend, app minimum OS and separate virtual-mount
minimum OS, and rejects mixing backends across architectures.
Commit and freeze source before the final builds. Manifest generation refuses
artifacts built from different source contents. Rebuild both architectures when
source changes. `source.dirty` and the source content hash remain visible in
release metadata, including for development builds.
The manifest also records each architecture's Go, Xcode, and XcodeGen versions.
Signed archives include Apple signing timestamps, so repeated builds reproduce
the packaging process rather than identical archive bytes.
Beta 3 defaults to app and Finder extension build number `3`. Set
`REPOREACH_BUILD_NUMBER` when packaging a later release.
ArtifactFS and GitHub CLI dependency notices are bundled. The GitHub CLI
collector reads the official binary's Go build metadata, downloads those exact
module versions, verifies their source hashes against both the binary metadata
and Go's checksum database, and retains license and notice files. It also
includes the corresponding Go standard library notices. Notice copies use
inert text filenames and retain their original source paths in the manifest.
This audits Go module notices; operating-system libraries are supplied by macOS
and are not bundled.

For Developer ID signing, pass `--sign-identity` with an identity already in the
Keychain, or set `REPOREACH_SIGN_IDENTITY`. Nested helpers and the Finder extension
are signed individually before the outer app. The Finder extension uses its
explicit entitlement file. Unsigned validation uses `--compile-only`; distributable
FSKit builds require the authorized Developer ID profile and identity described
above.

Add `--notarize` with either `REPOREACH_NOTARY_PROFILE`, or the three environment
variables `APPLE_API_KEY_PATH`, `APPLE_API_KEY_ID`, and `APPLE_API_ISSUER`. The API
key file must already exist locally. Never commit signing keys or credentials.
Signing without notarization is supported and represented accurately by
`signature: developer-id` and `notarized: false`.

The RepoReach workflow validates Go, native tests, the Swift client's control
contract against the real Go engine with synthetic authentication, website
formatting and builds, Linux FUSE behavior, and bundled FSKit source compilation.
Both Mac ARM jobs also exercise the real Go filesystem bridge with the Swift
extension client, including authenticated sessions, paged large responses,
binary reads/writes and handle cleanup. The standalone invocation is documented
in [the bridge smoke guide](../native/Tools/README.fsbridge-smoke.md).
The SDK 26 architecture jobs also run `scripts/test-fskit-volume-reads.sh`, a
hostless suite for production read callbacks, descriptor ownership and
cancellation. It creates no app, registration or mount.
Pull requests compile without signing keys, profiles or release archives. The
FSKit job uploads compilation logs; a manually requested run can also export
the unprovisioned local-validation artifacts described above. GitHub release publication is disabled
while the new backend is being validated. No source-validation command updates
the live downloads; publication remains a separate release step.

Run mounted filesystem tests without changing the Mac's driver or security:

```sh
scripts/fuse-e2e.sh
REPOREACH_FUSE_TEST_PATTERN='^TestE2ECatalogue$' \
  REPOREACH_FUSE_TEST_TIMEOUT=3m scripts/fuse-e2e.sh
```

On Linux, this uses an existing `/dev/fuse` device. On macOS, it uses a disposable
Linux container with only the FUSE device and mount capability enabled. It copies
Go source into the container; the host repository stays read-only.
