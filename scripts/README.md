# RepoReach builds and releases

Builds require macOS, Xcode, a Go launcher, Python 3, and network access to
the pinned official GitHub CLI and XcodeGen distributions. XcodeGen 2.46.0 is
downloaded into a verified private build cache. A macFUSE driver is not needed to
compile or package the app. Runtime mounts require a separate macFUSE install.
The engine compiler is pinned to Go 1.26.8 and fetched through Go's toolchain
mechanism without replacing the host's Go installation.

From the repository root:

```sh
scripts/build-macos.sh --arch arm64 --version 0.1.0-beta.2 --unsigned
scripts/build-macos.sh --arch x86_64 --version 0.1.0-beta.2 --unsigned
python3 scripts/release-manifest.py stage \
  --directory dist/releases/0.1.0-beta.2 --version 0.1.0-beta.2
npm ci --prefix site
npm run build --prefix site
npx --yes wrangler@4.147.0 deploy --config wrangler.jsonc
```

The two architecture builds contain the native app, Finder extension, ArtifactFS
engine, and verified official GitHub CLI. Each produces a ZIP and DMG. The
staging command checks their SHA-256 hashes and Cloudflare's 25 MiB asset limit,
then copies them into `site/public/releases/<version>` and writes `latest.json`.
Commit and freeze source before the final builds. Manifest generation refuses
artifacts built from different source contents. Rebuild both architectures when
source changes. `source.dirty` and the source content hash remain visible in
release metadata, including for development builds.
The manifest also records each architecture's Go, Xcode, and XcodeGen versions.
Signed archives include Apple signing timestamps, so repeated builds reproduce
the packaging process rather than identical archive bytes.
Beta 2 defaults to app and Finder extension build number `2`. Set
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
explicit entitlement file. Unsigned builds use ad-hoc signatures, as required by
Apple Silicon, and are identified as such in the manifest.

Add `--notarize` with either `REPOREACH_NOTARY_PROFILE`, or the three environment
variables `APPLE_API_KEY_PATH`, `APPLE_API_KEY_ID`, and `APPLE_API_ISSUER`. The API
key file must already exist locally. Never commit signing keys or credentials.
Signing without notarization is supported and represented accurately by
`signature: developer-id` and `notarized: false`.

The RepoReach workflow validates Go, native tests, the Swift client's control
contract against the real Go engine with synthetic authentication, website
formatting and builds, and Linux FUSE behavior. Pull requests produce unsigned
packages. Repository signing secrets are optional. A `reporeach-v<version>` tag,
or a manual run with `publish` enabled,
publishes architecture packages to GitHub after checks pass. Website publication
uses the staging and deploy steps above and remains a separate release step.

Run mounted filesystem tests without changing the Mac's driver or security:

```sh
scripts/fuse-e2e.sh
REPOREACH_FUSE_TEST_PATTERN='^TestE2ECatalogue$' \
  REPOREACH_FUSE_TEST_TIMEOUT=3m scripts/fuse-e2e.sh
```

On Linux, this uses an existing `/dev/fuse` device. On macOS, it uses a disposable
Linux container with only the FUSE device and mount capability enabled. It copies
Go source into the container; the host repository stays read-only.
