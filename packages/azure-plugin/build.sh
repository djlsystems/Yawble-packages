#!/bin/sh
# Builds one installable version of the azure plugin into <out>: the two static binaries the
# manifest's `platforms` names, plus the manifest and the skill. CGO_ENABLED=0 makes each binary
# static, so it needs nothing from the image. The bicep compiler is not in the package (each
# platform's is about 120 MB, over the Marketplace's 100 MB limit for a package): the plugin
# downloads its own platform's pinned release the first time it deploys a Bicep file, and uses it
# only when its sha256 is the one pinned in bicep.go.
#   ./build.sh ~/plugins-build/azure-plugin/3.0.0
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out/bin/linux-x64" "$out/bin/linux-arm64" "$out/skills"
out=$(cd "$out" && pwd)
cd "$here"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-x64/azure" .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-arm64/azure" .
cp plugin.json "$out/"
cp skills/plugin-azure-plugin.md "$out/skills/"
