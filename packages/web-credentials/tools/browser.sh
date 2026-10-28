#!/bin/sh
# Puts the Chromium headless shell that this plugin's playwright-core drives into <dest>, laid out as
# Playwright lays out its browsers (<dest>/chromium_headless_shell-<revision>/...):
#   tools/browser.sh <dest>
# A copy already at that revision under $PLAYWRIGHT_BROWSERS_PATH is copied (no download); else
# playwright-core's own installer downloads it. Only the shell is kept, with its en-US locale: the
# other locales and the hyphenation data are left out, which the plugin never uses.
# Needs node_modules/playwright-core (npm ci) and sh.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
dest=${1:?usage: tools/browser.sh <dest>}
mkdir -p "$dest"
dest=$(cd "$dest" && pwd)
core=$here/node_modules/playwright-core
if [ ! -f "$core/browsers.json" ]; then
  echo "tools/browser.sh: $core is missing; run npm ci in $here first" >&2
  exit 1
fi
revision=$(node -e 'const b=require(process.argv[1]).browsers.find(b=>b.name==="chromium-headless-shell");process.stdout.write(b.revision)' "$core/browsers.json")
name=chromium_headless_shell-$revision
shell=$dest/$name/chrome-headless-shell-linux64/chrome-headless-shell
if [ -x "$shell" ]; then
  exit 0
fi
cache=${PLAYWRIGHT_BROWSERS_PATH:-}
if [ -n "$cache" ] && [ -x "$cache/$name/chrome-headless-shell-linux64/chrome-headless-shell" ]; then
  rm -rf "$dest/$name.partial"
  cp -R "$cache/$name" "$dest/$name.partial"
else
  PLAYWRIGHT_BROWSERS_PATH="$dest/.download" node "$core/cli.js" install --only-shell chromium >&2
  rm -rf "$dest/$name.partial"
  mv "$dest/.download/$name" "$dest/$name.partial"
  rm -rf "$dest/.download"
fi
linux=$dest/$name.partial/chrome-headless-shell-linux64
find "$linux/locales" -type f ! -name en-US.pak -exec rm -f {} +
rm -rf "$linux/hyphen-data"
mv "$dest/$name.partial" "$dest/$name"
