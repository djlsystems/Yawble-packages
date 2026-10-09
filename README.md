# Yawble packages

Solution packages and plugins for [Yawble](https://github.com/djlsystems/Yawble), built from this repository, checked, and published with a catalog that the marketplace on the website and the product's own gallery read.

| Package | Kind | What it is |
|---|---|---|
| [`mail`](packages/mail) | solution | A team that reads, files, watches and drafts or sends your mail, with the `mail` plugin (Go). |
| [`job-tracker`](packages/job-tracker) | solution | A team that finds job postings, tracks them on a page and drafts cover letters, with the `job-board` plugin (Python). |
| [`sample-whoami-go`](packages/sample-whoami-go) | plugin | A small Go plugin that reports the Google account bound to it: the end-to-end check for connections. |

## Layout

```
packages/<id>/                   one folder per package, named after its manifest's id
  solution.json                  a solution package (the team), with:
  README.md                        for people
  skills/<name>.md                 the team's skills
  sites/<name>/                    each site, index.html at its root
  tools/                           code the members' instructions run
  plugins/<plugin-id>/             each plugin's SOURCE, with its build.sh
packages/<id>/                   or a plugin package: one plugin's source,
  plugin.json                      its manifest
  build.sh                         and its build.sh
scripts/build.sh                 builds every package into dist/ and writes dist/catalog.json
scripts/check.sh                 builds, tests, checks the catalog and scans for real people
scripts/publish.sh               uploads dist/ as one GitHub Release
scripts/allowed-hosts.txt        the hosts check.sh accepts, each with why
scripts/pkgtool/                 the Go helper the scripts use (manifests, zips, catalog, scan)
docs/catalog.md                  every catalog field, and when the schema number goes up
dist/                            build output, never committed
```

Solution packages follow Yawble's format (`docs/solutions.md` in the Yawble repository) and plugins
its manifest (`docs/plugins.md`), with one difference: here `plugins/<plugin-id>/` holds the plugin's
source, and the build puts the built version there in the zip.

### `build.sh`, the one contract every plugin keeps

```
build.sh <out>
```

lays out one installable version of the plugin in `<out>`: `plugin.json` at its root and every file
it names - for a Go plugin, a static binary for each of `linux-x64` and `linux-arm64` under `bin/`,
named in the manifest's `platforms`, and its skills. Nothing else goes in `<out>`: no sources, no
tests. It must work from any current folder (`cd "$(dirname "$0")"`), and need only Go and sh.

## Build

```sh
scripts/build.sh catalog-2026.10.09.1
```

needs Go (the version the packages' `go.mod` name; Go fetches it if yours is older) and a POSIX sh,
nothing else. It empties `dist/` and writes:

- `dist/<id>-<version>.zip` for every package;
  - a **solution** zip's root is the package folder's content - `solution.json` at the root - with
    each `plugins/<plugin-id>/` replaced by the version its `build.sh` built. It is what Yawble's
    Solutions > Install from a folder > **Upload a package (.zip)** unpacks and checks;
  - a **plugin** zip's root is the built version - `plugin.json` and what it names - which Admin >
    Plugins installs once unpacked;
- `dist/catalog.json`, described field by field in [docs/catalog.md](docs/catalog.md).

The argument is the GitHub Release tag the zips will be published under, because every download
link in the catalog names it: `catalog-<yyyy.mm.dd>.<n>`, today's UTC date and a number from 1. It
can come from `CATALOG_TAG` instead. The build refuses a date that is not on the calendar and an
`n` of 0 or with a leading zero.

The zips are **reproducible**: the same commit built with the same Go gives byte-identical zips
(fixed entry times and modes, sorted entries, `-trimpath -buildvcs=false`). `generatedAt` in the
catalog is the build's time unless `SOURCE_DATE_EPOCH` is set.

## Check

```sh
scripts/check.sh                              # all four checks, tag catalog-<today>.1
scripts/check.sh --tag catalog-2026.10.09.1   # all four, with that tag
scripts/check.sh catalog people               # only some: build, tests, catalog, people
```

runs, in order, and reports `PASS` or `FAIL` for each; it runs every check even after one fails,
then exits 1 if any did:

| Check | What fails it |
|---|---|
| `build` | `scripts/build.sh <tag>` fails for any package. |
| `tests` | `go test ./...` fails in any Go module under `packages/` or `scripts/` - every plugin's own tests, the mail plugin's unit tests **and** its verification suite, and the helper's. |
| `catalog` | `dist/catalog.json` does not match `dist/` or [docs/catalog.md](docs/catalog.md): a field the page does not name, a `generatedAt` that is not UTC to the second, packages not ordered by id, a zip's sha256 or size differs, a zip or a package folder is not listed, an entry names a zip that is not there, a download link names another tag, or any other field is not what the zip's manifests say. |
| `people` | Any text file (tracked or not; not `.git`, binaries or zips) holds a real person's data: an email address at a domain that is not reserved; a host or domain, in a link or bare, that is not reserved, not `github.com/djlsystems`, not a Go module path a `go.mod`/`go.sum` names and not in `scripts/allowed-hosts.txt`; an authorship line (`Copyright`, `Author:`, `Signed-off-by:` ...) that does not name the project; or the name or address of anyone in the repository's git history or of the git user running it. |

Reserved means `example.com`, `example.net`, `example.org` and their subdomains, and anything under
`.example`, `.test`, `.invalid` or `.localhost`. A service a package really talks to (an API host)
goes in `scripts/allowed-hosts.txt` with a comment saying why; nothing else does. A program cannot
tell a real name from a made-up one on its own, so keep to made-up people (see Add a package).

Run `scripts/check.sh` before every release. It builds every package, so on a shared machine take
the heavy lease first.

Each module's tests can also be run on their own, from its folder:

```sh
(cd packages/mail/plugins/mail && go test ./...)               # the mail plugin's unit tests
(cd packages/mail/plugins/mail/verification && go test ./...)  # its black-box verification suite
(cd packages/sample-whoami-go && go test ./...)
(cd scripts/pkgtool && go test ./...)                           # the scripts' own helper
```

The verification suite builds the plugin from the folder above it; to check a packaged binary
instead, point `MAIL_BIN` at it:

```sh
MAIL_BIN=<unpacked mail zip>/plugins/mail/bin/linux-x64/mail go test -count=1 .
```

The `job-board` plugin is a Python script with no tests of its own; Yawble's solution tests exercise
the sample it comes from.

## Add a package

1. Make `packages/<id>/`, where `<id>` is the manifest's `id`: lowercase letters, digits and
   hyphens, at most 48 characters. The build refuses a folder named otherwise.
2. A **solution**: `solution.json` (format 1), `README.md`, `skills/`, `sites/`, `tools/` as the
   team needs, and each plugin's source under `plugins/<plugin-id>/` with a `build.sh` (above). A
   **plugin**: its source, `plugin.json` and `build.sh` at the package's root.
3. Write the manifest's `description` as the catalog should show it: plain text, its first sentence a
   summary on its own, paragraphs separated by blank lines, no HTML. Everything the catalog says
   about the package comes from the manifests (see [docs/catalog.md](docs/catalog.md)); there is no
   other place to type it.
4. Use only made-up people in examples and tests: `person@example.com`, `@example.com`, and other
   reserved names (`example.org`, `example.net`, `*.test`). No real person's name, email address or
   domain.
5. Run `scripts/check.sh`, then look at its zip and its catalog entry in `dist/`. Check a
   solution with Yawble's own check too: `yawble solution check <unpacked zip>`.

To release a new version of a package, raise `version` in its manifest (and in each plugin's
`plugin.json` whose code changed); the zip's name follows.

## Publish

`scripts/publish.sh <tag>` uploads every zip in `dist/` and `dist/catalog.json` as one GitHub
Release named by the tag, cut from `HEAD` and marked the latest release, so
`https://github.com/djlsystems/Yawble-packages/releases/latest/download/catalog.json` is always the
current catalog. The release is cut from `HEAD`, and the catalog's `source` links point at `main`,
so **push or merge your commit to `main` first** and publish from a checkout of it. The catalog's
download links name the tag `scripts/build.sh` was given, so build, check and publish with the same
tag:

```sh
git switch main && git pull                     # HEAD is what is on origin's main
scripts/build.sh catalog-2026.10.09.1
scripts/check.sh --tag catalog-2026.10.09.1
scripts/publish.sh --dry-run catalog-2026.10.09.1   # prints the tag and every asset, does nothing
scripts/publish.sh catalog-2026.10.09.1             # publishes
```

The tag is `catalog-<yyyy.mm.dd>.<n>`: the UTC date and a number from 1, raised for a second release
the same day. Publishing needs `gh`, signed in with the right to make releases in
`djlsystems/Yawble-packages`. Whether it is a dry run or not, it refuses:

- a tag not shaped `catalog-<yyyy.mm.dd>.<n>`, with a date not on the calendar, or with an `n` of 0
  or with a leading zero (the build's own rule: both ask `pkgtool tag`);
- a working tree with any change or untracked file (`git status` must be empty), since the release
  is cut from `HEAD`;
- a tag that already exists, here or on the remote (`origin`, or `PUBLISH_REMOTE`), or a remote it
  cannot ask;
- a `HEAD` that is not on the remote's `main` (it reads `main` with `git ls-remote`, fetches that
  commit if it is missing here, moving no ref, and never pushes);
- a `dist/` with no catalog, or whose catalog does not match its zips, misses a package or links to
  another tag (the same check as `scripts/check.sh catalog`).

Publishing is a person's step; the team only dry-runs it.
