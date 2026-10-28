---
name: web-credentials-playbook
description: How this team keeps a person's website sign-ins through the Web Credentials plugin member - what a person sets up, the commands the Manager and a tester send it, what its statuses mean, and the rules nobody on the team may bend.
roles: manager member
---

# Web Credentials playbook

This team keeps one team's website sign-ins in the plugin member **Credentials** and checks them on
a schedule. A person fills in the entries; the team reads their status, verifies them, gets a
signed-in browser session for one, or has the plugin open a page signed in.

Load `plugin-web-credentials` before you tell Credentials anything. Send it one command per
instruction.

## What the person sets up (not the team)

In **Member settings** for Credentials, the person adds one row per site login: a **Name** (unique,
such as `yawblelabel-prod-admin`), the **Site URL**, the **Method** (`password`, `token` or
`interactive`), the signed-in check URL, and - typed once, never shown again - the password and
two-step secret, or the token, or (for `interactive`) the session saved with the row's **Sign in**
link. The person also sets **Verify every** (hours) and the **Active** switch.

Nobody on this team adds a row, changes a secret cell or turns an entry on. If an entry is missing
or wrong, ask the person to fix it in Member settings; never ask for the secret value yourself.

## Commands

- `list` - the active entries and each one's status.
- `verify <name>` - check one now. `verify all-due` is what the hourly schedule runs.
- `session <name> [for <minutes>]` - hand the asking member a short-lived signed-in browser session
  for that entry (default 30 minutes, at most 120), for its hosts only. A tester starts its own
  browser from that session to check the live site already signed in. For a `token` entry it hands
  the header and a reference instead. It is readable only by the member that asked and is deleted
  when it expires.
- `check <name> <path> [expect "<text>"]` - the plugin opens that path on the entry's site signed
  in and reports the HTTP status, the final URL, the title, the main heading, whether the expected
  text was there, and a screenshot in the team's documents.

Every command refuses an unknown or inactive entry, and a path or redirect off the entry's hosts.
"Other hosts" limits where the browser goes (pages and frames: a redirect, script or meta refresh is
stopped and named); images, scripts and fetches from other hosts still load, as in the person's own
browser.

## Statuses and the board

- **Verified** - signed in. **Expired** - the session or token no longer works. **Needs you** - a
  password change, CAPTCHA, device approval, terms page or locked account (an interactive entry says
  to use its Sign in link). **Failed** - a network or site error, retried next run.
- While an entry is Expired or Needs you, every run of the plugin member raises the board's **Needs
  you** item again, in the same words, so it stays while the person is needed; it sits on the card
  that ran last. Report it to the person in the plugin's own words.
- A password entry that is refused twice in a row - by `check`, `session` or `verify` - is held
  (Needs you). Re-sending a command does not lift it; the person sets a new password or ticks (or
  unticks) the row's **Try again** box, and saves.

## For a tester checking a deploy

To check production signed in: ask the person to add an `interactive` (or `password`) entry for the
site and sign in once. Then `session <name>` gives you a storage-state your browser can start from,
or `check <name> <path> expect "<text>"` has the plugin do the check and screenshot it. The plugin
never hands you, prints or stores the raw password, token or session.
