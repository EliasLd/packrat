#!/bin/sh
# Builds the release archives for every supported platform into dist/.
# The release workflow runs this on every version tag, and it is a plain
# script so the exact same release assets stay reproducible on any
# machine with Go installed.
#
# Usage: scripts/release.sh [version]
# The optional version is stamped into the binaries (-X main.version);
# it defaults to "dev" and the workflow passes the pushed tag.

set -eu
cd "$(dirname "$0")/.."

version=${1:-dev}
outdir=dist
rm -rf "$outdir"
mkdir -p "$outdir"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# archive | goos | goarch — the archives spell platforms the way users
# expect them ("macos"), only the build itself uses Go's darwin spelling.
while read -r archive goos goarch; do
    [ -n "$archive" ] || continue

    case $archive in
    *.zip) bin=packrat.exe ;;
    *) bin=packrat ;;
    esac

    echo "building $archive"
    mkdir "$work/$goos-$goarch"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "-s -w -X main.version=$version" \
        -o "$work/$goos-$goarch/$bin" ./cmd/packrat

    case $archive in
    # -j stores the binary without its directory, so archives unpack flat
    *.zip) zip -q -j "$outdir/$archive" "$work/$goos-$goarch/$bin" ;;
    *) tar -czf "$outdir/$archive" -C "$work/$goos-$goarch" "$bin" ;;
    esac
done <<'EOF'
packrat-linux-amd64.tar.gz    linux    amd64
packrat-linux-arm64.tar.gz    linux    arm64
packrat-macos-amd64.tar.gz    darwin   amd64
packrat-macos-arm64.tar.gz    darwin   arm64
packrat-windows.zip           windows  amd64
EOF

(cd "$outdir" && sha256sum packrat-* > checksums.txt)
ls -lh "$outdir"