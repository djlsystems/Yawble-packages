---
name: twilio-setup
description: Use when the person wants to set up Twilio texting, asks what is next or why texting does not work yet, or `twilio check` shows a Next line - the guided setup, one step at a time, with the Twilio Console link for each step, `check` after each, the copy-paste secrets block, and the live test that ends setup.
roles: manager member
---

# Twilio guided setup

You walk the person through setting up Twilio so the team can text. The person does the steps in
the Twilio Console; you tell them exactly where, then run `check` on the plugin member `twilio`
and say what is done and what is next. Setup can take days (registration review), so the person
may come back to it many times: always start from `check`, never from memory.

The commands, output and refusals of `twilio` are in the skill `plugin-twilio-plugin`. Load it too.

## The rules of this conversation

- **One thing at a time.** Ask for, or explain, one step. Wait for the person to say it is done.
  Then run `check` and report.
- **Always a direct link.** Give the Twilio Console page for the step (below). Never say "find it
  in the Console".
- **`check` after every step.** Tell `twilio` the one word `check`. Report its lines as they are,
  then say in one sentence what is done and what is next.
- **Never a secret value.** Never ask for the Account SID's, key SID's or key secret's value, never
  tell anyone to paste one into chat or a page, and never repeat one the person pastes by mistake
  (tell them to make a new key, step 2). The person types the three values into
  an API-key connection with the Twilio preset (Account SID, API key SID, API key secret) in Team settings -> Connections, bound to twilio's `twilio` slot.
- **Settings are the person's.** The person changes `mode`, `fromNumber`, `messagingServiceSid`,
  `sendAllowlist` and every other twilio setting on this solution's panel (Marketplace >
  Installed > Twilio SMS/MMS > Manage). You say which setting and which value when they ask; you never change one
  and never ask anyone to widen one.
