#!/bin/bash
# Build and package one macOS architecture. Network dependencies are pinned.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export GOTOOLCHAIN=go1.26.8
ARCH=arm64
VERSION=0.1.0-beta.1
SIGN_IDENTITY="${REPOREACH_SIGN_IDENTITY:-}"
NOTARIZE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --unsigned) SIGN_IDENTITY=""; shift ;;
    --sign-identity) SIGN_IDENTITY="$2"; shift 2 ;;
    --notarize) NOTARIZE=true; shift ;;
    *) echo "Usage: $0 [--arch arm64|x86_64] [--version VERSION] [--unsigned|--sign-identity ID] [--notarize]" >&2; exit 2 ;;
  esac
done
case "$ARCH" in arm64) GO_ARCH=arm64 ;; x86_64) GO_ARCH=amd64 ;; *) echo "Unsupported architecture: $ARCH" >&2; exit 2 ;; esac
case "$VERSION" in *[!A-Za-z0-9.+-]*|"") echo "Invalid release version" >&2; exit 2 ;; esac
test "$(uname -s)" = Darwin || { echo "Run macOS packaging on macOS." >&2; exit 1; }
XCODEGEN_BIN="$("$ROOT/scripts/vendor-xcodegen.sh")"
export PATH="$XCODEGEN_BIN:$PATH"
OUTPUT="$ROOT/dist/releases/$VERSION"
STAGE="$ROOT/build/package/$ARCH"
DERIVED="$ROOT/build/native/$ARCH"
GH_CACHE="$("$ROOT/scripts/vendor-gh.sh" "$ARCH")"
mkdir -p "$OUTPUT" "$STAGE"
python3 "$ROOT/scripts/release-manifest.py" source --output "$STAGE/source.json"
xcodegen generate --spec "$ROOT/native/project.yml" --project "$ROOT/native"
xcodebuild -project "$ROOT/native/RepoReach.xcodeproj" -scheme RepoReach \
  -configuration Release -derivedDataPath "$DERIVED" \
  ARCHS="$ARCH" ONLY_ACTIVE_ARCH=NO MACOSX_DEPLOYMENT_TARGET=13.0 \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO build
APP="$STAGE/RepoReach.app"
rm -rf "$APP"
ditto "$DERIVED/Build/Products/Release/RepoReach.app" "$APP"
mkdir -p "$APP/Contents/Helpers" "$APP/Contents/Resources/Licenses"
(
  cd "$ROOT"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$GO_ARCH" go build -trimpath \
    -ldflags='-s -w -buildid=' -o "$APP/Contents/Helpers/artifact-fs" ./cmd/artifact-fs
)
cp "$GH_CACHE/gh_2.102.0_macOS_${GO_ARCH}/bin/gh" "$APP/Contents/Helpers/gh"
cp "$ROOT/LICENSE" "$APP/Contents/Resources/Licenses/ArtifactFS-Apache-2.0.txt"
cp "$GH_CACHE/LICENSE" "$APP/Contents/Resources/Licenses/GitHubCLI-MIT.txt"
python3 "$ROOT/scripts/bundle-go-licenses.py" "$APP/Contents/Resources/Licenses/GoDependencies"
python3 "$ROOT/scripts/bundle-gh-licenses.py" --binary "$APP/Contents/Helpers/gh" \
  --destination "$APP/Contents/Resources/Licenses/GitHubCLI-Dependencies"
cat > "$APP/Contents/Resources/Licenses/NOTICE.txt" <<'NOTICE'
RepoReach is an Enough Tools project built on Cloudflare ArtifactFS.
ArtifactFS and RepoReach source: https://github.com/enoughtools/reporeach
Upstream ArtifactFS: https://github.com/cloudflare/artifact-fs (Apache-2.0)
Bundled official GitHub CLI: https://github.com/cli/cli (MIT)
GitHub CLI version: 2.102.0. GitHub is a trademark of GitHub, Inc.
macFUSE is a separately installed dependency and is not redistributed here.
NOTICE
MARKETING_VERSION="${VERSION%%-*}"
MARKETING_VERSION="${MARKETING_VERSION%%+*}"
BUILD_NUMBER="${REPOREACH_BUILD_NUMBER:-1}"
for PLIST in "$APP/Contents/Info.plist" "$APP/Contents/PlugIns/RepoReachFinder.appex/Contents/Info.plist"; do
  /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $MARKETING_VERSION" "$PLIST"
  /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $BUILD_NUMBER" "$PLIST"
