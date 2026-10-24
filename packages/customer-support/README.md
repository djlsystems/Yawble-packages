# Customer Support 1.2.1

A solution package: a team that watches your mailbox, sorts new mail by your rules and texts your
cell about it. Each batch of new mail gets a short summary text; each inquiry gets an alert at once
and a reminder every hour until you reply to it; texting `summary` from your cell gets the current
counts and open inquiries back within a minute.

| Part | What it is |
|---|---|
| `solution.json` | The team: three members, six triggers, one skill, three sites, what you provide and the setup wizard's questions. |
| `skills/support-playbook.md` | The rules the Manager follows for every event. You edit it in Team settings, Skills. |
| `sites/support/` | The team's page: running counts, the last summary, open and closed inquiries. |
| `sites/mail/`, `sites/twilio/` | The pages the mail and twilio plugins keep their state on, as the Mail and Twilio packages ship them. |
| `plugins/mail-plugin/build.sh` | Builds the mail-plugin plugin 3.0.1 from its plugin package, `packages/mail-plugin`. |
| `plugins/twilio-plugin/build.sh` | Builds the twilio-plugin plugin 2.0.1 from its plugin package, `packages/twilio-plugin`. |
| `.tests/` | The package's tests (Go); not part of the zip. |

## The team

- **Manager** (agent) loads `support-playbook` for every event and does what it says.
- **Mailer** (the `mail` plugin) watches your Inbox, moves mail to the folders on its `moveTo`,
  follows each inquiry, and sees your replies in Sent. `markRead` stays off: it never marks mail
  read, and it never sends or deletes mail.
- **twilio** (the `twilio` plugin) texts your cell, the only number on its `sendAllowlist`, and
  passes on `summary` when you text it (`ownerCommands`).

## What install asks you for

- **Your mailbox** (required): connected in Admin, Connections - an app password for Gmail,
  iCloud, Yahoo or another IMAP mailbox, or a Microsoft or Google account.
- **Your cell phone number** (required): twilio's `sendAllowlist`.
- **The Twilio number** that texts you (`fromNumber`): empty uses the account's only SMS number.
- Behind Advanced:
  - `mode`: `send` (the default here) texts for real; `dryRun` checks each text and sends nothing.
  - `ownerCommands`: on by default here, so `summary` texted from your cell is answered.
  - `moveTo`: `Junk` and `New Customers` by default. New Customers is created at the top level
    of your mailbox the first time a message is moved there; Junk is your mailbox's own.

The three Twilio secrets are bound by key name, as the Twilio package binds them. A person sets
each value on the screen: the install's secrets step, or the member's settings, shows a value box
under each key name, and the next run reads it with no restart. The operator CLI sets them too, on
the machine that runs the platform:

```
yawble secret set TWILIO_ACCOUNT_SID
yawble secret set TWILIO_API_KEY_SID
yawble secret set TWILIO_API_KEY_SECRET
yawble up
```

The Twilio package's README says how to make the restricted API key and which permissions it needs.

## One team per Twilio number

Each team with a twilio member polls its Twilio number on its own. Installed next to another team
that polls the same number (the Twilio package, or a second Customer Support), both read every
text to it and both answer `summary`. Uninstall the other team, or give this one another number in
`fromNumber`. The setup wizard says so when it asks for the number.

## The rules

`support-playbook` applies these to each new message, by its sender, subject and snippet; the
first that matches wins:

