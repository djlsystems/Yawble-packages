#!/bin/sh
# Builds one installable version of the stripe plugin into <out>: the two static binaries the
# manifest's `platforms` names, plus the manifest and the skill. CGO_ENABLED=0 makes each binary
# static, so it needs nothing from the image.
#   ./build.sh ~/plugins-build/stripe-plugin/2.0.1
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out/bin/linux-x64" "$out/bin/linux-arm64" "$out/skills"
out=$(cd "$out" && pwd)
cd "$here"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-x64/stripe" .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-arm64/stripe" .
cp plugin.json "$out/"
cp skills/plugin-stripe-plugin.md "$out/skills/"
