#!/bin/sh
# Lays out one installable version of the plugin in <out>: the manifest, the Python executable it
# names and the postings it reads. Nothing is compiled; the manifest `requires` python3.
#   ./build.sh /tmp/job-board-0.2.0
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out"
cp "$here/plugin.json" "$here/job-board" "$here/postings.json" "$out/"
chmod 755 "$out/job-board"
