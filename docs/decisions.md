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

### The lockout guard covers every sign-in; host limits are enforced inside the browser

(Tester Ingrid's verification of 7a50635.)

- **Every password sign-in is confirmed and counted.** A sign-in is done only when the check page,
  opened after the form (and the two-step code), shows a signed-in user. `verify`, `session` and
  `check` all sign in through the same function and fold its outcome through the same guard: a
  refusal counts, the second in a row (the first after the person's change, P13) holds the entry (Needs you, a status event, the board notice),
  and a held entry submits nothing again. A member's command never lifts the guard - an asked-for
  `verify` used to, which let any member lock the account by repeating it. Only the person lifts it,
  by changing the row (the salted fingerprint changes; see the recheck below for Try again). `check` and `session`
  say plainly when the sign-in failed and never report the sign-in page as the page asked for.
- **Off-host loads never leave the browser.** Each run's page has Chrome's `Fetch` domain paused on
  every document load, every hop of a redirect included; one to a host not in the row is failed
  there (`BlockedByClient`) and named in the answer. Playwright's `route()` is not used: it sees only
  the first URL of a redirect. Only document loads (pages and frames) are held to the row's hosts:
  images, scripts and styles from a CDN still load, since real sign-in pages need them and they are
  not where a redirect takes the browser.
- **The session a run holds is redacted, whatever the method.** When the browser closes, its storage
  state joins the run's redaction set part by part, by the same rule as the platform's saved session
  (Yawble docs/plugins.md, "Redacted part by part"; `SignInSessionValue.SecretParts` and
  `ValueRedactor`, Tobias's D13 fix 4f9b75e). Each WHOLE cookie value and each WHOLE local-storage
  value joins on its own, with its URL-decoded form (`+` as a space):
  - **8 characters or more:** redacted wherever it appears, in any case, inside other words too. So is
    each string of 8 characters or more inside a local-storage value that is JSON; shorter strings
    inside JSON are left out.
  - **4-7 characters:** redacted only as a WHOLE TOKEN, in any case: where no letter or digit (any
    script) is right before or after it. `flag=code123;`, `mid k9x4z` and `"k9x4z"` are redacted;
    `trueness` and `code1234x` are left as they are. An escape right before it (`\uXXXX`, as JSON
    writes a quote `\u0022`; `\n`, `\r`, `\t`, `\b`, `\f`; `%XX`, as `%22` or `%3D`) counts as no
    letter, so an escaped copy is still caught; an escape after it starts with `\` or `%`, which is no
    letter already. Ingrid's case: a value printed out of JSON or a URL is not left in the clear.
  - **1-3 characters** (`1`, `ok`): left out, so they do not blank ordinary text (D11).
  - **Never redacted:** a value, or a string inside JSON, that is one of `true`, `false`, `null`,
    `none`, `undefined`, `yes`, `no`, `on`, `off`, `dark`, `light`, `auto`, `default`, ignoring case,
    whatever its length (`undefined` is 9). These are settings, not secrets (D13: consent=true had
    turned "trueness" into "[redacted]ness").

  Like the platform's set, every value is also matched as a JSON writer prints it: .NET's default
  encoder (`+ < > & ' "` and non-ASCII as `\uXXXX`), the relaxed one (`\"`, `\\`), and that with
  `/` as `\/`; the longest spelling first, one redacted anywhere before a whole token of the same
  length. Secret cells the run is handed (a password, a token) are not sessions: they stay redacted
  wherever they appear, at 4 or more characters. This applies to every session the plugin holds - one
  it signed in for with a password, one from a token entry's page, and an interactive row's saved
  session.
- **The documents folder is the platform's.** A check's screenshot goes to `<team documents>/Web
  Credentials/`, the team documents taken from the request's `siteFiles` (`<documents>/<team>/sites/
  <site>/files`) or the workspace's place (`<data>/teams/<team>/workspaces/<name>` beside
  `<data>/documents/<team>`), never a fixed `/data/documents`. With neither, it stays in the plugin's
  workspace. The tests run in a temporary data root laid out the same way.
- **The first-run Chrome download and the run's clock.** `timeoutSeconds` is the platform's idle
  clock (no `progress` for that long ends the run), not a limit on the whole run. The download stays
  inside the run: a run cannot start work that outlives it, and fetching at build or install time
  would put the browser back in the package. So the fetch and the unpack report progress every 15
  seconds, the unpack no longer blocks the event loop, and `timeoutSeconds` is raised from 120 to
  300 (the platform's default). The next run carries on: a half-done download is discarded and
  refetched. (Corrected at the recheck below: progress is said only while bytes arrive.)

### Recheck of 9e3b559: the board notice, the way out of the guard, late navigations, the download

(Tester Ingrid's recheck of 9e3b559, P7-P12.)

- **The board's Needs you item is raised on every run while an entry needs the person (P8, P12).**
  The platform clears a member's pending notice when its NEXT run starts (Yawble `MemberRuntime.
  RunOneAsync` at da422a2 sets `_needsDecision` to null with the blocked and failed marks: new work
  makes the last run's marks a lie). A plugin's run is a member run, so the hourly `verify all-due`,
  a `list` or any check cleared a notice the plugin had raised once. So every run, quiet ones included,
  raises one `needsDecision` naming every active entry that is Expired or Needs you, and none when no
  entry is. Its words are built only from each entry's status and reason, which hold no count or
  time (the three-failures sentence says "its last 3 checks or more failed", not a running count), so
  it reads the same each run and only a new condition changes it; a repeat publishes no status event.
  The item moves to the card that ran last, which is accepted. An earlier version said it "stays on
  the card that found it"; it did not. A platform rule "keep a plugin's notice until it withdraws it"
  would let the plugin raise it once again; that is out of scope here.
- **The person's way out of a held entry is a change to the row, made with a save alone (P7).** "Turn
  it off and on" worked only when a run saw the row while it was off: two saves with no run between
  leave the row as it was, so the plugin cannot see them, and the platform tells a plugin nothing about
  a save. That route is gone, and the row has a person-only **Try again** setting (`retry`, bool).
  Switching it on or off and saving changes what the person wrote, so the salted fingerprint changes
  and the next run may sign in exactly once more (P13, below: a refusal holds it again at once). A new
  password does the same. A
  member cannot write a plugin's table, so no member command can lift the guard. The Needs-you text
  names only these two ways.
- **A password that was sent and then led off the hosts holds the entry without counting (P10).** The
  check page (or the sign-in's answer) leaving the entry's hosts after a submit says nothing about the
  password, so it is not a refusal and the text names the off-host redirect. But the password is not
  sent again into it every hour either: the entry is held until the row changes, as for a refusal. A
  CAPTCHA shown after the submit holds it the same way.
- **A navigation stopped after the page loaded is reported as stopped (P9).** The host limit is on
  where the main frame goes; Chrome's `Fetch` pause tells a main-frame load from a frame's by its
  `frameId`. `check` looks again after the page settles (`networkidle`): a meta refresh or a later
  script that tried to leave is reported as "loaded, then tried to go to <host> ... the browser stopped
  it", never as the page (which by then is Chrome's error page). An off-host frame inside a page is
  blocked and named on its own line; the page is reported. The interactive check page is looked at
  again the same way.
- **The host limit is on navigation, not a network allow-list.** Images, scripts, styles, `fetch` and
  beacons from other hosts still load, as in the person's own browser; a page on the entry's hosts can
  read its own non-HttpOnly cookies and local storage and send them anywhere, as it could there. The
  README and both skills say so.
- **The first-run download: no overall cap, a no-byte limit (P11).** The 15-second heartbeat used to
  run whatever the download did, so a stalled connection never tripped the idle clock and was ended
  only by a 15-minute cap on the whole download - which a link under about 160 KB/s could never beat.
  Now progress is said only when bytes have arrived (at most every 15 s), the whole-download cap is
  gone, and a download that receives no byte for 60 seconds is ended there, its partial file discarded
  (no resume: a slow but moving link simply finishes, so a Range resume buys little). The unpack,
  local work of a minute or so, keeps a plain heartbeat. `timeoutSeconds` stays 300.

### Recheck of 33a5756: one more password per change, the Try again words, the blocked host's name

(Tester Ingrid's recheck of 33a5756, P13-P16.)

- **Any lift allows exactly one more password (P13).** A lift used to reset the refusal count to 0,
  so each Try-again save or new password let the plugin send the password twice more - on a site that
  locks at 3, one Try again after the first two locked the account. Now a change to a held row (Try
  again switched, a new password, any other edit) sets the count one short of the limit: one more
  sign-in is sent, and if the site refuses it the entry is held again at once. A sign-in the site
  takes resets the count to 0. The refusal words carry no count ("the site refused the password, so
  the plugin has stopped trying"), since after a change one refusal is enough.
- **The words match the control (P14).** In the row editor Try again is a switch, and either
  direction lifts the guard, so every held text says "switch its Try again setting (on or off, either
  way) and save", never "tick its box". It sits right after **Active** in the column order (it was
  column 19 of 25, off screen at 1280 px). A hold that is not about the password - the site took it
  and then left the entry's hosts, or showed a CAPTCHA - no longer says "Set a new password if it
  changed". The Other hosts column's description says where the browser may go (pages and frames).
- **The blocked host is named (P15).** A script redirect (`location.href`) to another host lands on
  Chrome's own error page, whose host is `chromewebdata`; `check` names the host the guard stopped
  (`guard.blocked[0]`) first, as the meta-refresh case already did.
- **The plugin's redaction is the platform's ValueRedactor (P16).** Each session value's URL-decoded
  form joins the set (a run of `%XX` that is not UTF-8 is left as it is), and an escape (`\uXXXX`,
  `\n`-style, `%XX`) next to a 4-7 character value is a token boundary; see "The session a run holds
  is redacted" above. So `enc=q7%2Fwz9` printed decoded as `q7/wz9`, and `?a=1%26mid%3Dk9x4z%26`,
  are redacted.

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
redaction set and replaces them in every record it writes, longest-first and case-insensitively (a
session value of 4-7 characters only as a whole token, as above), under the platform's own redaction. The workspace state file holds no secret: the lockout
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
