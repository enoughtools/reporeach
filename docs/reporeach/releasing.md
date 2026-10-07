# Building and releasing EnoughRepos

The current native backend bundles EnoughRepos's FSKit module and requires **macOS 26 or later for virtual repositories**. The management app retains a macOS 13 deployment target; that does not lower the filesystem requirement. A complete package contains the app, Finder extension, FSKit module, ArtifactFS engine, official GitHub CLI, and license notices. Git remains a system dependency. Native releases require normal approval of the bundled File System Extension, with no separate macFUSE installation.

The published `0.1.0-beta.3` is a historical macOS 13+ release requiring separately installed macFUSE's kernel backend. Preserve its original assets, manifest, source tag, and requirements. Current source supports legacy compilation only; it cannot recreate that release through the native packaging path. See [platform setup](platform-setup.md) and the download's actual manifest.

Use Go 1.26.8, **full Xcode 26 or later with the macOS 26 SDK or later**, Python 3, and standard macOS packaging tools. The scripts select Go 1.26.8 and download checksum-verified XcodeGen 2.46.0. Native development also supports an existing XcodeGen installation. The website requires Node.js 22.14 or later. Start with [CONTRIBUTING.md](../../CONTRIBUTING.md).

## Rebrand and upgrade compatibility

Current products are named `EnoughRepos.app` and `EnoughRepos-*`. The Xcode
project, schemes, Swift module name, signing and app-group identifiers, FSKit
short name `reporeach`, developer environment variables, and existing state under
`~/Library/Application Support/RepoReach` retain their compatibility names.
Historical releases and build evidence keep their actual RepoReach names.

Existing app-owned Git configuration can contain absolute paths to the bundled
GitHub CLI beneath `RepoReach.app`. An upgrade must preserve those legacy helper
paths through a compatible app alias, or explicitly migrate verified app-owned
configuration before removing the old path. The local managed `build/latest`
installation retains a relative `RepoReach.app` alias to `EnoughRepos.app` for
this purpose. This local link migration does not establish a general installer
or every historical installation layout; qualify those separately before release.

## Local validation products

Unsigned native products are for local validation, not public distribution. Compilation does not prove that macOS will authorize, enable, or mount the module:

```sh
scripts/build-macos.sh --backend fskit --arch arm64 --compile-only --unsigned
scripts/build-macos.sh --backend fskit --arch x86_64 --compile-only --unsigned
```

Compiled apps are in `build/native/fskit/<arch>/Build/Products/Release/EnoughRepos.app`; these commands create no release archives. To export a complete unsigned ARM64 validation app with helpers and notices, use a fresh generation:

```sh
validation_root="$PWD/build/fskit-validation/generations/local-arm64-$(date -u +%Y%m%dT%H%M%SZ)"
scripts/build-macos.sh --backend fskit --arch arm64 --validation-artifact \
  --validation-root "$validation_root" --unsigned
```

This exports `stage/arm64/EnoughRepos.app` and `products/arm64/EnoughRepos-local-validation-arm64.zip` beneath that directory, with local source evidence. It cannot sign, notarize, or publish. Activation needs appropriate authorized local signing and a matching profile; a device-bound development profile is never a distribution profile. Preserve validation generations until their evidence and runtime state have been reviewed.

## Complete Developer ID packages

Check out the agreed source revision, require a clean checkout, and record source, Xcode, SDK, macOS, Go, and XcodeGen versions. Choose a new immutable version and build number. The example proposes a new version; it is not evidence that this version has been released.

Use an existing Developer ID Application identity from the Keychain and the actual matching **Developer ID provisioning profile that authorizes FSKit**. Supply a certificate name or hash and a profile path, never a private key or password. The profile must authorize production module `com.enoughtools.reporeach.fskit`, match the signing team and certificate, and permit distribution to all devices. The validator authenticates these claims and embeds the profile in the module. Do not substitute the profile-acquisition Xcode archive: it lacks the engine, GitHub CLI, and complete notices.

