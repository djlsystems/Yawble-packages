#!/bin/sh
# Builds one installable version of the azure plugin into <out>: the two static binaries the
# manifest's `platforms` names, each with the pinned bicep compiler beside it, plus the manifest and
# the skill. CGO_ENABLED=0 makes each binary static, so it needs nothing from the image. Each bicep
# binary is downloaded once from its release, kept in .bicep-cache, and refused unless its sha256 is
# the pinned one.
#   ./build.sh ~/plugins-build/azure/1.0.0
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out/bin/linux-x64" "$out/bin/linux-arm64" "$out/skills"
out=$(cd "$out" && pwd)
cd "$here"

bicep_version=v0.48.1
bicep_x64=b09ec25a9d376c1f8e33ede6ed22b587f915ad68488d5db77a6f9541748c7f6e
bicep_arm64=9cbf6a211137e894a863fbc842e355c2197fc2124a7bda902afe1bceffa35245
release=https://github.com/Azure/bicep/releases/download/$bicep_version
cache=$here/.bicep-cache/$bicep_version
mkdir -p "$cache"
go run ./tools/fetch "$release/bicep-linux-x64" "$bicep_x64" "$cache/bicep-linux-x64"
go run ./tools/fetch "$release/bicep-linux-arm64" "$bicep_arm64" "$cache/bicep-linux-arm64"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-x64/azure" .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-arm64/azure" .
cp "$cache/bicep-linux-x64" "$out/bin/linux-x64/bicep"
cp "$cache/bicep-linux-arm64" "$out/bin/linux-arm64/bicep"
chmod 755 "$out/bin/linux-x64/bicep" "$out/bin/linux-arm64/bicep"
cp plugin.json "$out/"
cp skills/plugin-azure.md "$out/skills/"
