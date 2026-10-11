#!/bin/sh
# Builds one installable version of the twilio plugin into <out>: the two static binaries the
# manifest's `platforms` names, plus the manifest. CGO_ENABLED=0 makes each binary static, so it
# needs nothing from the image.
#   ./build.sh /data/documents/<team>/twilio-1.0.0/plugins/twilio
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out/bin/linux-x64" "$out/bin/linux-arm64"
out=$(cd "$out" && pwd)
cd "$here"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-x64/twilio" .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out/bin/linux-arm64/twilio" .
cp plugin.json "$out/"
mkdir -p "$out/skills"
cp skills/twilio.md "$out/skills/"
