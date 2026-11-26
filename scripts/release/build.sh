#!/usr/bin/env bash
# Builds the release archives: every kiln command for linux/amd64 and
# linux/arm64, static (CGO off) and reproducible (-trimpath, empty build ID,
# archive timestamps and owners fixed to the commit), so anyone can rebuild a
# release from its tag and get the same SHA-256.
#
#   scripts/release/build.sh VERSION OUT_DIR
#
# Writes OUT_DIR/kiln_VERSION_linux_ARCH.tar.gz and OUT_DIR/SHA256SUMS.
set -euo pipefail
command -v go > /dev/null || export PATH="/usr/local/go/bin:$PATH"

VERSION=${1:?usage: build.sh VERSION OUT_DIR}
OUT=${2:?usage: build.sh VERSION OUT_DIR}
KILN=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$KILN" log -1 --format=%ct)}
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

cd "$KILN"
for arch in amd64 arm64; do
  name="kiln_${VERSION}_linux_${arch}"
  mkdir -p "$STAGE/$name"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w -buildid=" \
    -o "$STAGE/$name/" ./cmd/...
  cp LICENSE README.md "$STAGE/$name/"
  mkdir -p "$STAGE/$name/cmake" && cp cmake/kiln.cmake "$STAGE/$name/cmake/"
  tar -C "$STAGE" --sort=name --mtime="@$EPOCH" --owner=0 --group=0 --numeric-owner \
    -cf - "$name" | gzip -n -9 > "$OUT/$name.tar.gz"
done
(cd "$OUT" && sha256sum kiln_*.tar.gz > SHA256SUMS && cat SHA256SUMS)
