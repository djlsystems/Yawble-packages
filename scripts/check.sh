#!/bin/sh
# Checks the repository before a release:
#
#   build    every package builds (scripts/build.sh), into dist/
#   tests    every Go module's own tests pass: each plugin's (the mail plugin's unit tests and its
#            verification suite included) and the build's helper
#   catalog  dist/catalog.json matches the zips (sha256 and bytes), lists every package and every zip,
#            names the tag in every download link, and says what the manifests say
#   people   no file holds a real person's email address, domain or name (see scripts/pkgtool/scan.go
#            and scripts/allowed-hosts.txt)
#
#   scripts/check.sh                                  all four, with today's tag catalog-<date>.1
#   scripts/check.sh --tag catalog-2026.10.09.1       all four, with that tag
#   scripts/check.sh catalog people                   only those (catalog reads the dist/ there is)
#
# It runs every check asked for even after one fails, then exits 1 if any did. Needs Go and sh only;
# with git, the people check also looks for everyone in the repository's history.
set -u

root=$(cd "$(dirname "$0")/.." && pwd)
tag=${CATALOG_TAG:-catalog-$(date -u +%Y.%m.%d).1}
checks=
while [ $# -gt 0 ]; do
  case $1 in
    --tag) [ $# -ge 2 ] || { echo "--tag needs a tag" >&2; exit 2; }; tag=$2; shift 2 ;;
    build|tests|catalog|people) checks="$checks $1"; shift ;;
    *) echo "usage: scripts/check.sh [--tag <tag>] [build] [tests] [catalog] [people]" >&2; exit 2 ;;
  esac
done
checks=${checks# }
checks=${checks:-build tests catalog people}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
if ! (cd "$root/scripts/pkgtool" && go build -o "$work/pkgtool" .); then
  echo "FAIL: the build's helper scripts/pkgtool does not build" >&2
  exit 1
fi
pkgtool=$work/pkgtool

failed=
result() { # result <check> <status>
  if [ "$2" -eq 0 ]; then echo "== $1: PASS"; else echo "== $1: FAIL"; failed="$failed $1"; fi
}

check_build() {
  sh "$root/scripts/build.sh" "$tag"
}

check_tests() {
  status=0
  for mod in $(cd "$root" && find packages scripts -name go.mod -not -path '*/node_modules/*' | sort); do
    dir=${mod%/go.mod}
    echo "-- go test in $dir"
    (cd "$root/$dir" && go test -count=1 ./...) || status=1
  done
  return $status
}

check_catalog() {
  "$pkgtool" verify "$tag" "$root/dist" "$root/packages"
}

check_people() {
  people=$work/people
  : >"$people"
  if git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "$root" log --format='%an%n%ae%n%cn%n%ce' >>"$people" 2>/dev/null
    git -C "$root" config user.name >>"$people"
    git -C "$root" config user.email >>"$people"
  fi
  "$pkgtool" scan "$root" "$root/scripts/allowed-hosts.txt" "$people"
}

for c in $checks; do
  echo "== $c"
  case $c in
    build) check_build ;;
    tests) check_tests ;;
    catalog) check_catalog ;;
    people) check_people ;;
  esac
  result "$c" $?
done

if [ -n "$failed" ]; then
  echo "check.sh: FAILED:$failed"
  exit 1
fi
echo "check.sh: all passed ($checks)"
