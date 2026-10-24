# Mail 3.0.0

The `mail-plugin` plugin: a plugin member that lists, searches, reads, marks, moves and labels
mail, drafts or sends email (HTML made from the text by default, or plain text with
`format: text`), drafts or sends a reply threaded to the message it answers, says whether a message
was replied to, follows a message until it is, and watches a folder for new mail. It works over an
IMAP app-password mailbox, a Microsoft account or a Google account, connected to its `mailbox`
slot in Admin, Connections.

It only drafts by default, sends only to the addresses and domains on `sendAllowlist`, and never
deletes. `mode`, `sendAllowlist`, `markRead`, `moveTo` and the follow quiet hours are the person's
settings. No instruction or email can change them.

## What it needs

- **A connection for the `mailbox` slot**: an app password (Gmail, iCloud, Yahoo or any IMAP
  mailbox), a Microsoft account with `Mail.ReadWrite` and `Mail.Send`, or a Google account with
  `gmail.readonly`, `gmail.compose` and `gmail.modify`.
- **The team's `mail` site, with the collections `watch` and `follows`**, for the watch and for
  follow, and to remember across runs which messages it listed or read. The watch keeps one record
  per mailbox and folder, never one per message. Without the site, the watch and follow refuse and
  say why.

## Events

`plugin.mail-plugin.received` lists the new mail found by each check of the watch, and
`plugin.mail-plugin.unanswered` and `plugin.mail-plugin.answered` cover followed messages.
`sent`, `drafted` and `changed` record each change it makes in the mailbox, and the Monitor shows
them. The skill `plugin-mail-plugin` lists every command and what each answer means.

## Build and test

```sh
packages/mail-plugin/build.sh ~/plugins-build/mail-plugin/3.0.0   # one installable version
(cd packages/mail-plugin && go test ./...)                       # against fake IMAP, Graph and Gmail servers
```

`scripts/build.sh` puts the package zip in `dist/mail-plugin-3.0.0.zip`. A solution that hires
this plugin ships it from this same source: its `plugins/mail-plugin/build.sh` runs this folder's
`build.sh`.

## 3.0.0

- **The plugin's id is `mail-plugin`** (it was `mail`): every plugin's id now ends in `-plugin`, so
  a plugin and a solution never share one. It works exactly as 2.3.1 did, apart from names: its
  event types are `plugin.mail-plugin.*` (they were `plugin.mail.*`) and its skill is
  `plugin-mail-plugin` (it was `plugin-mail`). A member hired on the old id stays on it until it is
  repointed to `plugin:mail-plugin`, and its triggers on the old event types must be changed to the
  new ones.
