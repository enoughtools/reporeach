#!/bin/bash
# Fetch a fixed official GitHub CLI release and verify every downloaded byte.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GH_VERSION=2.102.0
ARCH="${1:-arm64}"
case "$ARCH" in
  arm64) GH_ARCH=arm64; EXPECTED=da922c20d1792e5b2cbf375593d7a658acf034c12c84e007e71c76ef959c337e ;;
  x86_64|amd64) GH_ARCH=amd64; EXPECTED=b245f24eb2bf5f75b426b4c26da3651a107f8d5b6f4fddfbfccc5679041378b3 ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 2 ;;
esac
CACHE="$ROOT/build/vendor/gh/$GH_VERSION/$GH_ARCH"
ARCHIVE="gh_${GH_VERSION}_macOS_${GH_ARCH}.zip"
mkdir -p "$CACHE"
if [ ! -f "$CACHE/$ARCHIVE" ]; then
  curl --fail --silent --show-error --location --retry 3 \
    "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${ARCHIVE}" \
    --output "$CACHE/$ARCHIVE.part"
  mv "$CACHE/$ARCHIVE.part" "$CACHE/$ARCHIVE"
fi
ACTUAL="$(shasum -a 256 "$CACHE/$ARCHIVE" | awk '{print $1}')"
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "GitHub CLI checksum mismatch. Remove $CACHE/$ARCHIVE and retry." >&2
  exit 1
fi
# Reextract after verification so cached executables cannot bypass integrity.
ditto -x -k "$CACHE/$ARCHIVE" "$CACHE"
if [ ! -f "$CACHE/LICENSE" ]; then
  curl --fail --silent --show-error --location --retry 3 \
    "https://raw.githubusercontent.com/cli/cli/v${GH_VERSION}/LICENSE" \
    --output "$CACHE/LICENSE.part"
  mv "$CACHE/LICENSE.part" "$CACHE/LICENSE"
fi
LICENSE_SHA=6da4adc42392c8485e40b4251c7e332fc3352df1947c9ffade71dd60b14a7a4f
test "$(shasum -a 256 "$CACHE/LICENSE" | awk '{print $1}')" = "$LICENSE_SHA" || {
  echo "GitHub CLI license checksum mismatch." >&2; exit 1;
}
printf '%s\n' "$CACHE"
