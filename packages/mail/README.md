# Mail 2.4.1

A team that works with your mailbox: a Manager you talk to, and a plugin member, Mailer, that lists,
searches and reads your mail, files it when you allow that, watches your Inbox for new mail, and
drafts or sends email: HTML made from the text by default, or plain text with `format: text`. A reply
is threaded to the message it answers, so your mail program shows it in that conversation. Mailer
tells whether you replied to a message, and follows a message you choose, reminding the team until
you reply.

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
  unless you fill it. One the mailbox does not have yet is created at the top level the first time
  Mailer moves or labels mail to it. Only you can set it.
- **Mailer's follow quiet hours** - `followQuietStart` and `followQuietEnd` (HH:MM, as 22:00 and
  07:00; empty for none) in `followTimeZone` (an IANA name such as Europe/London; UTC unless you set
  it). No reminder is listed in them. Only you can set them.

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
later check that finds new mail publishes one `plugin.mail-plugin.received` event listing every new message
- sender, subject, date and a short snippet, never the body - and the team acknowledges each message
once handled. A message nobody acknowledges is listed again by the next 3 checks and then named in
the check's output.

The **Mail** site (Open on the tile) shows where the watch has read up to and what is waiting for an
acknowledgement. Mailer keeps one record per mailbox and folder there, never one per message, and
one more per mailbox of the messages it has shown you, as short fingerprints of their ids, so a
reply answers only a message it listed or read. A followed message has a record of its own (see
below).

## Replied and follow-up reminders

`replied <id>` says whether you replied to a message from this mailbox: yes, with the reply's sent
time and subject, or no. A reply is a message in your Sent folder, sent after the original arrived,
threaded to it: over IMAP its `In-Reply-To` or `References` names the original's Message-ID (Sent is
the folder your server marks `\Sent`); on Google it is a sent message in the same thread; on
Microsoft, a message in Sent Items in the same conversation. The `\Answered` flag is never used.
Mailer cannot see a new message to the sender that is not a reply, or a reply sent from another
mailbox: either reads as not replied.

`follow <id> [every:<hours>h]` follows a message until you reply, and `unfollow <id>` stops. Each
check of the watch looks for the reply. Once you reply, the follow ends and one
`plugin.mail-plugin.answered` event lists the message. Until then, once per interval (1 to 24 hours,
1 by default), one `plugin.mail-plugin.unanswered` event lists every followed message still waiting -
sender, subject, since when and how many hours, never the body. In your quiet hours no reminder is
listed; the first check after lists each one due, once. A followed message you move keeps its
follow. `replied` and `follow` take only a message Mailer listed or read, as `reply` does, and a
mailbox follows at most 50 messages.

The Mail site lists the followed messages: from, subject, waiting since and last reminded.

The install wakes nobody when new mail arrives. To have the Manager told, add an event trigger on
`plugin.mail-plugin.received` for the Manager in the team's Triggers dialog (with a daily token cap); the
same goes for `plugin.mail-plugin.unanswered` and `plugin.mail-plugin.answered`.

## What the Monitor shows

Each change Mailer makes in your mailbox is published as an event, and the Monitor shows it as one
line:

- **sent** (icon `send`, created): one line per email Mailer sent - `send`, or `reply`, in send
  mode - labelled with its subject. The event names the mailbox, the message's id and thread where
  the provider gives them, how many recipients it went to and the subject; never who, never the body
  or an attachment. A draft that was only saved is not sent.
- **drafted** (icon `drafts`, created): one line per draft Mailer saved - `draft`, `send` in draft
  mode, or `reply` in draft mode - labelled with its subject. The event names the mailbox, the
  draft's id and the subject.
- **changed** (icon `mail`, changed): one line per message Mailer marked read or unread, moved or
  labelled, labelled `read: <subject>`, `unread: <subject>`, `moved: <subject>` or
  `labelled: <subject>`. A move or label of many messages publishes many, so the event is marked
  high-volume.

A subject is shown on one line, cut to 120 characters. Reading mail (`list`, `search`, `read`,
`folders`, `watch`, `replied`), a refused command, and `follow` or `unfollow` (which change only
Mailer's own records) show nothing. `plugin.mail-plugin.received`, `unanswered` and `answered` are events
for triggers and have no Monitor line. Mailer never deletes, so nothing is ever shown as deleted.

## What it does and does not do

- `list`, `search`, `read` (text body, links listed, attachments listed but never downloaded),
  `mark-read`/`mark-unread` (with `markRead`), `move`/`label` (to `moveTo` only), `draft`, `send`,
  `reply`, `replied`, `follow`, `unfollow`, `ack`, `folders` and `watch`.
- A `moveTo` folder or label the mailbox does not have is created on the first `move` or `label` to
  it, at the top level and spelled as in `moveTo`: a label on Google (shown in the label list and
  the message list), a mail folder on Microsoft, a folder on IMAP (created and subscribed). The
  answer says so: "Created folder New Customers; moved 3 messages: ...". Only names on `moveTo` are
  ever created. Mailer creates only top-level folders: a nested name (`Clients/Acme`) is used when it
  exists, and you create it in your mail app. Junk, Sent, Trash and the other special-use folders
  are found by their role and never created. A create the server refuses fails the command in the
  server's words and moves nothing. `list`, `search` and `watch` never create a folder.
- `folders` lists the mailbox's folders (labels on Google), each with its special-use role where it
  has one, then marks each `moveTo` entry "exists", "created on first use", or "nested: create it in
  your mail app". It changes nothing.
- `reply` answers a message Mailer listed or read: "Re: " and its subject, to its Reply-To or
  sender (to everyone on it only when you ask), and threaded to it - `In-Reply-To` and `References`
  over IMAP, the same thread on Google, Outlook's own reply on Microsoft. It drafts, or sends in send
  mode; every recipient must still be on `sendAllowlist`. The original is quoted below the reply
  only when you ask.
- No deleting, ever. No forwards, and no attachments.

## What changed in 2.4.1

- Mailer is `mail-plugin` 3.0.1: every one of its settings has a short label on its settings
  screen.

## What changed in 2.4.0

- Mailer is the plugin `mail-plugin` 3.0.0 (it was `mail` 2.3.1), now a plugin package of its own,
  built from `packages/mail-plugin`. It works as 2.3.1 did; its events are
  `plugin.mail-plugin.received`, `unanswered` and `answered`.
  A team that has this package installed and updates to this version: the Host installs the
  renamed plugin beside the old one but leaves each plugin member on the old id, so after the
  update repoint each one (its settings and secrets stay) and check its triggers.

## What changed in 2.3.1

- The mail plugin 2.3.1 reads every page of a Microsoft mailbox's folder lists (and of the other
  lists it reads from Microsoft Graph), so a mailbox with more than 100 folders, or a folder with
  more than 100 subfolders, finds them all for `move`, `label`, `folders`, `list` and the watch. A
  nested path such as `Inbox/Clients/Acme` is found level by level. Mailer reads at most 50 pages
  or 5,000 entries of one list; past that the command fails, saying how far it read, rather than
  going on with part of the list. Gmail and IMAP are as in 2.3.0.

## Try it

1. Connect a Gmail app password in Admin, Connections, and bind it at install.
2. Ask the Manager "what is new in my mail?" - it lists your Inbox.
3. Send yourself a message, wait for the next check, and open the Mail site: the message is waiting
   for an acknowledgement.
4. Set `mode` to `send`, put your own address on `sendAllowlist`, and ask the Manager to send a test
   email to it with subject "TESTING from concierge" and body "Hello world".
