# Twilio SMS/MMS 1.4.0

A team that texts through your Twilio account. The **Texting Assistant** guides you through
Twilio setup, writes texts for you and sends them through the **twilio** plugin member, and tells
you about replies, opt-outs and failed texts.

## Install asks for one thing, and offers a second

Your **cell phone number** (10 US digits or `+15551234567`). It goes on `sendAllowlist`, the only
number twilio may text until you add others. Everything else starts at a safe default: **dry run**
(nothing is sent), your cell only, US only, the sender found from your Twilio account, a poll every
5 minutes. Turning on real sending is one change: `mode` to `send` on the solution's panel.

It also offers **messagePrefix**: your business name as it starts each text (`Acme Plumbing: `),
the name your campaign registered. It can be left empty and set later; `check` reminds you.

The member's settings show the everyday ones first: `mode`, `sendAllowlist`, `fromNumber`,
`messagePrefix`, the three auto-reply settings and the three owner command settings. Every other setting is under **Advanced settings**, and its default is safe to
leave. Each setting's explanation is behind its info icon.

With an A2P 10DLC campaign approved, set `messagingServiceSid` (Advanced) to the Messaging Service
the campaign is registered on, so `registration` reads the campaign and texts go through it.

## What every text carries

The plugin adds what carriers expect of every text, so no message can leave it out:
`messagePrefix` in front (unless the text already starts with that name) and
`Reply STOP to opt out.` at the end (`optOutFooter`: `every` by default, or `first`, or `off`), to
every number, yours included. They count towards the text's length and segments, and `check` has a
`compliance` step saying what every text carries, the characters it adds and the privacy and terms
pages you registered (`privacyUrl`, `termsUrl`).

There are no quiet hours: a text goes when it is sent. A queued text waits at most 4 hours, `list`
shows 20 texts, and the poll re-checks sent texts for 72 hours; none of these is a setting.

## Auto-reply

twilio can answer new texts by itself, with a fixed text you set, at the poll that reads them: no
agent run and no tokens, and nothing in a text changes what is sent. Three settings, on the
solution's panel:

- `autoReply` (off): turns it on. Off, twilio sends nothing by itself.
- `autoReplyText` (empty, at most 300 characters): the reply. On with an empty text, nothing is
  sent and `check` shows it as a `Next:` step.
- `autoReplyOncePerHours` (24, from 1 to 720): at most one auto-reply to a number in this many
  hours. The time of each number's last auto-reply is kept on the twilio site, so a restart does
  not reset it.

An auto-reply goes through every check a text has: in dry run it is recorded and not sent; it goes
only to `sendAllowlist` numbers and active consented recipients, never to an opted-out number; the
countries, the caps and the 1600-character limit apply; `messagePrefix` and the opt-out line are
added. A STOP, START, HELP or other keyword text is never answered: Twilio answers those itself. A
text not answered is counted in one progress line per poll, by reason, without numbers or text.

The twilio page shows each auto-reply exactly as sent, with an `auto-reply` tag beside it. The
Texting Assistant still tells you about the texts and never sends a second reply to one twilio
answered.

## Owner commands

You can text a keyword such as `summary` from your own phone, and the Texting Assistant answers
with a short text back to you, within one poll. Your text is never an instruction to an agent:
twilio matches the keyword itself and publishes `plugin.twilio.command` naming only the keyword,
the number it came from, when, and the text's SID. Three settings, on the solution's panel:

- `ownerCommands` (off): turns it on. Off, every text is an ordinary received text.
- `ownerNumbers` (empty, up to 20 numbers): the numbers that may send commands. Empty means the
  numbers on `sendAllowlist`.
- `ownerKeywords` (`summary, status, updates`): the keywords, separated by commas; at most 20,
  each 1 to 32 letters, digits and spaces. Any other value is refused as a settings problem.

A text is a command when it comes from an owner number and its whole body, trimmed, in any case,
with a trailing `?`, `!` or `.` removed, is one of the keywords: `Summary?` is the command
`summary`; `summary please`, or `summary` from any other number, is an ordinary received text.
STOP, START, HELP and the other opt-out, opt-in and help keywords keep their meaning and are never
commands, even when listed. A command is not also published as a received text, and auto-reply
never answers it.

The **Owner command** trigger sends each command to the Texting Assistant with the instruction
"The owner asked for {event.command}. Answer with a short text to {event.from}." It fires only
while `ownerCommands` is on, since nothing publishes the event otherwise. A poll whose only news
is commands finishes quietly; the event wakes its trigger.

A text's sender can be spoofed. A spoofed command can at worst make the team text the owner's own
number with an answer meant for the owner.

## Set up: talk to the Texting Assistant

Say "set up Twilio" (or "what's next"). It goes one step at a time, links to the right Twilio
Console page and runs `check` after each step. The only manual step is the three secrets, run on
the machine that runs the platform (each command asks for the value; never paste one into chat, a
page or settings):

```
yawble secret set TWILIO_ACCOUNT_SID      # value: <AC... Account SID>   from https://console.twilio.com/
yawble secret set TWILIO_API_KEY_SID      # value: <SK... key SID>       from a Restricted API key:
yawble secret set TWILIO_API_KEY_SECRET   # value: <the key's secret>    https://console.twilio.com/us1/account/keys-credentials/api-keys
yawble up
```

Twilio has two Consoles. The links above are the legacy Console's. In the new Twilio Console:

- the Account SID is on the home page: https://1console.twilio.com/
- API keys are under Settings > Account settings > API keys & auth tokens:
  https://1console.twilio.com/go?to=/account/__account__/settings/us1/api-keys/list

### The API key's permissions

Press **Create API key**, choose key type **Restricted**, and tick only these:

- Messaging: `/twilio/messaging/messages/create`, `/twilio/messaging/messages/read`,
  `/twilio/messaging/messages/list`, `/twilio/messaging/services/read`, and the
  `services.usa2p-campaign` read and list permissions (only needed for 10DLC);
- Phone numbers: `/twilio/phone-numbers/active-numbers/read` and `.../list`;
- IAM: `/twilio/iam/accounts/read`;
- toll-free verification read, if the list offers it.

The key's secret is shown only once, when the key is created; if it is lost, make a new key.

## The twilio page

**Open** on the solution's tile shows the setup checklist (Done / Next / Waiting, with Console
links), recent texts, and the consented-recipients list where only you add and activate people.
The first poll runs at install, so the checklist fills straight away. Until the three secrets are
set, the 5-minute poll answers "Poll skipped: Twilio is not set up …" quietly and wakes nobody.

## The last setup step: the live test

With your real Twilio account, after setup: `check` shows every step done; one real SMS reaches
your cell and the next poll reports it `delivered`; your reply appears as a received text; and
`STOP` from your phone is reported as an opt-out (text `START` afterwards). The Texting Assistant
walks you through it.
