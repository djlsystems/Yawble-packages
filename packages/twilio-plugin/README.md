# Twilio 3.0.0

The `twilio-plugin` plugin: a plugin member that sends SMS and MMS through a person's Twilio
account with a restricted API key, and reads delivery status and replies by polling. It keeps a
consented-recipients list and opt-outs on the team's `twilio` site.

It starts in dry run and sends only to the numbers on `sendAllowlist`, or to consented recipients
a person activated. It never buys numbers and never deletes anything. Its mode, sender, allowlist,
caps, countries, prefix, footer, auto-reply and owner-command settings are the person's.

## What it needs

- **A Twilio connection on the `twilio` slot**: an API-key connection with the Twilio preset (Account
  SID, API key SID and the secret of a Restricted API key), which a person sets in Team settings ->
  Connections and binds to the member's `twilio` slot. Until one is bound, `check` and the poll still
  run and say so on the setup checklist.
- **The team's `twilio` site, with the collections `state`, `messages` and `recipients`.** The
  plugin keeps its poll position, the texts and the consented recipients there. Without the site,
  a command that needs it refuses and says why, and `check` names the site as the next step.

## Events

`plugin.twilio-plugin.sent`, `received`, `optout`, `optin`, `delivery` and `command`. `command`
is an owner command: a text from one of `ownerNumbers` that is only one of `ownerKeywords`. The
skill `plugin-twilio-plugin` lists every command and what each answer means.

## Build and test

```sh
packages/twilio-plugin/build.sh ~/plugins-build/twilio-plugin/3.0.0   # one installable version
(cd packages/twilio-plugin && go test ./...)                         # against a fake Twilio API
```

`scripts/build.sh` puts the package zip in `dist/twilio-plugin-3.0.0.zip`. A solution that hires
this plugin ships it from this same source: its `plugins/twilio-plugin/build.sh` runs this folder's
`build.sh`.

## 3.0.0

- **The Twilio credential is a connection.** The plugin reads the Account SID, API key SID and API
  key secret from the API-key connection (Twilio preset) bound to its `twilio` slot, set in Team
  settings -> Connections, and no longer from its own settings.
- **An update carries existing values over.** `accountSid`, `apiKeySid` and `apiKeySecret` are
  declared moved to the `twilio` slot: installing this version over 2.0.1 makes a Twilio connection
  from each member's own values, assigns it to the member's team and binds it to the slot. A value
  only in the Host's environment is not moved: a person makes the connection and binds it.
- **The slot stays optional**, as the three settings were: `check` and the poll run without it and
  say what to do on the setup checklist.

## 2.0.1

- **Every setting and secret has a short label** on the member's settings screen, such as "From
  number", "Daily send cap", "Account SID" and "API key secret".
- **The Account SID and the API key SID are shown, not masked**: each names an account or a key and
  grants nothing without the key's secret, which stays masked.
- **The three stay optional**: `check` and the poll run before they are set and fill the setup
  checklist on the twilio page, saying which is missing; a required secret would stop every run
  until all three were set.

## 2.0.0

- **The plugin's id is `twilio-plugin`** (it was `twilio`): every plugin's id now ends in `-plugin`,
  so a plugin and a solution never share one. It works exactly as 1.5.0 did, apart from names: its
  event types are `plugin.twilio-plugin.*` (they were `plugin.twilio.*`) and its skill is
  `plugin-twilio-plugin` (it was `plugin-twilio`). A member hired on the old id stays on it until it
  is repointed to `plugin:twilio-plugin`, and its triggers on the old event types must be changed to
  the new ones.
