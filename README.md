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
scripts/pkgtool/                 the Go helper build.sh uses (manifests, zips, catalog)
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
can come from `CATALOG_TAG` instead.

The zips are **reproducible**: the same commit built with the same Go gives byte-identical zips
(fixed entry times and modes, sorted entries, `-trimpath -buildvcs=false`). `generatedAt` in the
catalog is the build's time unless `SOURCE_DATE_EPOCH` is set.

## Test

Each plugin's own tests run from its folder:

```sh
(cd packages/mail/plugins/mail && go test ./...)               # the mail plugin's unit tests
(cd packages/mail/plugins/mail/verification && go test ./...)  # its black-box verification suite
(cd packages/sample-whoami-go && go test ./...)
(cd scripts/pkgtool && go test ./...)                           # the build's own helper
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
5. Run its tests, then `scripts/build.sh <tag>` and check its zip and its catalog entry. Check a
   solution with Yawble's own check too: `yawble solution check <unpacked zip>`.

To release a new version of a package, raise `version` in its manifest (and in each plugin's
`plugin.json` whose code changed); the zip's name follows.

## Publish

`scripts/publish.sh <tag>` uploads every zip in `dist/` and `catalog.json` as one GitHub Release
named by the tag, marked the latest, so
`https://github.com/djlsystems/Yawble-packages/releases/latest/download/catalog.json` is always the
current catalog. Build with the same tag first:

```sh
scripts/build.sh catalog-2026.10.09.1
scripts/publish.sh --dry-run catalog-2026.10.09.1   # prints what it would upload
scripts/publish.sh catalog-2026.10.09.1
```

It refuses a dirty working tree and a tag that already exists. Publishing is a person's step.