```sh
release_version=0.1.0-beta.4
export REPOREACH_BUILD_NUMBER=4
export REPOREACH_SIGN_IDENTITY='Developer ID Application: Your Organization (TEAMID)'
export REPOREACH_FSKIT_PROFILE=/absolute/path/developer-id-fskit.provisionprofile
test -z "$(git status --porcelain)"
scripts/build-macos.sh --backend fskit --arch arm64 --version "$release_version"
scripts/build-macos.sh --backend fskit --arch x86_64 --version "$release_version"
python3 scripts/release-manifest.py manifest \
  --directory "dist/releases/$release_version" --version "$release_version"
```

Run architecture builds sequentially. Complete apps stage in `build/package/fskit/<arch>/EnoughRepos.app`; intermediates are in `build/native/fskit/<arch>`. Outputs are `dist/releases/<version>/EnoughRepos-<version>-macOS-<arch>.dmg` and `.zip`, per-build `.metadata-<arch>.json`, `release.json`, and `SHA256SUMS`.

The script signs the engine, GitHub CLI, Finder extension, FSKit module, and app with hardened runtime and timestamps, resolves authorized app-group claims, and verifies nested signatures and module authorization. Existing release filenames cannot be overwritten. Any source change after packaging requires new builds from the final clean revision. Confirm both architectures and formats, matching source revision/content hashes, profile hashes, and `source.dirty: false`; manifest generation does not independently require every architecture or a clean source tree.

The scripts use committed Go checksums, the website lockfile, pinned tool archives, and Go `-trimpath` with an empty build ID. Native build metadata, packaging dates, signing timestamps, and notarization can change output bytes. **Bit-for-bit reproducibility is not established.** Compare provenance and recorded hashes.

## Notarization

For notarized packages, add `--notarize` to both original architecture packaging commands above and set `REPOREACH_NOTARY_PROFILE` to a **known, existing** `notarytool` Keychain profile name. Do not guess the name. Alternatively, the script accepts `APPLE_API_KEY_PATH`, `APPLE_API_KEY_ID`, and `APPLE_API_ISSUER`; keep the private key outside source control and logs.

```sh
: "${REPOREACH_NOTARY_PROFILE:?Existing configured notary profile required}"
scripts/build-macos.sh --backend fskit --arch arm64 --version "$release_version" --notarize
scripts/build-macos.sh --backend fskit --arch x86_64 --version "$release_version" --notarize
```

These are alternatives to packaging without `--notarize`, not a way to overwrite completed archives. The script submits the signed app, staples and validates its ticket, and assesses it with Gatekeeper. It then creates and signs the DMG, submits it separately, and staples, validates, and assesses that image. Both submissions must succeed before the completed release records `notarized: true`. Preserve accepted submission results with the release evidence.

A Developer ID signed package without `--notarize` records `notarized: false`. Keep that state accurate in the manifest, website, and release notes. Later notarization requires a new immutable release generation; do not rewrite completed archive metadata. Signatures and profile authorization alone do not establish normal extension activation or downloaded-app Gatekeeper acceptance.

## Bundled tools and notices

`scripts/vendor-gh.sh` downloads official GitHub CLI `v2.102.0` archives and verifies pinned SHA-256 values and the MIT license. Updating it requires both architecture hashes, the license hash, package path, manifest metadata, and notices to change together.

`scripts/bundle-go-licenses.py` includes notices for the engine's Go modules and standard library. `scripts/bundle-gh-licenses.py` verifies dependency sources against the checksum database and binary metadata, then collects GitHub CLI module, standard-library, and vendor notices. Both collectors refuse missing notices. Packages also include the Apache 2.0 license and attribution to upstream Cloudflare ArtifactFS. Review generated resources when dependencies change.

## Manifest, website, and publication

The manifest records source/toolchain provenance, architecture, size/hash, signing and notarization state, filesystem backend, app and mount minimum OS versions, and bundled-module profile hash. Generating it rechecks archive bytes and consistent backend/profile/source content. Checksums detect changed downloads; they do not independently authenticate the publisher. The website displays the published backend's requirements: historical beta.3 needs macOS 13+ and external macFUSE; native virtual repositories need macOS 26+, Git, and bundled extension approval.

