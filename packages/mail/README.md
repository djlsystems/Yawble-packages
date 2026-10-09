# Mail 2.0.2

A team that works with your mailbox: a Manager you talk to, and a plugin member, Mailer, that lists,
searches and reads your mail, files it when you allow that, watches your Inbox for new mail, and
drafts or sends plain-text email.

## What install asks you for

- **A connection for Mailer's `mailbox` slot** (required), made first in Admin, Connections. Any of:
  - **An app password** (the simplest): Gmail, iCloud, Yahoo or any IMAP mailbox. No OAuth client
    is involved; the connection's login test checks it before it is saved.
  - **A Microsoft account**, with `Mail.ReadWrite` and `Mail.Send`.
  - **A Google account**, with `gmail.readonly`, `gmail.compose` and `gmail.modify`.

  The team never sees your password or a token.
- **Mailer's `mode`** - `draft` or `send`. Only you can set it.
- **Mailer's `sendAllowlist`** - the addresses (`person+tag@example.com`) or whole domains
  (`@example.com`) Mailer may write to. Only you can set it.
- **Mailer's `markRead`** - off unless you turn it on. Only you can set it.
- **Mailer's `moveTo`** - the folders and labels Mailer may move mail to or label it with; empty
  unless you fill it. Only you can set it.

No documents are uploaded and no secrets are bound.

## The default mode only drafts

Left at its default, `mode` is `draft`: Mailer saves a draft in your mailbox and you send it
yourself. Nothing is ever sent.

To let it send, set `mode` to `send` on the solution's panel and add each address or domain it may
send to under `sendAllowlist`. In send mode with an empty list, every run is refused. In either mode,
a command with any To, Cc or Bcc recipient that is not on the list is refused whole, and nothing is
sent or drafted. No instruction and no email can change these settings: only you can, on the panel.

## The watch

The install runs Mailer's first check at once, and then every 5 minutes. The first check starts
from now: mail already in your Inbox is not listed (set `watchLookbackDays` to look back). Each
later check that finds new mail publishes one `plugin.mail.received` event listing every new message
- sender, subject, date and a short snippet, never the body - and the team acknowledges each message
once handled. A message nobody acknowledges is listed again by the next 3 checks and then named in
the check's output.

The **Mail** site (Open on the tile) shows where the watch has read up to and what is waiting for an
acknowledgement. Mailer keeps one record per mailbox and folder there, never one per message.

The install wakes nobody when new mail arrives. To have the Manager told, add an event trigger on
`plugin.mail.received` for the Manager in the team's Triggers dialog (with a daily token cap).

## What it does and does not do

- `list`, `search`, `read` (text body, links listed, attachments listed but never downloaded),
  `mark-read`/`mark-unread` (with `markRead`), `move`/`label` (to `moveTo` only), `draft`, `send`,
  `ack` and `watch`.
- No deleting, ever. No replies or forwards with the original quoted, and no attachments.

## Try it

1. Connect a Gmail app password in Admin, Connections, and bind it at install.
2. Ask the Manager "what is new in my mail?" - it lists your Inbox.
3. Send yourself a message, wait for the next check, and open the Mail site: the message is waiting
   for an acknowledgement.
4. Set `mode` to `send`, put your own address on `sendAllowlist`, and ask the Manager to send a test
   email to it with subject "TESTING from concierge" and body "Hello world".
