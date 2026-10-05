#!/bin/bash
# Build and package one macOS architecture. Network dependencies are pinned.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export GOTOOLCHAIN=go1.26.8
ARCH=arm64
VERSION=0.1.0-beta.3
BACKEND=fskit
COMPILE_ONLY=false
VALIDATION_ARTIFACT=false
VALIDATION_ROOT=""
SIGN_IDENTITY="${REPOREACH_SIGN_IDENTITY:-}"
NOTARIZE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --backend) BACKEND="$2"; shift 2 ;;
    --compile-only) COMPILE_ONLY=true; shift ;;
    --validation-artifact) COMPILE_ONLY=true; VALIDATION_ARTIFACT=true; SIGN_IDENTITY=""; shift ;;
    --validation-root) test "$#" -ge 2 && test -n "$2" || { echo "--validation-root requires an absolute generation path." >&2; exit 2; }; VALIDATION_ROOT="$2"; shift 2 ;;
    --unsigned) SIGN_IDENTITY=""; shift ;;
    --sign-identity) SIGN_IDENTITY="$2"; shift 2 ;;
    --notarize) NOTARIZE=true; shift ;;
    *) echo "Usage: $0 [--arch arm64|x86_64] [--version VERSION] [--backend fskit|macfuse] [--compile-only|--validation-artifact [--validation-root ABSOLUTE_PATH]] [--unsigned|--sign-identity ID] [--notarize]" >&2; exit 2 ;;
  esac
done
case "$ARCH" in arm64) GO_ARCH=arm64 ;; x86_64) GO_ARCH=amd64 ;; *) echo "Unsupported architecture: $ARCH" >&2; exit 2 ;; esac
case "$VERSION" in *[!A-Za-z0-9.+-]*|"") echo "Invalid release version" >&2; exit 2 ;; esac
if [ -n "$VALIDATION_ROOT" ]; then
  test "$VALIDATION_ARTIFACT" = true || { echo "--validation-root is only valid with --validation-artifact." >&2; exit 2; }
  python3 - "$ROOT" "$VALIDATION_ROOT" <<'PY'
import os, pathlib, sys
base = pathlib.Path(sys.argv[1]) / "build/fskit-validation/generations"
path = pathlib.Path(sys.argv[2])
if not path.is_absolute() or os.path.abspath(sys.argv[2]) != sys.argv[2] or base not in path.parents:
    raise SystemExit("--validation-root must be a normalized absolute generation path under build/fskit-validation/generations.")
if any(parent.is_symlink() or (parent.exists() and not parent.is_dir()) for parent in (path, *path.parents)):
    raise SystemExit("--validation-root cannot pass through symlinks or non-directory paths.")
PY
fi
test "$(uname -s)" = Darwin || { echo "Run macOS packaging on macOS." >&2; exit 1; }
if [ "$VALIDATION_ARTIFACT" = true ] && { [ "$BACKEND" != fskit ] || [ -n "$SIGN_IDENTITY" ] || [ "$NOTARIZE" = true ]; }; then
  echo "--validation-artifact only exports unsigned FSKit products for local validation; it cannot sign, notarize or publish." >&2
  exit 2
fi
case "$BACKEND" in
  fskit)
    PROJECT_SPEC="$ROOT/native/project.yml"
    SCHEME=RepoReachFSKit
    XCODE_MAJOR="$(xcodebuild -version | awk '/^Xcode / {split($2, v, "."); print v[1]}')"
    SDK_MAJOR="$(xcrun --sdk macosx --show-sdk-version | cut -d. -f1)"
    if [ "$XCODE_MAJOR" -lt 26 ] || [ "$SDK_MAJOR" -lt 26 ]; then
      echo "The bundled FSKit module requires Xcode 26 and the macOS 26 SDK. Use the macos-26 CI validation job or install a compatible Xcode." >&2
      exit 1
    fi
    if [ "$COMPILE_ONLY" = false ]; then
      test -n "$SIGN_IDENTITY" || { echo "FSKit distribution requires Developer ID signing. Use --compile-only for unsigned validation." >&2; exit 1; }
      : "${REPOREACH_FSKIT_PROFILE:?FSKit distribution requires the actual matching Developer ID provisioning profile path}"
      test -f "$REPOREACH_FSKIT_PROFILE" || { echo "FSKit provisioning profile is missing." >&2; exit 1; }
    fi
    ;;
  macfuse)
    test "$COMPILE_ONLY" = true || { echo "The legacy backend is available for source compilation only; the current Darwin engine requires the bundled FSKit module." >&2; exit 1; }
    PROJECT_SPEC="$ROOT/native/project-macfuse.yml"; SCHEME=RepoReach
    ;;
  *) echo "Unsupported filesystem backend: $BACKEND" >&2; exit 2 ;;