Before staging or deploying, preserve the whole historical download path set. Restore exact retained assets or download the existing GitHub release into an empty versioned directory without overwriting files, then verify their previously recorded hashes and manifests. In particular, retain beta.3's four DMG/ZIP downloads, `release.json`, and `SHA256SUMS` beneath `site/public/releases/0.1.0-beta.3`. Never regenerate its manifest from current source. A new `latest.json` may point to the new release while old versioned URLs remain available.

After runtime and package qualification, stage verified downloads:

```sh
python3 scripts/release-manifest.py stage \
  --directory "dist/releases/$release_version" --version "$release_version"
npm ci --prefix site
npm run format:check --prefix site
npm run build --prefix site
python3 scripts/test-website-release.py
```

Staging writes versioned downloads and `site/public/releases/latest.json`; it does not deploy. Individual files larger than 25 MiB are refused by the static-asset guard. If an archive exceeds that limit, use suitable object storage and update the actual download/staging path rather than bypassing the guard.

`wrangler.jsonc` deploys the complete `site/dist` asset set to [reporeach.reb.run](https://reporeach.reb.run). Deploy only after confirming it contains new downloads and preserved historical paths. After deployment, verify the rendered version, requirements, signing states, architecture/format URLs and hashes, and historical URLs. Source and releases are at [enoughtools/reporeach](https://github.com/enoughtools/reporeach).

`.github/workflows/reporeach.yml` validates Go, race-sensitive packages, disposable Linux FUSE behavior, the website, and native SDK builds/tests for both architectures. **Its release job is disabled.** Tags and `publish` dispatches do not currently publish a release, and unsigned CI validation exports are not distribution packages. Manual GitHub publication requires qualified complete local packages, an explicit final source revision, a fresh immutable tag, and reviewed notes listing tested environments and remaining limitations. Re-enabling automation also requires signed package producers and release gates, not only changing the disabled condition.

## Release validation checklist

Follow [native acceptance](fskit-acceptance.md) and retain its evidence. An authenticated production profile is a signing prerequisite; a mounted pass on macOS 27 does not establish macOS 26 runtime support. SDK 26 compilation for ARM64 and Intel is separate from installed-app testing.

- [ ] Record final clean source/toolchain and review notices. Pass relevant Go build/vet/unit/race tests, native tests, and website checks.
- [ ] Validate complete packages for each offered architecture, helper/extension/app signatures, authorized production profile and app-group claims, hashes, and notarization when advertised.
- [ ] Install a downloaded, quarantined production app on supported Macs, including macOS 26. Check Gatekeeper, normal bundled FSKit enablement, exact production module selection, Git, optional GitHub authorization, and dependency errors.
- [ ] Validate installed Finder registration, badges, private cache access, single-repo actions, and absence of mixed-repo actions.
- [ ] Browse virtual names without acquisition; exercise lazy text/binary/symlink/mode reads, edits, staging, commit, branch switch, kernel attributes, and restart persistence.
- [ ] Without GitHub sign-in, adopt disposable supported remote/bare/checkout sources using native Git credentials. Verify committed-ref selection and unchanged original index, dirty files, and untracked files.
- [ ] Toggle repository/owner visibility and restart; verify exclusions, data retention, normal catalogue reconnect, busy refusal, and cancellation without force detach.
- [ ] Perform cold Keep Downloaded, disconnect the source, restart normally, and read the committed tree. Exercise cancellation/recovery; do not promise full history, LFS, or submodules.
- [ ] Complete clean Free Up Space and lazy reacquisition, retaining local filesystem attributes. Verify refusal for dirty/staged/unpushed/recovered state, HEAD/baseline divergence, unsupported metadata, and busy mounts, with retained data after each refusal.
- [ ] Verify fetch-only Refresh preserves working files, branch, index, and local attributes; test failed-source retry and explicit read-only operation.
- [ ] Exercise normal quit/relaunch, supported folder relocation, optional launch at login, and reviewed interruption/recovery paths without global registry resets or forced detach.
- [ ] Confirm accurate privacy/auth/download copy, private vulnerability reporting, beta upgrade/migration behavior, and preserved historical assets. Publish actual tested environments and every unperformed qualification step.
