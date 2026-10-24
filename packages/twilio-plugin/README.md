# Twilio 2.0.1

The `twilio-plugin` plugin: a plugin member that sends SMS and MMS through a person's Twilio
account with a restricted API key, and reads delivery status and replies by polling. It keeps a
consented-recipients list and opt-outs on the team's `twilio` site.

It starts in dry run and sends only to the numbers on `sendAllowlist`, or to consented recipients
a person activated. It never buys numbers and never deletes anything. Its mode, sender, allowlist,
caps, countries, prefix, footer, auto-reply and owner-command settings are the person's.

## What it needs

- **Three secrets**: `accountSid`, `apiKeySid` and `apiKeySecret`, each bound to a key the person
  sets in the member's settings (or the operator sets with `yawble secret set <KEY>`).
- **The team's `twilio` site, with the collections `state`, `messages` and `recipients`.** The
  plugin keeps its poll position, the texts and the consented recipients there. Without the site,
  a command that needs it refuses and says why, and `check` names the site as the next step.

## Events

`plugin.twilio-plugin.sent`, `received`, `optout`, `optin`, `delivery` and `command`. `command`
is an owner command: a text from one of `ownerNumbers` that is only one of `ownerKeywords`. The
skill `plugin-twilio-plugin` lists every command and what each answer means.

## Build and test

```sh
packages/twilio-plugin/build.sh ~/plugins-build/twilio-plugin/2.0.1   # one installable version
(cd packages/twilio-plugin && go test ./...)                         # against a fake Twilio API
```

`scripts/build.sh` puts the package zip in `dist/twilio-plugin-2.0.1.zip`. A solution that hires
this plugin ships it from this same source: its `plugins/twilio-plugin/build.sh` runs this folder's
`build.sh`.

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
