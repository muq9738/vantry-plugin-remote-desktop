#!/usr/bin/env bash
# Build vantry-remote-helper with the system Swift toolchain (Xcode or CLT).
# Usage: ./build.sh -> bin/vantry-remote-helper (host arch, macOS 13.0 target)
#        ARCHS="arm64 x86_64" ./build.sh  -> universal binary via lipo
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${OUT_DIR:-$HERE/bin}"
PRODUCT_NAME="${PRODUCT_NAME:-vantry-remote-helper}"
ARCHS="${ARCHS:-$(uname -m)}"
MIN_MACOS="${MIN_MACOS:-13.0}"
SDK="$(xcrun --sdk macosx --show-sdk-path)"
mkdir -p "$OUT_DIR"

FRAMEWORKS=(
  -framework ScreenCaptureKit
  -framework VideoToolbox
  -framework CoreMedia
  -framework CoreVideo
  -framework CoreGraphics
  -framework Accelerate
  -framework Foundation
)

slices=()
for arch in $ARCHS; do
  out="$OUT_DIR/$PRODUCT_NAME-$arch"
  echo "swiftc $arch -> $out" >&2
  xcrun swiftc -O -whole-module-optimization \
    -sdk "$SDK" -target "$arch-apple-macos$MIN_MACOS" \
    "${FRAMEWORKS[@]}" \
    -o "$out" "$HERE/Sources/main.swift"
  slices+=("$out")
done

if [ "${#slices[@]}" -gt 1 ]; then
  lipo -create -output "$OUT_DIR/$PRODUCT_NAME" "${slices[@]}"
else
  cp "${slices[0]}" "$OUT_DIR/$PRODUCT_NAME"
fi
codesign --force --sign - "$OUT_DIR/$PRODUCT_NAME" >/dev/null 2>&1
codesign --verify --strict "$OUT_DIR/$PRODUCT_NAME"
file "$OUT_DIR/$PRODUCT_NAME" >&2
