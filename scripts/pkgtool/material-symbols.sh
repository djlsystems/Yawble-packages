#!/bin/sh
# Writes material-symbols-outlined.txt beside it: every icon name in the Material Symbols (Outlined)
# font, from the codepoints file Google publishes with the font in google/material-design-icons, at
# the commit given (so a rerun with the same commit writes the same file). pkgtool reads it to check
# a plugin event's monitor icons. Needs curl and sh.
#
#   scripts/pkgtool/material-symbols.sh <commit of github.com/google/material-design-icons>
set -eu

if [ $# -ne 1 ]; then
  echo "usage: scripts/pkgtool/material-symbols.sh <commit>" >&2
  exit 2
fi
commit=$1
dir=$(cd "$(dirname "$0")" && pwd)
url="https://github.com/google/material-design-icons/raw/$commit/variablefont/MaterialSymbolsOutlined%5BFILL,GRAD,opsz,wght%5D.codepoints"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM
curl -fsSL "$url" -o "$tmp"
{
  echo "# Material Symbols (Outlined) icon names, one per line, generated - do not edit by hand."
  echo "# From: $url"
  echo "# Regenerate: scripts/pkgtool/material-symbols.sh $commit"
  cut -d' ' -f1 "$tmp" | LC_ALL=C sort -u
} >"$dir/material-symbols-outlined.txt"
