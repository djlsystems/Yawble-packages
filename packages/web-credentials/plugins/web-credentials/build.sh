#!/bin/sh
# Builds one installable version of the Web Credentials plugin into <out>: the manifest, the
# launcher, the source, its one npm dependency (playwright-core, pinned by package-lock.json) and
# the Chromium headless shell that drives the sign-ins. Everything the plugin needs is in <out>;
# nothing is added to the image. Needs node, npm, sh, and either a network for the first fetch or a
# Playwright browser cache (PLAYWRIGHT_BROWSERS_PATH) that already holds the shell.
#   ./build.sh <out folder>
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out"
out=$(cd "$out" && pwd)

# The runtime dependency, into <out>/node_modules, from the lock so the bytes are fixed.
cp "$here/package.json" "$here/package-lock.json" "$out/"
( cd "$out" && npm ci --omit=dev --no-audit --no-fund --loglevel=error )

# The Chromium headless shell, laid out as Playwright lays it out, under <out>.
PW_CORE="$out/node_modules/playwright-core" sh "$here/tools/browser.sh" "$out"

cp "$here/plugin.json" "$out/"
mkdir -p "$out/bin" "$out/src" "$out/skills"
cp "$here/bin/web-credentials" "$out/bin/"
chmod +x "$out/bin/web-credentials"
cp "$here"/src/*.mjs "$out/src/"
cp "$here"/skills/*.md "$out/skills/"
