#!/bin/sh
# Build release packages: one self-contained binary per platform, in dist/.
#   scripts/dist.sh [version]      (default: git describe, else "dev")
# The binary has no dependencies: the web interface is embedded and nothing
# links against C, so any Linux or macOS machine of the right CPU can run it.
set -eu
cd "$(dirname "$0")/.."
version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
	os=${target%/*} arch=${target#*/}
	name="arborist-$version-$os-$arch"
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "dist/$name/arb" ./cmd/arb
	cp README.md "dist/$name/"
	tar -C dist -czf "dist/$name.tar.gz" "$name"
	rm -rf "dist/$name"
done
(cd dist && (sha256sum *.tar.gz 2>/dev/null || shasum -a 256 *.tar.gz) > SHA256SUMS)
ls -l dist
