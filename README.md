# Yawble packages

Solution packages and plugins for [Yawble](https://github.com/djlsystems/Yawble). This repository
builds each package into a zip and checks it. Packages are published on the
[yawble.ai](https://yawble.ai) Marketplace, which is what a Yawble instance's **Marketplace** reads.

| Package | Kind | What it is |
|---|---|---|
| [`mail`](packages/mail) | solution | A team that reads, files, watches and drafts or sends your mail, with the `mail` plugin (Go). |
| [`twilio`](packages/twilio) | solution | A team that texts through your Twilio account (SMS and MMS), with the `twilio` plugin (Go): dry run first, your numbers only until you add consented recipients. |
| [`stripe`](packages/stripe) | plugin | Sets up Stripe products, prices and webhook endpoints with a restricted key (Go); webhook signing secrets go straight into Azure Key Vault. Dry run first. |
| [`sample-echo`](packages/sample-echo) | plugin | A deterministic stand-in member (.NET) that upper-cases or reverses each instruction: for trying plugin members without a model. |
| [`sample-echo-go`](packages/sample-echo-go) | plugin | The same stand-in member written in Go. |

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
scripts/allowed-hosts.txt        the hosts check.sh accepts, each with why
scripts/pkgtool/                 the Go helper the scripts use (manifests, zips, catalog, scan)
dist/                            build output, never committed
```

Solution packages follow Yawble's format (`docs/solutions.md` in the Yawble repository) and plugins
its manifest (`docs/plugins.md`), with one difference: here `plugins/<plugin-id>/` holds the plugin's
source, and the build puts the built version there in the zip. A site ships inside a solution
package; the build takes no site on its own.

Each package and each plugin has its own `n.n.n` version, in its manifest's `version`.

### `build.sh`, the one contract every plugin keeps

```
build.sh <out>
```

lays out one installable version of the plugin in `<out>`: `plugin.json` at its root and every file
it names - for a Go plugin, a static binary for each of `linux-x64` and `linux-arm64` under `bin/`,
named in the manifest's `platforms`, and its skills. Nothing else goes in `<out>`: no sources, no
tests. It must work from any current folder (`cd "$(dirname "$0")"`), and need only Go and sh -
except `sample-echo`'s, which publishes a .NET project and needs the .NET SDK (10).

## Build

```sh
scripts/build.sh
```

needs Go (the version the packages' `go.mod` name; Go fetches it if yours is older), the .NET SDK
(10, for `sample-echo` only) and a POSIX sh, nothing else. It empties `dist/` and writes:

- `dist/<id>-<version>.zip` for every package;
  - a **solution** zip's root is the package folder's content - `solution.json` at the root - with
    each `plugins/<plugin-id>/` replaced by the version its `build.sh` built. It is what Yawble's
    Marketplace > Advanced > Install from a folder > **Upload a package (.zip)** unpacks and checks;
  - a **plugin** zip's root is the built version - `plugin.json` and what it names - which Admin >
    Plugins installs once unpacked;
- `dist/catalog.json`, one entry per zip, which the `catalog` check reads back (see
  [The catalog](#the-catalog)).

The zips are **reproducible**: the same commit built with the same Go gives byte-identical zips
(fixed entry times and modes, sorted entries, `-trimpath -buildvcs=false`, and a deterministic
`dotnet publish` with its paths mapped away). `generatedAt` in the catalog is the build's time
unless `SOURCE_DATE_EPOCH` is set.

## Check

```sh
scripts/check.sh                   # all four checks
scripts/check.sh catalog people    # only some: build, tests, catalog, people
```

runs, in order, and reports `PASS` or `FAIL` for each; it runs every check even after one fails,
then exits 1 if any did:

| Check | What fails it |
|---|---|
| `build` | `scripts/build.sh` fails for any package. |
| `tests` | `go test ./...` fails in any Go module under `packages/` or `scripts/` - every plugin's own tests, the mail plugin's unit tests **and** its verification suite, and the helper's. |
| `catalog` | `dist/catalog.json` does not match `dist/`: a field the catalog does not have, a `generatedAt` that is not UTC to the second, packages not ordered by id, a zip's sha256 or size differs, a zip or a package folder is not listed, an entry names a zip that is not there, a download link that is not the Marketplace's address for that version, or any other field is not what the zip's manifests say. |
| `people` | Any text file (tracked or not; not `.git`, binaries or zips) holds a real person's data: an email address at a domain that is not reserved; a host or domain, in a link or bare, that is not reserved, not `github.com/djlsystems`, not a Go module path a `go.mod`/`go.sum` names and not in `scripts/allowed-hosts.txt`; an authorship line (`Copyright`, `Author:`, `Signed-off-by:` ...) that does not name the project; or the name or address of anyone in the repository's git history or of the git user running it. |

Reserved means `example.com`, `example.net`, `example.org` and their subdomains, and anything under
`.example`, `.test`, `.invalid` or `.localhost`. A service a package really talks to (an API host)
goes in `scripts/allowed-hosts.txt` with a comment saying why; nothing else does. A program cannot
tell a real name from a made-up one on its own, so keep to made-up people (see Add a package).

Run `scripts/check.sh` before you publish a package. It builds every package, so on a shared machine
take the heavy lease first.

Each module's tests can also be run on their own, from its folder:

```sh
(cd packages/mail/plugins/mail && go test ./...)               # the mail plugin's unit tests
(cd packages/mail/plugins/mail/verification && go test ./...)  # its black-box verification suite
(cd packages/stripe && go test ./...)                       # against a fake Stripe and Key Vault
(cd packages/sample-echo-go && go test ./...)                   # builds it; it has no tests
(cd scripts/pkgtool && go test ./...)                           # the scripts' own helper
```

The verification suite builds the plugin from the folder above it; to check a packaged binary
instead, point `MAIL_BIN` at it:

```sh
MAIL_BIN=<unpacked mail zip>/plugins/mail/bin/linux-x64/mail go test -count=1 .
```

The `job-board` plugin is a Python script, and `sample-echo` and `sample-echo-go` are stand-ins, with
no tests of their own; Yawble's own tests exercise the same code from their test data.

## The catalog

`dist/catalog.json` is the schema 1 catalog the yawble.ai Marketplace serves. Every field in it is
read from a package's own manifests (`solution.json`, `plugin.json`) inside its zip, or from the zip
itself, by `scripts/pkgtool` (`catalog.go`); nothing is typed twice. A field a package has nothing
for is an empty list, never left out and never `null`.

- `summary` is the first sentence of the manifest's `description`, and `description` is that text
  as written: plain text, paragraphs separated by blank lines. The build refuses a description
  holding an HTML tag.
- `needs` lists what only a person provides: connections, secrets (by key name, with `when` in
  words for a secret needed only under a setting), documents and person-only settings, and the
  runtimes the plugins `requires`. The build refuses a secret's `when` that names more than one
  setting, a setting the plugin does not declare, or a value that setting does not offer.
- `platforms` is what every plugin with binaries was built for; each file a `platforms` entry names
  must be in the zip.
- `download` is `https://yawble.ai/api/marketplace/download/<id>/<version>.zip`, where the
  Marketplace serves that version once it is uploaded, with the zip's `sha256` and `bytes`.
- `source` is the package's folder on `main` in this repository.

`schema` goes up by one, in `scripts/pkgtool/catalog.go`, on any change a reader must know about: a
field removed or renamed, a field's type or meaning changed, or a field added that a reader must act
on to install correctly. A field a reader may ignore safely does not raise it.

## Add a package

1. Make `packages/<id>/`, where `<id>` is the manifest's `id`: lowercase letters, digits and
   hyphens, at most 48 characters. The build refuses a folder named otherwise.
2. A **solution**: `solution.json` (format 1), `README.md`, `skills/`, `sites/`, `tools/` as the
   team needs, and each plugin's source under `plugins/<plugin-id>/` with a `build.sh` (above). A
   **plugin**: its source, `plugin.json` and `build.sh` at the package's root.
3. Write the manifest's `description` as the Marketplace should show it: plain text, its first
   sentence a summary on its own, paragraphs separated by blank lines, no HTML. Everything the
   catalog says about the package comes from the manifests; there is no other place to type it.
4. Use only made-up people in examples and tests: `person@example.com`, `@example.com`, and other
   reserved names (`example.org`, `example.net`, `*.test`). No real person's name, email address or
   domain.
5. Run `scripts/check.sh`, then look at its zip and its catalog entry in `dist/`. Check a
   solution with Yawble's own check too: `yawble solution check <unpacked zip>`.

To make a new version of a package, raise `version` in its manifest (and in each plugin's
`plugin.json` whose code changed); the zip's name follows.

## Publish

A package is published by uploading its zip, `dist/<id>-<version>.zip`, on the yawble.ai
Marketplace's admin page. Build and check first:

```sh
scripts/check.sh
```

then upload each zip whose version is new. Publishing is a person's step.
