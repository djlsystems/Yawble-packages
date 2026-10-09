#!/bin/sh
# Publishes dist/ as one GitHub Release: every zip and catalog.json, under <tag>, marked the latest
# release, so https://github.com/djlsystems/Yawble-packages/releases/latest/download/catalog.json is
# the catalog it carries.
#
#   scripts/build.sh catalog-2026.10.09.1              build with the tag first
#   scripts/publish.sh --dry-run catalog-2026.10.09.1  prints the tag and every asset, does nothing
#   scripts/publish.sh catalog-2026.10.09.1            publishes (needs gh, signed in)
#
# It refuses, dry run or not:
#   - a tag that is not catalog-<yyyy.mm.dd>.<n> with a real date and n from 1 (pkgtool tag, the
#     same rule scripts/build.sh applies);
#   - a working tree with changes (git status shows anything), since the release is cut from HEAD;
#   - a tag that already exists, here or on the remote (PUBLISH_REMOTE, origin by default);
#   - a HEAD that is not on the remote's main branch, which the release and the catalog's source
#     links point at: push or merge to main first;
#   - a dist/ whose catalog does not match its zips, misses a package, or links to another tag.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
repo=djlsystems/Yawble-packages
remote=${PUBLISH_REMOTE:-origin}
dist=$root/dist

dry_run=false
tag=
for arg in "$@"; do
  case $arg in
    --dry-run) dry_run=true ;;
    -*) echo "unknown option $arg" >&2; exit 2 ;;
    *) [ -z "$tag" ] || { echo "one tag only" >&2; exit 2; }; tag=$arg ;;
  esac
done
if [ -z "$tag" ]; then
  echo "usage: scripts/publish.sh [--dry-run] <tag>   (tag: catalog-<yyyy.mm.dd>.<n>)" >&2
  exit 2
fi

refuse() {
  echo "publish.sh: refused: $*" >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
(cd "$root/scripts/pkgtool" && go build -o "$work/pkgtool" .)
pkgtool=$work/pkgtool

if ! "$pkgtool" tag "$tag"; then
  refuse "tag $tag is not catalog-<yyyy.mm.dd>.<n> with a real date and n from 1, such as catalog-2026.10.09.1"
fi

cd "$root"
if [ -n "$(git status --porcelain)" ]; then
  git status --short >&2
  refuse "the working tree has changes; commit or remove them, so the release is HEAD"
fi

if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  refuse "tag $tag already exists in this repository; take the next number"
fi
if ! remote_tags=$(git ls-remote --tags "$remote" "refs/tags/$tag"); then
  refuse "cannot ask $remote whether tag $tag exists"
fi
if [ -n "$remote_tags" ]; then
  refuse "tag $tag already exists on $remote; take the next number"
fi

# The release is cut from HEAD and the catalog's source links point at main, so HEAD must already be
# on the remote's main. Fetch main's commit only when it is not here yet; the empty refmap keeps the
# fetch from moving any ref (only FETCH_HEAD is written), and nothing is pushed.
if ! main=$(git ls-remote --heads "$remote" refs/heads/main) || [ -z "$main" ]; then
  refuse "cannot read main on $remote"
fi
main=${main%%[[:space:]]*}
if ! git cat-file -e "$main^{commit}" 2>/dev/null; then
  git fetch -q --no-tags --refmap= "$remote" refs/heads/main || refuse "cannot fetch main from $remote"
fi
if ! git merge-base --is-ancestor HEAD "$main"; then
  refuse "HEAD $(git rev-parse --short HEAD) is not on $remote's main ($(git rev-parse --short "$main")); push or merge it to main first"
fi

[ -f "$dist/catalog.json" ] || refuse "no dist/catalog.json; run scripts/build.sh $tag first"
if ! "$pkgtool" verify "$tag" "$dist" "$root/packages" >&2; then
  refuse "dist/ is not a build of every package with tag $tag; run scripts/build.sh $tag"
fi

commit=$(git rev-parse HEAD)
set -- "$dist"/*.zip "$dist/catalog.json"

echo "tag:    $tag"
echo "commit: $commit"
echo "repo:   $repo (marked latest)"
echo "assets:"
for asset in "$@"; do
  printf '  %s (%s bytes)\n' "${asset##*/}" "$(wc -c <"$asset" | tr -d ' ')"
done

if $dry_run; then
  echo "dry run: nothing published"
  exit 0
fi

gh release create "$tag" "$@" \
  --repo "$repo" \
  --target "$commit" \
  --title "$tag" \
  --notes "Packages and catalog built from $commit. The current catalog is always https://github.com/djlsystems/Yawble-packages/releases/latest/download/catalog.json" \
  --latest
echo "published $tag"
