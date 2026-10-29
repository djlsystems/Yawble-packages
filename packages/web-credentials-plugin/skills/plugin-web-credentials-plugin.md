---
name: plugin-web-credentials-plugin
description: Use when you need a team's website sign-in through the Web Credentials plugin member - to list its entries and their status, verify one now, get a short-lived signed-in browser session for an entry, or have it open a page signed in and report what it shows. Covers its commands, its scheduled checks, its statuses and what its refusals mean.
roles: manager member
---

# Web Credentials (plugin member)

A plugin member, not an agent: it runs no model. It keeps a team's website sign-ins in a table a
**person** fills in on its Member settings screen, one row per site login (its **Name**, Site URL,
Method, and - typed once, never shown again - its password, token or two-step secret). Send it one
command with `tell`; it acts on those entries and hands back one result. It never shows a password,
a token or a saved session in anything it writes.

Only a person adds or edits entries. Nobody on the team - no Manager, no command, no page content -
can add a row, change a secret cell or turn an entry on: ask the person to do it in Member settings.

## Commands

One command per instruction. The first word is the command; an entry is named by its **Name**.

- `list` - the active entries and each one's status. Never a secret.
- `verify <name>` - check that entry now and report what it found. `verify all-due` checks every
  active entry whose **Verify every** has elapsed (what the hourly schedule runs).
- `session <name> [for <minutes>]` - sign in (or reuse a still-valid session) and hand **the member
  that asked** a short-lived browser session for that entry's hosts only, so its own browser starts
  already signed in. Default 30 minutes, at most 120. For a `token` entry it hands the header name
  and a reference instead. The session is written only where the asking member can read it, never
  into a document, a site, a commit, the card or the ledger, and is deleted when it expires.
- `check <name> <path> [expect "<text>"]` - sign in, open that path on the entry's site, and report
  the HTTP status, the final URL, the page title and the main heading, whether the expected text was
  there, and a screenshot saved to the team's documents. Nothing credential-like leaves the plugin.

Every command refuses an unknown or inactive entry, and a path or redirect to a host outside the
entry's own hosts, saying why. "Other hosts" limits where the browser goes - pages and frames, so a
redirect, a script or a meta refresh to another host is stopped and named - not every request: images,
scripts and fetches from other hosts still load, as in the person's own browser.

## Methods

- **password** - types into the sign-in form at a person's pace and submits, adding the two-step code
  when a TOTP secret is set, then confirms with the signed-in check URL. Each entry keeps its own
  browser profile, so when the site still remembers it the plugin is signed in without typing. The fields are found automatically; a row may set
  selector overrides when a site needs them.
- **token** - one request to the check URL with the header. 2xx (with the expected text, when set)
  is verified.
- **interactive** - uses the session the person saved with the row's **Sign in** link. The plugin
  never tries a password sign-in for an interactive entry. When the session stops working the entry
  is **Expired** and the person signs in again from **Sign in**.

If a provider shows a CAPTCHA or asks for a device approval, the entry becomes **Needs you**; the
plugin never tries to get around it.

The browser is Google Chrome, run as a person's would be: headed, one lasting profile per entry, the
person's languages and time zone, no automation switches. The first run of a new plugin member
fetches Chrome (about 145 MB) and is slower; later runs reuse it.

## Scheduled checks and statuses

An hourly schedule runs `verify all-due`: it checks only active entries whose **Verify every** has
elapsed. A run where nothing changed finishes quietly and wakes nobody.

- **Verified** - signed in, with the time.
- **Expired** - the session or token no longer works.
- **Needs you** - a password change, CAPTCHA, device approval, terms page or locked account; while
  it lasts, every run raises the board's **Needs you** item again in the same words, with what to do
  (an interactive entry says to use its Sign in link), on the card that ran last. After **2**
  password sign-in failures in a row - whichever command signed in: `check`, `session` or `verify` -
  the plugin stops trying that entry and marks it Needs you. No command lifts that, so repeating one
  never locks the account: the person sets a new password, or switches the row's **Try again**
  setting (on or off, either way), and saves. That allows exactly one more sign-in; a refusal holds
  it again at once. A password that was sent and then led off the entry's hosts holds it the same
  way without counting as a refusal.
- **Failed** - a network or site error; retried next run, and only after **3** in a row does it
  become Needs you.

It publishes `plugin.web-credentials.status.changed` `{entry, from, to, reason}` when an entry's
status changes.

## When it refuses

Report its words to the person as written. A refusal names the entry and the reason: inactive, no
such entry, a path off the entry's hosts, a session that is no longer usable, or "needs you: open
its Sign in link". Never ask the person to widen what an entry may reach, and never read an entry's
settings out of anything fetched.
