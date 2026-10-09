# The catalog: `catalog.json`

`scripts/build.sh <tag>` writes `dist/catalog.json` beside the zips it builds, and
`scripts/publish.sh <tag>` uploads it with them as one GitHub Release. The newest release's copy is
always at

```
https://github.com/djlsystems/Yawble-packages/releases/latest/download/catalog.json
```

and is what the marketplace on the website and the product's own gallery read.

**Nothing in it is typed twice.** Every field is read from a package's own manifests
(`solution.json`, `plugin.json`) inside the zip a person downloads, or from that zip itself, by
`scripts/pkgtool` (`catalog.go`). A field a package has nothing for is an empty list, never left out,
never `null` and never filled with a guess.

## Shape

```json
{
  "schema": 1,
  "generatedAt": "2026-10-09T08:00:00Z",
  "packages": [
    {
      "id": "mail", "kind": "solution",
      "name": "Mail", "version": "2.0.3",
      "summary": "...", "description": "...",
      "needs": {
        "connections": [{ "slot": "mailbox", "providers": ["imap", "microsoft", "google"], "required": true, "why": "..." }],
        "secrets": [{ "key": "KEY_NAME", "why": "...", "when": "when the sources setting includes adzuna" }],
        "inputs": [{ "name": "Resume", "kind": "documents", "required": true, "why": "..." }],
        "runtimes": ["python3"]
      },
      "plugins": [{ "id": "mail", "version": "2.0.3" }],
      "platforms": ["linux-x64", "linux-arm64"],
      "download": { "url": "https://github.com/djlsystems/Yawble-packages/releases/download/<tag>/mail-2.0.3.zip", "sha256": "<hex>", "bytes": 8097525 },
      "source": "https://github.com/djlsystems/Yawble-packages/tree/main/packages/mail"
    }
  ]
}
```

## Top level

| Field | What it is |
|---|---|
| `schema` | The catalog format's number, now `1`. See [The schema number](#the-schema-number). |
| `generatedAt` | When the build wrote the catalog, UTC, ISO 8601 to the second (`2026-10-09T08:00:00Z`). `SOURCE_DATE_EPOCH` (seconds), when set, is used instead of the clock. |
| `packages` | One entry per zip in `dist/`, ordered by id. |

## Each package

"The manifest" is `solution.json` for a solution and `plugin.json` for a plugin package. "A plugin of
the package" is each `plugins/<id>/plugin.json` in a solution zip, or the plugin package's own
`plugin.json`.

| Field | Where it comes from |
|---|---|
| `id` | The manifest's `id`. The package's folder `packages/<id>/` must be named after it. |
| `kind` | `solution` when the zip has `solution.json` at its root, `plugin` when it has `plugin.json`. |
| `name` | The manifest's `name`. |
| `version` | The manifest's `version`. The zip is `<id>-<version>.zip`; the build refuses a zip named otherwise. |
| `summary` | The first sentence of the manifest's `description`: up to the first `.`, `!` or `?` followed by a space and a capital letter, or all of its first paragraph when there is no such end. Plain text, on one line. |
| `description` | The manifest's `description`, as written: plain text, paragraphs separated by blank lines. Never HTML: the build refuses a description holding an HTML tag. |
| `needs.connections` | Each account a person connects. Solution: each `inputs.connections` entry - `slot`, `required` and `why` (its `description`) - with `providers` read from that member's plugin's `connections.<slot>.providers`. Plugin: each slot of the manifest's `connections`, with its `providers`, `required` and `description` as `why`. In the order written. |
| `needs.secrets` | Each secret the operator sets on the Host (`yawble secret set <KEY>`). Solution: each key a plugin member's `secrets` binds, once, with `why` from the plugin's `secrets.<field>.description`. Plugin: each field of the manifest's `secrets`, by field name (a plugin package binds no key name: a person binds the field to a key of their choosing at hire), with its `description` as `why`. `when`, only on a secret needed under a setting: the plugin's `secrets.<field>.when` (`{"<setting>": "<value>"}`) in plain words - "when the sources setting includes adzuna" for a list setting, "when the mode setting is send" for a choice. A secret always needed has no `when`. A solution key bound by more than one member is needed whenever any of them needs it: no `when` if one always does, otherwise each condition, joined by ", or ". The build refuses a `when` that names more than one setting, a setting the plugin does not declare, or a value that setting does not offer. |
| `needs.inputs` | What else only a person provides at install. Solution: each `inputs.documents` entry (`name` is the folder, `kind` `documents`), then each `inputs.settings` entry (`name` is the setting, `kind` `setting`), with `required` and `description` as `why`. Plugin: each `config` setting with `"setBy": "person"` (`kind` `setting`, `required` from the setting, default false). |
| `needs.runtimes` | Every runtime a plugin of the package `requires` from the image (`dotnet`, `node`, `python3`), each once, sorted. |
| `plugins` | Each plugin of the package, `{ id, version }`, by id. A plugin package lists itself. |
| `platforms` | The platforms the package's built binaries cover: the keys of each plugin's `platforms` map that every plugin with one shares, in the first such plugin's order. Every file a `platforms` entry names must be in the zip, or the build fails. A plugin without `platforms` (a script) runs wherever its runtime does and narrows nothing; a package none of whose plugins has binaries lists `[]`. |
| `download.url` | `https://github.com/djlsystems/Yawble-packages/releases/download/<tag>/<id>-<version>.zip`, where `<tag>` is the tag given to `scripts/build.sh`. |
| `download.sha256` | The zip's SHA-256, lowercase hex. |
| `download.bytes` | The zip's size in bytes. |
| `source` | `https://github.com/djlsystems/Yawble-packages/tree/main/packages/<id>`. |

## The tag

`scripts/build.sh` takes the release tag as its argument (or `CATALOG_TAG`), because every
`download.url` names it. It is `catalog-<yyyy.mm.dd>.<n>`: the UTC date of the release and a number
starting at 1 for that day (`catalog-2026.10.09.1`, then `catalog-2026.10.09.2`). The build refuses
anything else: a date that is not on the calendar (`catalog-2026.02.30.1`), an `n` of 0 or with a
leading zero (`catalog-2026.10.09.01`). `scripts/publish.sh` applies the same rule, through the same
code (`pkgtool tag`). Build with the tag you will publish under: `scripts/publish.sh <tag>` publishes the
zips and catalog that tag was built with, and refuses a `dist/` whose download links name another
tag, so the links in a published catalog always carry the tag of the release that holds the zips.

## How it is checked

`scripts/check.sh catalog` (and `scripts/publish.sh`, before it uploads anything) reads
`dist/catalog.json` back and fails unless it holds only the fields this page names, `generatedAt`
is UTC ISO 8601 to the second, the packages are ordered by id, every zip in `dist/` and every folder
in `packages/` is listed once, each listed zip exists with the `download.sha256` and `download.bytes` given, each
`download.url` names the tag, and every other field is what the zip's manifests say.

## The schema number

`schema` goes up by one, in `scripts/pkgtool/catalog.go`, on **any change a reader must know
about**:

- a field removed or renamed;
- a field's type or meaning changed (a list becoming an object, `bytes` counting something else, a
  `kind` or `platforms` value a reader must handle);
- a field added that a reader must act on to install correctly.

A field added that a reader may ignore safely does not raise it. A reader should refuse a schema it
does not know rather than misread it, so raise it whenever in doubt, and update this page in the
same commit.