- **The page shows the same checklist.** The `twilio` site (Open on the solution's tile) shows the
  last `check` as Done / Next / Waiting with these links. Point the person to it between sessions.

## Reading `check`

`check` prints one line per step, each one of:

- `Done: <step> — <evidence>`
- `Next: <step> — <what to do>`
- `Waiting: <step> — <since when, typical time>`

The steps come in this order: `secrets`, `credentials`, `account type`, `number` (or `buy a
number` / `choose fromNumber`), `capabilities`, `registration` (one or more lines), `mode`,
`allowlist`, and last `first message`. While the Twilio connection is unbound or lacks a field, `check`
stops after `secrets` and `credentials`; the network steps are skipped.

Work on the **first** `Next:` line. A `Waiting:` line needs nothing from the person but time: say
how long it typically takes and that the twilio page shows when it changes. The first poll runs at
install, so the checklist is filled before the first conversation. Until the Twilio connection is bound, each
poll answers `Poll skipped: Twilio is not set up: <field> has no value on this platform; …` - quietly,
waking nobody - and still fills the checklist.

## The steps

Start every session with `check` and pick up at the first step that is not Done.

### 1. Account

The person signs up and verifies their own cell (the one on `sendAllowlist`).

- Sign up: https://www.twilio.com/try-twilio
- Verified numbers (a Trial account can text only these): https://console.twilio.com/us1/develop/phone-numbers/manage/verified

Ask once, and only to explain later steps: does their Console show a dollar credit, or free
messages with template-only bodies? On a template-only trial the free-text test (step 5) must wait
for the upgrade (step 6).

`check` evidence: once the credentials work (step 2), `account type` appears. A Trial account
shows `Next: account type — Trial (status active): … to upgrade, press Upgrade at the top of the Twilio Console; billing overview: <link>`; that is expected
until step 6, so carry on with step 3. A Full one shows `Done: account type — Full, status active`.

### 2. Credentials (the only manual step that cannot be avoided)

The person makes a **Restricted** API key, then puts its three values in an API-key connection with the Twilio preset (Account SID, API key SID, API key secret) in Team settings -> Connections, bound to twilio's `twilio` slot.

1. Open https://console.twilio.com/us1/account/keys-credentials/api-keys and press
   **Create API key**. Key type **Restricted**. Name it `yawble-twilio`. In the new Twilio
   Console the page is Settings > Account settings > API keys & auth tokens:
   https://1console.twilio.com/go?to=/account/__account__/settings/us1/api-keys/list
2. Tick only these permissions:
   - Messaging: `/twilio/messaging/messages/create`, `/twilio/messaging/messages/read`,
     `/twilio/messaging/messages/list`, `/twilio/messaging/services/read`, and the
     `services.usa2p-campaign` read and list permissions (only needed for 10DLC);
   - Phone numbers: `/twilio/phone-numbers/active-numbers/read` and `.../list`;
   - IAM: `/twilio/iam/accounts/read`;
   - toll-free verification read, if the list offers it (otherwise `registration` says the status
     cannot be read with this key and the person checks it in the Console).
3. Twilio shows the key's **SID** (`SK…`) and **secret** once. Keep the page open until the next
   step is done.
4. The Account SID (`AC…`) is on the Console home page: https://console.twilio.com/ or, in the
   new Twilio Console: https://1console.twilio.com/

5. In Team settings -> Connections the person adds an **API key** connection, chooses **Twilio**,
   types the Account SID, the API key SID and the API key secret, saves it, and binds it to the
   twilio member's `twilio` slot. Nothing goes in chat or on a page.

Never use the account's Auth Token: twilio does not accept it.

`check` evidence: `Done: secrets — accountSid set (…1234), apiKeySid set (…5678), apiKeySecret
set` (last 4 characters of the SIDs, never the secret) and `Done:
credentials — the API key signs in to account …1234`. What `check` names, and what to do:

- `` Next: secrets — <fields> missing; no connection is bound to this member's `twilio` slot; … `` - no
  Twilio connection is bound yet: do step 5.
- `` Next: secrets — <fields> missing; a person fills in <fields> in the API-key connection bound to
  the `twilio` slot … `` - the bound connection lacks those fields: fill in only those, in Team
  settings -> Connections.
- `` Twilio rejected the Account SID, API key SID and API key secret of the connection bound to the
  `twilio` slot (401) `` - a value is mistyped or the key was deleted; replace the values in that
  connection in Team settings -> Connections, or make a new key.
- `Twilio refused the request (403): the API key lacks the permission <permission>` - edit the key
  at the link above and tick that permission.

### 3. Number

Recommended: a **toll-free** number (about $2.15/month; verification in about 3–5
business days, no campaign fees). A local 10DLC number is about $1.15/month but needs A2P 10DLC
registration (10–15 days or more, plus brand, vetting and monthly campaign fees).

- Buy a number: https://console.twilio.com/us1/develop/phone-numbers/manage/search (tick SMS, and
  MMS if they want pictures).
- Their numbers: https://console.twilio.com/us1/develop/phone-numbers/manage/incoming

The person does **not** need to set `fromNumber` when the account has exactly one SMS-capable
number: twilio uses it (`Done: number — +1…, toll-free (the account's only SMS-capable number)`).
`check` says:

- `Next: buy a number` - none yet; give the buy link.
- `Next: choose fromNumber — the account has <n> SMS-capable numbers (<list>); …` - the person
  sets `fromNumber` to one of the listed numbers on the panel.

Leave the number's "A message comes in" webhook empty; twilio reads replies by polling.

`check` evidence: `Done: number — …` and `Done: capabilities — sms=true mms=true` (mms=false is
fine for text only; `mms` is then refused).

### 4. Dry run to their own phone

Nothing for the person to do. Tell `twilio`:

```
sms
To: <the person's cell from sendAllowlist>

Hello from the Texting Assistant: a dry run.
```

Evidence: twilio answers `Dry run: would send SMS to <cell> from <sender>, <n> segment(s),
<encoding>; nothing was sent.` Show it to the person: this is how every text looks before real
sending is on. (`check` shows `mode — dryRun: …` and `allowlist — <cell>`; dry runs are not
stored, so `check` has no line for this step.)

### 5. Trial test (optional)

Only on a Trial account, and only if the person wants a real text before upgrading. The person sets
`mode` to `send` on the panel. Warn first: the text arrives prefixed "Sent from a Twilio trial
account", and a new-Console trial may refuse free text (template-only bodies) - then skip to step
6. Send the same `sms` as step 4.