esac
if [ "$COMPILE_ONLY" = true ] && [ "$NOTARIZE" = true ]; then
  echo "--compile-only cannot notarize or publish archives." >&2; exit 2
fi
XCODEGEN_BIN="$("$ROOT/scripts/vendor-xcodegen.sh")"
export PATH="$XCODEGEN_BIN:$PATH"
OUTPUT="$ROOT/dist/releases/$VERSION"
STAGE="$ROOT/build/package/$BACKEND/$ARCH"
DERIVED="$ROOT/build/native/$BACKEND/$ARCH"
if [ "$VALIDATION_ARTIFACT" = true ]; then
  VALIDATION_ROOT="${VALIDATION_ROOT:-$ROOT/build/fskit-validation}"
  OUTPUT="$VALIDATION_ROOT/products/$ARCH"
  STAGE="$VALIDATION_ROOT/stage/$ARCH"
  BASENAME="RepoReach-local-validation-$ARCH"
  if [ -e "$OUTPUT/$BASENAME.zip" ]; then
    echo "Local validation archive already exists; refusing to overwrite it: $OUTPUT/$BASENAME.zip" >&2
    exit 1
  fi
  mkdir -p "$OUTPUT" "$STAGE"
  python3 "$ROOT/scripts/release-manifest.py" source --output "$STAGE/source.json"
fi
if [ "$COMPILE_ONLY" = false ]; then
  BASENAME="RepoReach-${VERSION}-macOS-${ARCH}"
  if [ -e "$OUTPUT/$BASENAME.zip" ] || [ -e "$OUTPUT/$BASENAME.dmg" ]; then
    echo "Release archives already exist for this version and architecture; refusing to overwrite them. Choose a new release version." >&2
    exit 1
  fi
  mkdir -p "$OUTPUT" "$STAGE"
  python3 "$ROOT/scripts/release-manifest.py" source --output "$STAGE/source.json"
fi
xcodegen generate --spec "$PROJECT_SPEC" --project "$ROOT/native"
xcodebuild -project "$ROOT/native/RepoReach.xcodeproj" -scheme "$SCHEME" \
  -configuration Release -derivedDataPath "$DERIVED" \
  ARCHS="$ARCH" ONLY_ACTIVE_ARCH=NO \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO \
  REGISTER_APP_WITH_LAUNCH_SERVICES=NO build
if [ "$COMPILE_ONLY" = true ]; then
  if [ "$BACKEND" = fskit ]; then
    python3 "$ROOT/scripts/validate-fskit-bundle.py" compile \
      --app "$DERIVED/Build/Products/Release/RepoReach.app" --arch "$ARCH" \
      --entitlements "$ROOT/native/FSKitExtension/FSKit.entitlements"
  fi
  if [ "$VALIDATION_ARTIFACT" = false ]; then
    echo "Compiled $BACKEND $ARCH without release archives: $DERIVED/Build/Products/Release/RepoReach.app"
    exit 0
  fi
fi
GH_CACHE="$("$ROOT/scripts/vendor-gh.sh" "$ARCH")"
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
NOTICE
if [ "$BACKEND" = fskit ]; then
  echo "FSKit is supplied by macOS. RepoReach's FSKit module is included in this app." >> "$APP/Contents/Resources/Licenses/NOTICE.txt"
else
  echo "macFUSE is a separately installed dependency and is not redistributed here." >> "$APP/Contents/Resources/Licenses/NOTICE.txt"
fi
if [ "$VALIDATION_ARTIFACT" = true ]; then
  # This export has no module provisioning profile or Developer ID signature.
  # Preserve the normal distribution guard above and all release paths below.
  python3 "$ROOT/scripts/validation-artifact.py" --app "$APP" --arch "$ARCH" \
    --source-file "$STAGE/source.json" --output "$OUTPUT"
  echo "Exported unsigned local-validation product: $OUTPUT (requires local authorized signing before extension activation)"
  echo "The app group remains unresolved until signing binds the app, engine and module to the actual developer team."
  exit 0
