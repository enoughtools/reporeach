# Local signing of an FSKit validation app

When the Mac has macOS 26 or later and an older Xcode, CI can compile the complete
app with the macOS 26 SDK. The local signing utility signs that compiled product
using the existing Developer ID identity in the Mac's Keychain. It does not
compile the app or export a private key. This profile-authorized path remains
separate from any explicitly approved ad-hoc local experiment.

First dispatch the optional validation artifact workflow described in
[the build guide](../../scripts/README.md). Select its successful run, verify the
repository and exact commit, and download `fskit-local-validation-arm64` or
`fskit-local-validation-x86_64` through the authenticated GitHub CLI. Retain the
original ZIP, JSON metadata and `SHA256SUMS`. The archive checksum must come from
that selected, trusted workflow download. Copying a checksum out of arbitrary
untrusted metadata does not authenticate an archive.

The signing utility requires a clean checkout of this repository, the CI commit
available locally, its source content fingerprint, and the actual matching
Developer ID provisioning profile for `com.enoughtools.reporeach.fskit`. The
profile must authorize the FSKit Module capability, the expected developer team,
and the selected local Developer ID Application certificate. The existing
production validator authenticates the Apple CMS signer and checks profile
scope, validity, certificate and claims; decoding its plist alone is insufficient.

Run from the repository root after recording the independently verified values:

```sh
python3 scripts/sign-fskit-validation.py \
  --archive build/fskit-validation/download/RepoReach-local-validation-arm64.zip \
  --metadata build/fskit-validation/download/RepoReach-local-validation-arm64.json \
  --archive-sha256 "$EXPECTED_ARCHIVE_SHA256" \
  --source-revision "$EXPECTED_CI_REVISION" \
  --source-sha256 "$EXPECTED_SOURCE_SHA256" \
  --workflow-run "$EXPECTED_WORKFLOW_RUN" \
  --arch arm64 \
  --identity "$DEVELOPER_ID_CERTIFICATE_SHA1" \
  --team "$DEVELOPER_TEAM_ID" \
  --profile "$FSKIT_DEVELOPER_ID_PROFILE"
```

The identity argument is the full uppercase certificate SHA-1 fingerprint from
the local Keychain. It selects the existing identity; it is not a private key.
The Mac's installed compiler builds only the small existing profile verifier,
which uses public Security APIs available before macOS 26. An authorized profile
and the local identity are mandatory for this signing path. There is no unsigned
or ad-hoc fallback.

Before signing, the utility checks the successful workflow's source revision and
active architecture artifact, the independent archive checksum, the exported
test-purpose marker and source fingerprint against the committed Git tree. The
archive check does not attest that a binary was compiled from that tree; trust
in the selected GitHub workflow and its authenticated download remains necessary.
Extraction rejects traversal, symlinks, special files, duplicate names, macOS
case or Unicode collisions, privileged modes and excessive expansion. Inert
AppleDouble metadata is omitted. The app, both extensions and helpers must have
the selected architecture and their expected identities.

The utility resolves the runtime app-group identifier and entitlement templates from the verified signing team. The app, `artifact-fs` and FSKit module claim the same macOS Team-ID-prefixed `<TeamID>.rr` group; `gh` and Finder do not need that connection. A Team-ID-prefixed macOS group needs no separate group profile or portal registration. The FSKit module still requires its own profile to authorize the restricted filesystem capability.

The utility signs `artifact-fs`, `gh`, the profile-bound FSKit module, the Finder
extension and the app in that order. Every component must pass strict signature,
certificate, team, hardened-runtime and timestamp checks. The FSKit module then
passes the existing signed-bundle/profile validator again. Failed checks leave
no retained app, and an existing output directory cannot be overwritten.

Successful output is isolated under
`build/fskit-validation/signed/<architecture>-<revision>-<archive-hash>/`. Its
`local-signing.json` records the input artifact, CI source, signing-utility source,
workflow, public identity and team, embedded-profile hash and every signed app
file's SHA-256. The CI app revision and signing-utility revision can differ; both
remain explicit so mounted-test evidence can identify the actual app and test
checkout separately.

This utility does not install or register the app, enable extensions, change
System Settings, mount a filesystem, notarize, produce release archives, change
the Applications link or publish downloads. Its metadata continues to say
`distribution: false`, `extensionActivationAuthorized: false` and
`mountedValidationPassed: false`. Complete normal extension enablement and
[the real mounted acceptance tests](fskit-acceptance.md) as separate steps before
preparing a release.

Focused regressions use inert archive, certificate and profile fixtures:

```sh
python3 scripts/test-local-fskit-signing.py
python3 scripts/test-fskit-packaging.py
```
