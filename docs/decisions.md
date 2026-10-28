# Decisions

Choices made building the packages in this repository, with the reason, so a later change knows why
things are as they are.

## Web Credentials (`web-credentials`, 1.0.0)

### Language: Node, with Playwright and Chromium inside the plugin's folder

The plugin drives a real browser to sign in to websites, read a signed-in page and save a session.
Playwright's official, maintained library is Node, and Node is one of the runtimes the image
guarantees (`requires: ["node"]`). So the plugin is Node. Everything else it needs lives in its own
folder and nothing is added to the image:

- **playwright-core** (pinned in `package-lock.json`) is installed into the built plugin's
  `node_modules` by its `build.sh` (`npm ci --omit=dev`). The full `playwright` package is not used:
  it downloads browsers on install, which we do ourselves.
- **The Chromium headless shell** is laid into the built plugin's folder by `tools/browser.sh`, at
  the revision `playwright-core` names (`browsers.json`). The shell, not full Chromium, because the
  plugin only ever runs headless. `tools/browser.sh` copies it from a Playwright browser cache
  (`PLAYWRIGHT_BROWSERS_PATH`) when one is there, else downloads it once with `playwright-core`'s own
  installer. The other locales and the hyphenation data are dropped (the plugin uses neither),
  leaving about 220 MB.
- Only `linux-x64` is declared in `platforms`: the bundled shell is the linux64 build. A plugin run
  is always on a worker's Linux host, so this is the only processor that matters here; `linux-arm64`
  is a follow-up (bundle the arm64 shell and declare it).

### Build needs a network or a browser cache, once

`build.sh` runs `npm ci` (the one dependency) and, when no browser cache holds the shell,
`playwright-core install`. Both reach the network on a clean machine; on an instance both are
already present (`node_modules` is restored from the lock, the shell sits under
`PLAYWRIGHT_BROWSERS_PATH`). The built plugin then needs no network at all. Recorded because the
repository's Go plugins build offline, and this one does not on a first, cold build.

### Session handoff: the asking member's own workspace

`session <name>` must hand a short-lived signed-in storage-state to the member that asked, readable
only by that member, never in a document, a site file, a commit, the card or the ledger, and deleted
when it expires. A plugin has no API for "give this file to that member", and every member runs as
the same `agent` user, so file permissions cannot isolate them. Isolation here is by **location**:
the plugin writes the handoff into the requesting member's **own workspace**
(`<that member>/.web-credentials-sessions/<entry>-<random>.json`, mode 0600), which only that member
reads by the team's rule that no member reads another's workspace. The requesting member is the
work message's `source` (`<team>/<member>`); the plugin derives the sibling workspace from its own
`workingDirectory`. The plugin records each handoff's path and expiry in its own workspace state and
deletes it on the first run after it expires (and prunes any already gone). A `token` entry's
handoff holds the header name and the header value in that same file; the command's output names
only the header and the file reference, never the token. The storage-state is pruned to the entry's
hosts before it is written, so a handoff never carries a cookie or origin for any other host.

### No secret is ever written out

Secret cells (password, TOTP secret, token) arrive under `secrets.entries.<row>.<column>`; the saved
interactive session arrives under `secrets.entries.<row>.session`. The plugin keeps every secret
value it is handed - and every cookie and local-storage value inside a saved session - in its own
redaction set and replaces them in every record it writes, longest-first and case-insensitively,
under the platform's own redaction. The workspace state file holds no secret: the lockout
fingerprint that notices an edited row is a salted scrypt digest, and the handoff list holds paths
and times only. Screenshots are page images saved to the team's documents; nothing credential-like
is drawn into them because the page is the site's own signed-in page.

### Packaged as one solution that embeds the plugin

The plugin's id is `web-credentials` (fixed by the spec and by the platform's interactive sign-in
page, which reads this table's columns). The repository's usual shape - a plugin in its own
`packages/<id>-plugin/` package, shipped by a separate solution - would give the plugin and the
solution different ids. Here there is exactly one solution that ships this plugin, and the spec's
deliverable is a single `web-credentials-1.0.0` package folder. So the plugin's source lives
directly under the solution at `packages/web-credentials/plugins/web-credentials/` (its own
`plugin.json` and `build.sh`), and the solution's id is `web-credentials`. Neither `pkgtool` nor the
Host enforces the `-plugin` suffix; it is a repository convention, set aside here for this one
package with the reason recorded.