done
/usr/libexec/PlistBuddy -c "Add :RepoReachReleaseVersion string $VERSION" "$APP/Contents/Info.plist" 2>/dev/null || \
  /usr/libexec/PlistBuddy -c "Set :RepoReachReleaseVersion $VERSION" "$APP/Contents/Info.plist"
SIGNATURE=ad-hoc
if [ -n "$SIGN_IDENTITY" ]; then
  SIGNATURE=developer-id
  for HELPER in artifact-fs gh; do
    codesign --force --timestamp --options runtime --sign "$SIGN_IDENTITY" "$APP/Contents/Helpers/$HELPER"
  done
  codesign --force --timestamp --options runtime --entitlements "$ROOT/native/FinderExtension/Finder.entitlements" --sign "$SIGN_IDENTITY" \
    "$APP/Contents/PlugIns/RepoReachFinder.appex"
  codesign --force --timestamp --options runtime --sign "$SIGN_IDENTITY" "$APP"
else
  # Apple Silicon requires an executable signature even for unsigned betas.
  for HELPER in artifact-fs gh; do codesign --force --sign - "$APP/Contents/Helpers/$HELPER"; done
  codesign --force --entitlements "$ROOT/native/FinderExtension/Finder.entitlements" --sign - "$APP/Contents/PlugIns/RepoReachFinder.appex"
  codesign --force --sign - "$APP"
fi
codesign --verify --strict --verbose=2 "$APP/Contents/Helpers/artifact-fs"
codesign --verify --strict --verbose=2 "$APP/Contents/Helpers/gh"
codesign --verify --strict --verbose=2 "$APP/Contents/PlugIns/RepoReachFinder.appex"
codesign --verify --strict --verbose=2 "$APP"
NOTARIZED=false
submit_notary() {
  if [ -n "${REPOREACH_NOTARY_PROFILE:-}" ]; then
    xcrun notarytool submit "$1" --keychain-profile "$REPOREACH_NOTARY_PROFILE" --wait
  else
    : "${APPLE_API_KEY_PATH:?Notarization requires APPLE_API_KEY_PATH}"
    : "${APPLE_API_KEY_ID:?Notarization requires APPLE_API_KEY_ID}"
    : "${APPLE_API_ISSUER:?Notarization requires APPLE_API_ISSUER}"
    xcrun notarytool submit "$1" --key "$APPLE_API_KEY_PATH" \
      --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER" --wait
  fi
}
if [ "$NOTARIZE" = true ]; then
  test -n "$SIGN_IDENTITY" || { echo "Notarization requires Developer ID signing." >&2; exit 1; }
  NOTARY_ZIP="$STAGE/notarization.zip"
  ditto -c -k --sequesterRsrc --keepParent "$APP" "$NOTARY_ZIP"
  submit_notary "$NOTARY_ZIP"
  xcrun stapler staple "$APP"
  xcrun stapler validate "$APP"
  spctl --assess --type execute --verbose=2 "$APP"
  NOTARIZED=true
  rm "$NOTARY_ZIP"
fi
BASENAME="RepoReach-${VERSION}-macOS-${ARCH}"
ZIP="$OUTPUT/$BASENAME.zip"
DMG="$OUTPUT/$BASENAME.dmg"
rm -f "$ZIP" "$DMG"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$ZIP"
DMG_STAGE="$STAGE/dmg"
mkdir -p "$DMG_STAGE"
rm -rf "$DMG_STAGE/RepoReach.app"
ditto "$APP" "$DMG_STAGE/RepoReach.app"
ln -sfn /Applications "$DMG_STAGE/Applications"
hdiutil create -volname RepoReach -srcfolder "$DMG_STAGE" -ov -format UDZO \
  -imagekey zlib-level=9 "$DMG"
if [ -n "$SIGN_IDENTITY" ]; then codesign --force --timestamp --sign "$SIGN_IDENTITY" "$DMG"; fi
if [ "$NOTARIZED" = true ]; then
  submit_notary "$DMG"
  xcrun stapler staple "$DMG"
  xcrun stapler validate "$DMG"
  spctl --assess --type open --context context:primary-signature --verbose=2 "$DMG"
fi
python3 "$ROOT/scripts/release-manifest.py" record --directory "$OUTPUT" \
  --version "$VERSION" --arch "$ARCH" --signature "$SIGNATURE" --notarized "$NOTARIZED" --source-file "$STAGE/source.json"
echo "Packaged $ARCH: $OUTPUT ($SIGNATURE; notarized=$NOTARIZED)"
