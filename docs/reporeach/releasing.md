# Building and releasing RepoReach

The beta packages the native RepoReach app, Finder extension, ArtifactFS engine, official GitHub CLI, and license notices. The macFUSE kernel backend and system Git executable remain separately installed dependencies. Read [platform setup](platform-setup.md) for driver, security approval, and Finder extension requirements. The build targets macOS 13 or later and produces separate Apple Silicon (`arm64`) and Intel (`x86_64`) archives.

Use Go 1.26.8, full Xcode 16 or later and its command-line tools, Python 3, and the standard macOS packaging tools. The packaging scripts select Go 1.26.8 and download checksum-verified XcodeGen 2.46.0. Native development also supports an existing XcodeGen installation. The website requires Node.js 22.14 or later. First run the checks in [CONTRIBUTING.md](../../CONTRIBUTING.md).

## Repeat a source build

Check out a specific source revision and record your Xcode, macOS, Go, and XcodeGen versions. Keep the checkout clean when creating a release. For development packages:

```sh
release_version=0.1.0-beta.1
scripts/build-macos.sh --arch arm64 --version "$release_version" --unsigned
scripts/build-macos.sh --arch x86_64 --version "$release_version" --unsigned
python3 scripts/release-manifest.py manifest \
  --directory "dist/releases/$release_version" --version "$release_version"
```

Outputs are `dist/releases/<version>/RepoReach-<version>-macOS-<arch>.dmg` and `.zip`, alongside `release.json` and `SHA256SUMS`. The generated app stages in `build/package/<arch>/RepoReach.app`; native intermediates are in `build/native/<arch>`.

`--unsigned` produces **ad-hoc signed** executables so they can run on Apple Silicon. It does not mean Developer ID signing or Apple notarization. The manifest records `signature: "ad-hoc"` and `notarized: false`. Do not advertise such an artifact as notarized or advise users to disable Gatekeeper globally.

The scripts make source builds repeatable by using committed Go dependency checksums and the website lockfile, fixed GitHub CLI archives with hard-coded SHA-256 verification, and Go `-trimpath` with an empty build ID. Native build metadata, packaging dates, signing timestamps, and notarization can change output bytes. **Bit-for-bit reproducibility is not established.** Compare the source/toolchain provenance and recorded artifact hashes rather than promising identical archives.

## Bundled tools and notices

`scripts/vendor-gh.sh` downloads the official GitHub CLI `v2.102.0` archive for the requested architecture and verifies its pinned SHA-256. It also verifies the pinned MIT license file. Changing the version requires updating both architectures' hashes, license hash, package path, manifest metadata, and distribution notices.

`scripts/bundle-go-licenses.py` copies notices for Go modules included in the engine and the Go standard library. `scripts/bundle-gh-licenses.py` reads the official GitHub CLI binary's module metadata, verifies dependency sources against the Go checksum database and embedded hashes, and collects module, standard-library, and vendor notices. Both collectors refuse missing notices. The app also includes ArtifactFS/RepoReach's Apache 2.0 license, GitHub CLI's MIT license, and attribution to upstream Cloudflare ArtifactFS. Review generated resources before distribution, especially when changing dependencies.

## Developer ID signing and notarization

Use a Developer ID Application identity already available in your Keychain. Pass its name or hash, not a private key or password:

```sh
release_version=0.1.0-beta.1
export REPOREACH_SIGN_IDENTITY='Developer ID Application: Your Organization (TEAMID)'
export REPOREACH_NOTARY_PROFILE='reporeach-notary'
export REPOREACH_BUILD_NUMBER=1
scripts/build-macos.sh --arch arm64 --version "$release_version" --notarize
scripts/build-macos.sh --arch x86_64 --version "$release_version" --notarize
python3 scripts/release-manifest.py manifest \
  --directory "dist/releases/$release_version" --version "$release_version"
```

The profile must already be configured for `xcrun notarytool`. Alternatively, the script accepts `APPLE_API_KEY_PATH`, `APPLE_API_KEY_ID`, and `APPLE_API_ISSUER`; keep the API private key outside source control. The packaging script signs helpers, the Finder extension, and the app in order, verifies their signatures, submits an archive, staples and validates the app's ticket, and assesses the app with Gatekeeper. It then creates and signs the DMG, submits it separately, and staples, validates, and assesses that image. Both notarization submissions must succeed when `--notarize` is selected.

Only a successful notarization path records `notarized: true`. A signing identity without `--notarize` produces a Developer ID signed but unnotarized artifact. Verify the actual release manifest and logs.

## Manifest, website, and CI

The manifest records source repository/revision, dirty-checkout status, artifact architecture, byte size, hash, actual signing/notarization flags, minimum macOS, and macFUSE requirement. Generating it rechecks that the archives still match their packaging metadata. Checksums detect changed downloads; they do not independently authenticate the publisher.

To stage verified downloads for the website:

```sh
release_version=0.1.0-beta.1
python3 scripts/release-manifest.py stage \
  --directory "dist/releases/$release_version" --version "$release_version"
npm ci --prefix site
npm run build --prefix site
```

Staging writes versioned downloads and `site/public/releases/latest.json`. It rejects individual files over the current Cloudflare static-asset limit of 25 MiB; use an appropriate object-storage distribution path if artifacts exceed it. Staging and building do not deploy the website. Product downloads point to [reporeach.reb.run](https://reporeach.reb.run); the source/release repository is [enoughtools/reporeach](https://github.com/enoughtools/reporeach).

`.github/workflows/reporeach.yml` checks the engine, race-sensitive packages, a disposable Linux FUSE environment, and the website, then packages both architectures on macOS. A `reporeach-v<version>` tag or an explicitly selected publish dispatch can create a GitHub release after those jobs pass. Signing is conditional on configured secrets; never assume that CI artifacts are notarized just because a release job ran. The workflow's generated notes do not replace a clear statement of beta limits and any unperformed macOS runtime checks.

## Release validation checklist

- [ ] Record the source revision and toolchain versions; build from a clean checkout. Review dependency changes and included notices.
- [ ] Pass engine build, vet, unit tests, and relevant race tests. Pass native unit tests and the locked website build.
- [ ] Pass mounted FUSE tests with local disposable repos. Separate Linux results from actual macOS runtime results.
- [ ] Validate both architecture archives, app/helper/extension signatures, hashes, manifest flags, and the notarization path when advertised.
- [ ] Install a downloaded, quarantined app on a clean supported Mac. Check launch, Git availability, macFUSE setup, real GitHub authorization, and meaningful dependency errors.
- [ ] Enable the installed Finder extension and verify badges, private cache access, single-repo action routing, and no action for a mixed-repo selection.
- [ ] Browse owner/repo names without acquisition; enter one repo and verify on-demand reads, binary content, edits, staging, commit, branch switch, and restart persistence.
- [ ] Complete Keep Downloaded, disconnect the network, and read the current committed tree. Check cancellation and interrupted download recovery; do not claim full history, LFS, or submodule availability.
- [ ] Exercise Free Up Space refusal for dirty/staged/unpushed/recovered state, unsupported metadata, and a busy mount. Verify retained data after failure and the surviving virtual entry after successful release.
- [ ] Check explicit refresh and discovery semantics, folder relocation, quit/relaunch, and optional launch at login without silently changing or publishing local work.
- [ ] Confirm private vulnerability reporting, accurate privacy/auth copy, and working product/download/source links. Publish release notes with tested environments and all remaining runtime limitations.
