# Decisions

Choices made building the packages in this repository, with the reason, so a later change knows why
things are as they are.

## Web Credentials (`web-credentials`, 1.0.0)

### Language: Node, with Playwright inside the plugin's folder

The plugin drives a real browser to sign in to websites, read a signed-in page and save a session.
Playwright's official, maintained library is Node, and Node is one of the runtimes the image
guarantees (`requires: ["node"]`). So the plugin is Node. **playwright-core** (pinned in
`package-lock.json`) is installed into the built plugin's `node_modules` by its `build.sh`
(`npm ci --omit=dev`); the full `playwright` package is not used, since it downloads test browsers
on install. `build.sh` needs the network once for `npm ci` on a cold machine; the built plugin does
not.

### The browser: headed Google Chrome stable, looking like the person's own

A site must not be able to tell the plugin's browser from the person on their own Chrome. So:

- **Build.** Google Chrome **stable**, Google's own `google-chrome-stable` .deb - not Chrome for
  Testing, not Playwright's Chromium, not `chrome-headless-shell`. It is the same build, from the same
  index and with the same layout, as the platform's interactive sign-in page
  (`scripts/install-google-chrome.sh` in Yawble, card 21813), so a session the person saved there is
  replayed by the same browser. Checked here (Oct 2026, 156.0.8078.73): it runs as `agent` with
  nothing added to the image - Xvfb is in the image, `dpkg-deb` is, and every library `chrome` links
  is present (`ldd` finds them all) - with **Chrome's own sandbox on**: the setuid helper
  (`chrome-sandbox`) is deleted, and Chrome uses its unprivileged user-namespace sandbox. A test
  proves its children run in their own PID namespace under their own seccomp filter.
- **Where it is fetched and stored.** `src/chrome.mjs`. Google's apt index
  (`https://dl.google.com/linux/chrome/deb/dists/stable/main/binary-amd64/Packages`) names the newest
  stable version, its .deb and its SHA-256; the download must match, and `dpkg-deb -x` unpacks it
  (no root) into the plugin's own folder, `<plugin member workspace>/.web-credentials/chrome/<version>/`.
  When the platform's copy (`$HARNESS_SIGN_IN_CHROME`, default `/data/google-chrome/<version>/`,
  readable by agents) is already that version or newer, it is used and nothing is fetched.
- **Kept current.** No version is pinned in the package, on purpose: the user agent must stay what a
  real, up-to-date Chrome says. At most once a day a run reads the index (one small request); a newer
  stable is installed beside the old one and the newest two of the plugin's own are kept, so a row
  whose saved session names the previous version still runs it until it rolls off. A run with no
  network keeps what it has. A row whose session names a version no longer kept runs a copy of the
  same major if there is one, else the newest - as a person's own Chrome would have updated - and the
  row's kept identity then says the newer version.
- **Why not bundle it.** The unpacked build is ~450 MB (the old package already exceeded the
  Marketplace's 100 MB with only the headless shell), and a bundled browser would freeze the user
  agent at build time. Fetching at first use keeps the package small and the browser current. The
  cost: the first run of a new plugin member downloads ~145 MB (it reports progress as it goes).
- **Run headed.** Each launch starts its own Xvfb (`-displayfd`, `-nolisten tcp`, a fresh
  MIT-MAGIC-COOKIE in a 0600 file in the plugin's folder), so no other process can watch the screen.
- **No automation tells.** `ignoreDefaultArgs: true`: none of Playwright's default switches
  (`--enable-automation`, `--headless`, `--no-sandbox`, `--disable-extensions`, the field-trial and
  background-networking switches...). Chrome gets only `--remote-debugging-pipe`, the profile, no
  first-run/default-browser prompts, `--disable-blink-features=AutomationControlled` (a DevTools pipe
  otherwise makes `navigator.webdriver` true), `--test-type` (no warning bar for that switch),
  `--use-angle=gl --ignore-gpu-blocklist` (WebGL through Mesa, not SwiftShader), `--lang`, a window
  size and position - the same set as the platform's sign-in page. No viewport is emulated
  (`viewport: null`): the window has its tabs and address bar, and the page's viewport is what is
  left, as on a desktop.
- **Same identity everywhere.** Languages, time zone and screen are the browser's own, never an
  emulation over it: languages go in the profile's Chrome preference (`intl.accept_languages`, so
  `navigator.languages` and `Accept-Language` agree) and `--lang`; the time zone is `TZ`; the screen
  is the Xvfb screen. The user agent and client hints are not overridden at all - they are the real
  Chrome's, so `User-Agent`, `navigator.userAgent`, `userAgentData` and every `Sec-CH-UA*` header
  agree by construction.
- **Paced like a person.** `src/human.mjs`: the mouse moves in steps to a point inside an element
  before it presses; each key is typed on its own with a 55-190 ms jittered gap (now and then
  longer); short pauses between steps. Never `fill`. A test reads the per-key times the page saw.

### One persistent profile per member and row, and the identity handoff

- **Profile.** `<plugin member workspace>/.web-credentials/profiles/<key>/` (0700), `<key>` the first
  32 hex digits of SHA-256 over the member id and the row's key: one per member+row, reused on every
  run with `launchPersistentContext`, so a site sees the same browser returning (cookies, local
  storage, cache). A row that is deleted loses its profile at the next run. Beside it,
  `<key>.json` keeps the row's identity and salted scrypt digests of the session last applied and of
  what the person wrote - never a value.
- **Identity handoff from the sign-in page.** The row's `session` secret (written only by the
  platform's sign-in page) is the v2 envelope
  `{"v":2,"storageState":{...},"identity":{browser, version, userAgent, userAgentMetadata, locale,
  languages, timezoneId, screen, viewport, deviceScaleFactor}}`; a bare storage state (v1) is still
  read. When an envelope is present the plugin runs the same Chrome version (else same major, else
  the newest), with the envelope's languages, time zone, screen, viewport and scale, and applies its
  storage state to the row's profile (`context.setStorageState`) - once per new session: while the
  person has not signed in again, the profile carries its own, newer cookies forward. With no
  session, the identity comes from the plugin's `languages` and `timeZone` settings (else en-US and
  the machine's zone) on a 1920x1080 screen, and is kept for the row afterwards.
- **Password rows.** The profile keeps the site's cookies, so a run first opens the check page and
  only types the password when the browser is no longer signed in - fewer sign-ins, as a person
  returning to a site. An edit to the row clears the profile's cookies so the new values are tried.
- CAPTCHA and device approvals still stop and go to the person (Needs you); the lockout guard and the
  host limits are unchanged.

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
