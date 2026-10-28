#!/bin/sh
# release.sh VERSION: build static Linux binaries into dist/ and package
# them with the contrib files, ready for `gh release create`.
set -eu
version=${1:?usage: scripts/release.sh v0.9.0}
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist
for arch in amd64 arm64; do
	name=zorp-$version-linux-$arch
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "dist/$name/zorp" ./cmd/zorp
	cp README.md DEPLOY.md LICENSE "dist/$name/"
	cp -r contrib "dist/$name/"
	tar -C dist -czf "dist/$name.tar.gz" "$name"
	rm -rf "dist/$name"
done
(cd dist && sha256sum ./*.tar.gz > SHA256SUMS)
ls -l dist
