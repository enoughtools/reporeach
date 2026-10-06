#!/bin/bash
# Run production filesystem callbacks and bridge tests without an app or system mount.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

test "$(uname -s)" = Darwin || { echo "FSKit read tests require macOS." >&2; exit 1; }
SDK_MAJOR="$(xcrun --sdk macosx --show-sdk-version | cut -d. -f1)"
SWIFT_MAJOR="$(xcrun --sdk macosx swift --version | awk '/Swift version/ {for (i=1; i<NF; i++) if ($i == "version") {split($(i+1), v, "."); print v[1]; exit}}')"
test "$SDK_MAJOR" -ge 26 || { echo "FSKit read tests require the macOS 26 SDK or later." >&2; exit 1; }
test "$SWIFT_MAJOR" -ge 6 || { echo "FSKit read tests require Swift 6 or later." >&2; exit 1; }

# This fresh source-only package is the sole directory removed by this script.
# Its deployment/language settings match the real FSVolume callback contract.
PACKAGE_DIR="$(mktemp -d /tmp/rr-volume-reads.XXXXXX)"
trap 'rm -rf -- "$PACKAGE_DIR"' EXIT
mkdir -p "$PACKAGE_DIR/Sources/NativeFilesystem" "$PACKAGE_DIR/Tests/NativeFilesystemTests"
for source in BridgeModels BridgeConfiguration BridgeClient BridgeTransport RepoReachVolume RepoReachFileSystem; do
  cp "$ROOT/native/FSKitExtension/$source.swift" "$PACKAGE_DIR/Sources/NativeFilesystem/"
done
cp "$ROOT/native/Shared/FSBridgeContainer.swift" "$PACKAGE_DIR/Sources/NativeFilesystem/"
cp "$ROOT/native/FSKitTests/FSVolumeReadTests.swift" "$PACKAGE_DIR/Tests/NativeFilesystemTests/"
cp "$ROOT/native/FSKitTests/NativeVolumeXattrTests.swift" "$PACKAGE_DIR/Tests/NativeFilesystemTests/"
cp "$ROOT/native/Tests/FSBridgeTests.swift" "$PACKAGE_DIR/Tests/NativeFilesystemTests/"
cat > "$PACKAGE_DIR/Package.swift" <<'SWIFT'
// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "RepoReachVolumeReadValidation",
    platforms: [.macOS("15.4")],
    targets: [
        .target(name: "NativeFilesystem"),
        .testTarget(name: "NativeFilesystemTests", dependencies: ["NativeFilesystem"])
    ],
    swiftLanguageVersions: [.v5]
)
SWIFT

xcrun --sdk macosx swift test --package-path "$PACKAGE_DIR" \
  --filter 'FSVolumeReadTests|NativeVolumeXattrTests|FSBridgeTests' -Xswiftc -warnings-as-errors
