#!/bin/sh
# Builds every package under packages/ into dist/: one zip per package, <id>-<version>.zip, and
# dist/catalog.json listing them, with download links under the GitHub Release <tag>.
#
#   scripts/build.sh catalog-2026.10.08.1
#   CATALOG_TAG=catalog-2026.10.08.1 scripts/build.sh
#
# The tag is the release the zips will be published under (scripts/publish.sh uses the same one);
# it is required because every download link in the catalog names it.
#
# A solution zip's root is the package folder's content (solution.json at the root) with each
# plugins/<id>/ replaced by the version its build.sh builds. A plugin zip's root is the version the
# package's build.sh builds (plugin.json and what it names). Needs Go and sh, and the .NET SDK
# for sample-echo.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
tag=${1:-${CATALOG_TAG:-}}
if [ -z "$tag" ]; then
  echo "usage: scripts/build.sh <tag>   (catalog-<yyyy.mm.dd>.<n>; or set CATALOG_TAG)" >&2
  exit 2
fi

# Static binaries with no build-machine paths or repository state in them, so a rebuild of the same
# commit with the same Go makes the same bytes.
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-trimpath -buildvcs=false"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
(cd "$root/scripts/pkgtool" && go build -o "$work/pkgtool" .)
pkgtool=$work/pkgtool
"$pkgtool" tag "$tag"

dist=$root/dist
rm -rf "$dist"
mkdir -p "$dist"

for dir in "$root"/packages/*/; do
  dir=${dir%/}
  # pkgtool has said why when it refuses a folder; stop there rather than split an empty answer.
  info=$("$pkgtool" info "$dir") || exit 1
  set -- $info
  kind=$1 id=$2 version=$3
  out=$work/stage/$id
  echo "building $id $version ($kind)"
  case $kind in
    solution)
      "$pkgtool" stage "$dir" "$out"
      for plugin in "$dir"/plugins/*/; do
        [ -d "$plugin" ] || continue
        plugin=${plugin%/}
        if [ ! -f "$plugin/build.sh" ]; then
          echo "$plugin has no build.sh" >&2
          exit 1
        fi
        sh "$plugin/build.sh" "$out/plugins/${plugin##*/}"
      done
      ;;
    plugin)
      if [ ! -f "$dir/build.sh" ]; then
        echo "$dir has no build.sh" >&2
        exit 1
      fi
      sh "$dir/build.sh" "$out"
      ;;
  esac
  "$pkgtool" zip "$out" "$dist/$id-$version.zip"
done

"$pkgtool" catalog "$tag" "$dist"
echo "wrote $dist:"
ls -l "$dist"
