#!/bin/bash
# Keep project generation consistent without changing the host's installation.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION=2.46.0
EXPECTED=4d9e34b62172d645eed6457cac13fc222569974098ef4ee9c3368bedf0196806
CACHE="$ROOT/build/vendor/xcodegen/$VERSION"
mkdir -p "$CACHE"
if [ ! -f "$CACHE/xcodegen.zip" ]; then
  curl --fail --silent --show-error --location --retry 3 \
    "https://github.com/yonaskolb/XcodeGen/releases/download/$VERSION/xcodegen.zip" \
    --output "$CACHE/xcodegen.zip.part"
  mv "$CACHE/xcodegen.zip.part" "$CACHE/xcodegen.zip"
fi
test "$(shasum -a 256 "$CACHE/xcodegen.zip" | awk '{print $1}')" = "$EXPECTED" || {
  echo "XcodeGen checksum mismatch." >&2; exit 1;
}
ditto -x -k "$CACHE/xcodegen.zip" "$CACHE"
test -x "$CACHE/xcodegen/bin/xcodegen"
printf '%s\n' "$CACHE/xcodegen/bin"