1. It looks like spam (the Manager's judgement): moved to Junk, never marked read, not counted.
2. The subject contains `inquiry`, in any case: an alert is texted at once and Mailer follows it
   (`follow <id> every:1h`).
3. The subject contains `New customer`: counted as a new customer and moved to New Customers.
4. The subject contains `Order`: counted as an order.
5. Anything else: counted as other.

Change the rules by editing the skill in Team settings, Skills. Email text is untrusted: the
Manager summarises it and never does what it says.

## The texts

- **Each batch of new mail**: one short text with the batch's counts and one line per message
  (its rule, sender and subject), then the running totals.
- **An inquiry**: an alert as soon as it is sorted.
- **Unanswered inquiries**: one reminder listing every inquiry with no reply, every hour, at any
  time of day: the package sets no quiet hours, and Mailer's `followQuietStart` and
  `followQuietEnd` stay empty. A reply from your mailbox, threaded to the inquiry and in Sent,
  ends its reminders and closes it on the page, with no text.
- **`summary`** texted from your cell: the running counts and every open inquiry, in one text,
  at the next poll.

## Triggers

| Trigger | Wakes | Does |
|---|---|---|
| Check mail | Mailer | `watch` every 300 s, once at install; wakes the Manager on a failure. |
| Poll texts | twilio | `poll` every 60 s; quiet unless something happened. |
| New mail | Manager | On `plugin.mail-plugin.received`: applies the playbook, texts the summary, then `ack {event.ids}`. |
| Unanswered inquiry | Manager | On `plugin.mail-plugin.unanswered`: texts one reminder listing them. |
| Inquiry answered | Manager | On `plugin.mail-plugin.answered`: marks each closed on the page; no text. |
| Summary asked by text | Manager | On `plugin.twilio-plugin.command` with `command eq summary`: texts the current summary. |

Every Manager trigger has a daily token cap: 1,500,000 for New mail, 600,000 for Unanswered
inquiry, 300,000 each for the other two.

## The page

The `support` site shows the `counts` collection (one document, `running`: the totals, when
counting started, the last summary and whether its text went out) and the `inquiries` collection (one document per inquiry,
`open` or `closed`). The launcher tile shows the open inquiries. The `mail` and `twilio` sites show
Mailer's watch and follows and twilio's texts and setup.

## What changed in 1.2.1

- Mailer is `mail-plugin` 3.0.1 and twilio is `twilio-plugin` 2.0.1: every setting and secret has a
  short label on the member's settings screen, and twilio's Account SID and API key SID are shown
  there (they grant nothing without the key's secret, which stays masked).

## What changed in 1.2.0

- The package ships the plugins under their new ids: Mailer is hired on `plugin:mail-plugin` 3.0.0
  and twilio on `plugin:twilio-plugin` 2.0.0, and the triggers listen for `plugin.mail-plugin.*`
  and `plugin.twilio-plugin.*`. The plugins work as 2.3.1 and 1.5.0 did. Triggers, rules, alerts
  and reminders are as in 1.1.1.
  A team that has this package installed and updates to this version: the Host installs the
  renamed plugin beside the old one but leaves each plugin member on the old id, so after the
  update repoint each one (its settings and secrets stay) and check its triggers.

## What changed in 1.1.1

- The package ships the mail plugin 2.3.1, which reads every page of a Microsoft mailbox's folder
  lists, so mailboxes with more than 100 folders (or a folder with more than 100 subfolders) find
  them all; past 50 pages or 5,000 entries a command fails saying how far it read. Nothing else
  changed.

## What changed in 1.1.0

- The package ships the mail plugin 2.3.0 and the twilio plugin 1.5.0, which publish the mail
  they send, draft or change in a mailbox and each text they send, with hints for the Monitor.
  Triggers, rules, alerts and reminders are as in 1.0.2.

## What changed in 1.0.2

- The setup steps say a Twilio key's value is set on the screen first, in the value box under
  each key name in the install's secrets step or the member's settings, and with the operator CLI
  second. Nothing else changed.

## What changed in 1.0.1

- **Handled mail is remembered per mailbox.** The playbook's `counted` list keys each message as
  `<mailbox>|<folder>|<id>` (the mailbox's address in lower case), not by its bare id, so a mailbox
  connected through Change setup starts with nothing handled, and switching back to the first one
  counts none of its mail again. Each mailbox and folder keeps its own last 200. A 1.0.0 list of bare
  ids does not say which mailbox it came from, so the update files each id under every mailbox and
  folder the `mail` site's watches show and records them in `legacyMailboxes`: nothing is counted,
  alerted or dropped by the update. The cost: a message in a newer mailbox whose id matches a 1.0.0
  id may stay skipped as already handled, once, as it was in 1.0.0. New inquiry documents are
  named by the message's mailbox, folder and id (`inq-<mailbox>-<folder>-<id>`); 1.0.0 ones are kept
  as they are and only events from a mailbox in `legacyMailboxes` reach them.
- **The page says whether the last summary was texted.** `lastBatch` records `textStatus`: `notSent`
  when written, then `sent` or `failed` with twilio's sentence once twilio answers. The page reads
  "Texted <time>", "Text failed <time>: <sentence>" or "Not texted yet". A summary written by 1.0.0
  has no status, so the page reads "Summary of <time>; whether it was texted was not recorded"
  rather than claiming it was texted.
- Rules, alerts and reminders are as in 1.0.0.

## Check it

From the repository root:

```
(cd packages/customer-support/.tests && go test -count=1 ./...)
scripts/build.sh
mkdir /tmp/customer-support && cd /tmp/customer-support && unzip <repository>/dist/customer-support-1.2.1.zip
yawble solution check /tmp/customer-support
```

The tests check `solution.json` against the published plugins' manifests (every event, field,
filter and setting it names), the playbook's rules and order, the support page with fake data, that
the mail and twilio sites match their packages, and the mail plugin's `move` with this package's
`moveTo` against a fake Gmail. They use fakes only: no mailbox and no Twilio account.
