#!/bin/sh
# Builds one installable version of the Web Credentials plugin into <out>: the manifest, the
# launcher, the source and its one npm dependency (playwright-core, pinned by package-lock.json).
# Google Chrome is not in the package: the plugin fetches Google's stable build into its own folder
# on first use and keeps it current (src/chrome.mjs), or uses the platform's copy when that is
# current. Nothing is added to the image. Needs node, npm and sh.
#   ./build.sh <out folder>
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out"
out=$(cd "$out" && pwd)

# The runtime dependency, into <out>/node_modules, from the lock so the bytes are fixed.
cp "$here/package.json" "$here/package-lock.json" "$out/"
( cd "$out" && npm ci --omit=dev --no-audit --no-fund --loglevel=error )
# The plugin imports playwright-core as a library; its CLI launcher is a symlink, and a package
# holds no links, so drop node_modules/.bin. Any other stray symlink is removed too.
rm -rf "$out/node_modules/.bin"
find "$out/node_modules" -type l -delete 2>/dev/null || true

cp "$here/plugin.json" "$out/"
mkdir -p "$out/bin" "$out/src" "$out/skills"
cp "$here/bin/web-credentials" "$out/bin/"
chmod +x "$out/bin/web-credentials"
cp "$here"/src/*.mjs "$out/src/"
cp "$here"/skills/*.md "$out/skills/"
