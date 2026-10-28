# Web Credentials

Keep a team's website sign-ins in one place, and know when a login needs you. The **Web
Credentials** plugin member holds a table of site logins a person fills in - `password` (with an
optional two-step code), `token`, or an `interactive` sign-in the person does once in a browser the
platform runs. It checks each active entry on its own schedule and shows one on the board under
**Needs you** when its sign-in stops working. A team member can ask it for a short-lived signed-in
browser session for one entry, or have it open a page signed in and report what it shows with a
screenshot. No password, token or saved session is ever shown, returned, logged or written to a
document.

This is separate from **Admin → Connections**, which stays for the OAuth accounts the platform acts
on; Web Credentials is for signing in to ordinary websites to check your own work.

## What is in the package

- `solution.json` - a small team (a Manager and the plugin member **Credentials**) with an hourly
  check. Installing it makes a ready-to-use team.
- `plugins/web-credentials/` - the plugin itself (built into the zip). This is what you install into
  a team you already have.
- `skills/web-credentials-playbook.md` - the team playbook for Managers and testers.
- The plugin's own skill, `plugin-web-credentials`, ships inside the plugin.

The package ships **no entries, URLs or values**. A person adds entries in Member settings after the
plugin is hired.

## Add it to a team you already have (the main use)

The person's goal is usually to put this plugin on a team they already run (for example an
engineering team whose tester signs in to production). The package format installs a **new** team;
it cannot, on its own, hire a plugin into a pre-existing team or add a trigger there (see
[Gaps](#gaps)). So for an existing team, install the plugin and hire it, in these steps:

1. **Install the plugin for the instance.** Build the package (`scripts/build.sh`), or get it from
   the Marketplace, so you have the folder `web-credentials-1.0.0/`. Open **Admin → Plugins →
   Install from a folder** and choose `web-credentials-1.0.0/plugins/web-credentials` (the folder
   holding `plugin.json`). This installs the plugin once for the instance; it hires nothing yet.
2. **Hire it onto your team.** On that team, **Add member → Web Credentials**. The member appears
   with an empty **Entries** table. No settings are required at hire.
3. **Add the hourly check.** On the team's **Triggers** dialog, add a **schedule** trigger on the
   new member, every 3600 seconds (1 hour), instruction `verify all-due`, waking the Manager **only
   if it hands back or fails**. (This is the same trigger the solution's team gets automatically.)
4. **Add your sign-ins.** Open the member's **Settings** and add one entry per site login. For an
   `interactive` entry, use its **Sign in** link and sign in once in the browser the platform opens.
   Set **Languages** and **Time zone** to your own browser's, so the plugin's browser presents the
   same to a site (an entry you signed in to with **Sign in** takes them from that browser instead).

## Or install the whole solution

Put `web-credentials-1.0.0/` in your Documents and install it from **Marketplace → Advanced →
Install from a folder** (or **Upload a package (.zip)**). It makes a **Web Credentials** team with
the plugin member and the hourly check already set up. Then add your entries in Member settings as
in step 4 above. Nothing is asked for at install.

## Commands

Sent to the member with one command per instruction (the team's Manager does this; the playbook has
the detail):

- `list` - the active entries and each one's status.
- `verify <name>` / `verify all-due` - check one, or every entry that is due now.
- `session <name> [for <minutes>]` - hand the asking member a short-lived signed-in browser session
  for that entry's hosts (default 30 minutes, at most 120); a `token` entry hands the header and a
  reference instead.
- `check <name> <path> [expect "<text>"]` - open that path on the entry's site signed in and report
  the status, final URL, title, heading, whether the text was found, and a screenshot in the team's
  documents.

## Methods and statuses

`password` fills the sign-in form (adding the two-step code when a TOTP secret is set); `token`
sends a header to the check URL; `interactive` uses the session saved with **Sign in** and never
tries a password. Statuses are **Verified**, **Expired**, **Needs you** (a password change, CAPTCHA,
device approval, terms page or locked account - the plugin never tries to get around it) and
**Failed** (retried next run). Every command that signs in with a password - `check` and `session` as
well as `verify` - confirms the sign-in took and counts a refusal against the lockout guard. A password
entry refused twice in a row is held (Needs you): no command, from anyone, submits its password again
until the person edits the row (such as a new password) or turns it off and on. A redirect to a host
that is not one of the entry's is stopped inside the browser, before the other host is asked anything.

## Gaps

- **Hiring into an existing team is manual.** The solution install makes a new team, and an update
  only updates a team the package itself created. There is no package action that adds this plugin
  as a member of a team you already have or adds its trigger there, so that is the four manual steps
  above. This is a limit of the package format, recorded rather than worked around.
- **The first run downloads Google Chrome (~145 MB).** The browser is not in the zip: the plugin
  fetches Google's own stable Chrome into its folder on first use (checksum-checked) and keeps it
  current, unless the platform's copy (`/data/google-chrome`) is already current. A machine that
  cannot reach `dl.google.com` cannot run a sign-in until one copy is there. See `docs/decisions.md`.
- **linux-x64 only.** Google publishes Chrome for Linux on x86-64 only, so the plugin declares only
  `linux-x64`.

## Build and check

```sh
scripts/build.sh                 # builds every package, this one included, into dist/
scripts/check.sh                 # build, tests, catalog and the real-person scan
```

The plugin's own unit and integration tests (Node) run from the plugin folder:

```sh
cd packages/web-credentials/plugins/web-credentials
npm ci
npm test     # fetches Google Chrome once into $TMPDIR/web-credentials-chrome (WEB_CREDENTIALS_CHROME_ROOT)
```

The tests run against a local fake site (a password form, a two-step step, a token API, an expiring
session, a CAPTCHA page, a redirect to another host, a password it keeps rejecting, and a probe page
that reports what a site sees of the browser: user agent, client hints and their headers,
`navigator.webdriver`, plugins, languages, WebGL, screen, viewport, time zone and the per-key timing
of the typing); no real account is used.

**How the browser looks to a site.** Google Chrome stable, headed on its own virtual screen, with
Chrome's own sandbox on, none of Playwright's automation switches, one lasting profile per entry, the
person's languages and time zone (from the sign-in page's saved session, else the plugin's
**Languages** and **Time zone** settings), and typing and clicking at a person's pace.