fi
MARKETING_VERSION="${VERSION%%-*}"
MARKETING_VERSION="${MARKETING_VERSION%%+*}"
BUILD_NUMBER="${REPOREACH_BUILD_NUMBER:-3}"
PLISTS=("$APP/Contents/Info.plist" "$APP/Contents/PlugIns/RepoReachFinder.appex/Contents/Info.plist")
if [ "$BACKEND" = fskit ]; then PLISTS+=("$APP/Contents/Extensions/RepoReachFSKit.appex/Contents/Info.plist"); fi
for PLIST in "${PLISTS[@]}"; do
  /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $MARKETING_VERSION" "$PLIST"
  /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $BUILD_NUMBER" "$PLIST"
done
/usr/libexec/PlistBuddy -c "Add :RepoReachReleaseVersion string $VERSION" "$APP/Contents/Info.plist" 2>/dev/null || \
  /usr/libexec/PlistBuddy -c "Set :RepoReachReleaseVersion $VERSION" "$APP/Contents/Info.plist"
SIGNATURE=ad-hoc
if [ -n "$SIGN_IDENTITY" ]; then
  SIGNATURE=developer-id
  # Inspect an actual signed component to resolve the team; identity display
  # names and App ID prefixes are not authoritative TeamIdentifier values.
  codesign --force --timestamp --options runtime --sign "$SIGN_IDENTITY" "$APP/Contents/Helpers/gh"
  if [ "$BACKEND" = fskit ]; then
    FSMODULE="$APP/Contents/Extensions/RepoReachFSKit.appex"
    FSKIT_CLAIMS="$STAGE/fskit-signing"
    python3 "$ROOT/scripts/validate-fskit-bundle.py" prepare \
      --profile "$REPOREACH_FSKIT_PROFILE" --bundle-id com.enoughtools.reporeach.fskit \
      --entitlements "$ROOT/native/FSKitExtension/FSKit.entitlements" \
      --app "$APP" --signing-component "$APP/Contents/Helpers/gh" \
      --app-entitlements "$ROOT/native/App/App.entitlements" \
      --helper-entitlements "$ROOT/native/Helpers/artifact-fs.entitlements" \
      --output-directory "$FSKIT_CLAIMS"
    codesign --force --timestamp --options runtime --entitlements "$FSKIT_CLAIMS/helper.entitlements" --sign "$SIGN_IDENTITY" "$APP/Contents/Helpers/artifact-fs"
    cp "$REPOREACH_FSKIT_PROFILE" "$FSMODULE/Contents/embedded.provisionprofile"
    codesign --force --timestamp --options runtime --entitlements "$FSKIT_CLAIMS/module.entitlements" --sign "$SIGN_IDENTITY" "$FSMODULE"
  else
    codesign --force --timestamp --options runtime --sign "$SIGN_IDENTITY" "$APP/Contents/Helpers/artifact-fs"
  fi
  codesign --force --timestamp --options runtime --entitlements "$ROOT/native/FinderExtension/Finder.entitlements" --sign "$SIGN_IDENTITY" \
    "$APP/Contents/PlugIns/RepoReachFinder.appex"
  if [ "$BACKEND" = fskit ]; then
    codesign --force --timestamp --options runtime --entitlements "$FSKIT_CLAIMS/app.entitlements" --sign "$SIGN_IDENTITY" "$APP"
  else
    codesign --force --timestamp --options runtime --sign "$SIGN_IDENTITY" "$APP"
  fi
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
if [ "$BACKEND" = fskit ]; then
  python3 "$ROOT/scripts/validate-fskit-bundle.py" signed --app "$APP" --arch "$ARCH" \
    --entitlements "$ROOT/native/FSKitExtension/FSKit.entitlements"
fi
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
RECORD_OPTIONS=(--backend "$BACKEND")
if [ "$BACKEND" = fskit ]; then RECORD_OPTIONS+=(--fskit-profile "$FSMODULE/Contents/embedded.provisionprofile"); fi
python3 "$ROOT/scripts/release-manifest.py" record --directory "$OUTPUT" \
  --version "$VERSION" --arch "$ARCH" --signature "$SIGNATURE" --notarized "$NOTARIZED" --source-file "$STAGE/source.json" "${RECORD_OPTIONS[@]}"
echo "Packaged $ARCH: $OUTPUT ($SIGNATURE; notarized=$NOTARIZED)"