Evidence: twilio answers `Sent SM… to <cell>, status queued, …`; after the next poll `check`
shows `Done: first message delivered to <cell>`. The person may set `mode` back to `dryRun` until
the upgrade.

### 6. Upgrade

The person upgrades in the Console: profile, address, payment method and a starting balance.

- Upgrade: click **Upgrade** at the top of the Console (in the new Twilio Console, the **Trial**
  banner or **Upgrade your account**; or search "Upgrade" in the Console search bar).
- Billing overview (balance and refills, after the upgrade):
  https://console.twilio.com/us1/billing/manage-billing/billing-overview

`check` evidence: `Done: account type — Full, status active`.

### 7. Registration

US carriers block unregistered traffic. Which path:

- **Toll-free number (recommended):** submit toll-free verification from the number itself. It
  needs the upgraded account and a business profile. Open the number in Active numbers, then its
  **Regulatory Information** tab, then **Verify this toll free number**:
  https://console.twilio.com/us1/develop/phone-numbers/manage/incoming
  In the new Twilio Console the status is under Trust Hub, **Registrations** tab:
  https://1console.twilio.com/go?to=/account/__account__/us1/trusthub/compliance-registrations
  `check`: `Waiting: registration — … typically 3–5 business days`.
- **Local 10DLC number:** register a brand (Sole Proprietor if no EIN; Low-Volume Standard with an
  EIN) and a campaign. Campaigns submitted after 2026-06-30 need a public privacy policy URL and
  terms URL - ask where those pages live before starting. Then create a Messaging Service with the
  number in it, and the person sets `messagingServiceSid` (`MG…`) on the panel.
  https://console.twilio.com/us1/develop/sms/regulatory-compliance/a2p-onboarding
  https://console.twilio.com/us1/develop/sms/services
  `check`: `Waiting: registration — … typically 10–15 days`.

Twilio sets these fees and changes them; the person confirms them in the Console before paying.

`check` evidence: `Done: registration — …` showing `TWILIO_APPROVED` (toll-free) or `VERIFIED`
(10DLC). If it says the toll-free verification status cannot be read with this key, the person
checks it at the link above and tells you. Sent and received texts, with their error codes, are in
the Console's Messaging logs: https://console.twilio.com/us1/monitor/logs/sms

### 8. First real message to their own phone

The person sets `mode` to `send` on the panel, if not already. Send the `sms` of step 4 with a real
body. The next poll (within 5 minutes) records its status.

`check` evidence: `Waiting: first message — <sid> to <cell> is <status> since <time>; …` until
the poll sees it, then `Done: first message delivered to <own cell>`. A `Next: first message —
<failure sentence>` is explained with plugin-twilio-plugin's failures.

### 9. Wider sending (optional, later)

Only after registration is approved. The person:

1. publishes how people consent (a form, a sign-up page), with the purpose that matches the
   registered use case;
2. adds each recipient on the twilio page with **Add recipient** - method, purpose, date and
   evidence are required, and the entry starts `pending`;
3. presses **Activate** on each entry;
4. turns on `allowConsentedRecipients` on the panel.

You never add or activate a recipient. `check` evidence: `recipients` counts the active entries;
`allowlist` shows the caps.

## The live test (the last setup steps)

When steps 1–8 are Done, walk the person through this acceptance test, one step at a time:

1. **`check`** - every step 1–8 shows `Done:` (step 5 may be skipped).
2. **One real SMS to their own cell** - send it with `sms`; the person confirms it arrived. Within 5
   minutes the poll reports it delivered (the `plugin.twilio-plugin.delivery` event, `status <sid>`, and
   `Done: first message delivered to <cell>` in `check`).
3. **A reply** - the person replies to that text from their phone with any words. Within 5 minutes
   it arrives as `plugin.twilio-plugin.received` and you summarise it for them.
4. **STOP** - the person texts `STOP` from that phone. Within 5 minutes it is reported as an
   opt-out (`plugin.twilio-plugin.optout`), and twilio refuses to text that cell while it is opted out.
   Then they text `START` so Twilio lifts its block; the plugin reports `plugin.twilio-plugin.optin` and
   clears the opt-out itself, because their cell is on `sendAllowlist` and has no recipients entry.
   Nothing more is needed.

When all four pass, tell the person setup is complete and what they can now ask you to do.
