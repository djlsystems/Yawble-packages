# namecheap

A plugin member that adds and changes DNS records for domains registered at Namecheap that use
Namecheap's DNS: a custom domain's verification and certificate records for a hosted app, or a
mailbox's MX, SPF, DKIM and DMARC records. It never loses an existing record. Go, standard library
only, one static binary per processor.

## Set it up

No script is needed. You need a Namecheap account that holds the domains, and a few minutes.

1. **Marketplace, Get Namecheap**, then **Add member** on the team that will change DNS.
2. **Turn on Namecheap's API access.** Sign in at Namecheap and open **Profile, Tools**; under
   **Business & Dev Tools**, **Namecheap API Access**, press **Manage**
   (https://ap.www.namecheap.com/settings/tools/apiaccess/). Switch **API Access** to **ON** and
   accept the terms.
   - Namecheap turns API access on only for an account that meets one of its conditions: at least
     20 domains in the account, at least $50 on the account balance, or at least $50 spent in the
     last 2 years. The page says so when the account does not qualify yet. Namecheap's own guide is
     https://www.namecheap.com/support/api/intro/.
   - The page then shows the **API key**. Anyone who has it, and calls from an allowed address, can
     change every domain in the account: keep it only in the member's key field.
3. **Paste the API user and API key** into the member's two key fields: `apiUser` is your
   Namecheap user name (the one you sign in with), `apiKey` the key from that page.
4. **List the domains** it may change in the member's `domains` setting, such as `example.com`.
   It reads and changes no other domain.
5. **Run the member's `setup` command** (tell it `setup`). It checks each thing and says the next
   step in words:
   - the API user and key are set;
   - this computer's internet address, which it names, such as `203.0.113.7`;
   - API access is on for the account;
   - that address is on Namecheap's **API allow-list**. Namecheap answers only calls from
     addresses on it. The list is on the same API Access page, under **Whitelisted IPs**: press
     **Edit**, **Add IP**, paste the address `setup` named, give it a name, and save. Namecheap
     takes IPv4 addresses there;
   - each listed domain is in the account and uses Namecheap DNS (**Domain List**, the domain,
     **Manage**, **Nameservers** shows **Namecheap BasicDNS**, PremiumDNS or FreeDNS). A domain on
     other nameservers has its records served elsewhere, and this plugin refuses it. Switching it
     to Namecheap moves where its records come from, so do that only when they are meant to live
     at Namecheap.

   Run `setup` again after each step until it says `Ready`.
6. **Try a change in dry run**, the default mode: the member answers the records before and after
   and changes nothing. When it is right, set `mode` to `apply` in the member's settings and send
   the command again.

## Settings

All are set by a person.

| Setting | Default | What it does |
|---|---|---|
| `domains` | none | The only domains it may read or change. |
| `mode` | `dryrun` | `dryrun` answers each change as the records before and after and changes nothing; `apply` makes it. |
| `defaultTtl` (advanced) | `1799` | The TTL in seconds (60 to 60000) `ensure` gives a record it adds when the command names none. 1799 is Namecheap's Automatic. A ttl the command names always wins; a value out of range is refused, naming `defaultTtl`. |
| `clientIp` (advanced) | empty | This computer's public IPv4 address. Empty: the plugin finds it by asking `api.ipify.org`, and sends it to Namecheap as `ClientIp`. Set it when the computer reaches the internet through an address that changes or differs from the one found; that address must be on the allow-list. |
| `sandbox` (advanced) | off | Calls Namecheap's sandbox API (https://www.sandbox.namecheap.com) instead of the real one. The sandbox has its own account, API access page, key and allow-list. |

Secrets: `apiUser` and `apiKey`, as above.

## Commands

- `setup` - checks each step above and says the next one.
- `list <domain>` - every record of the domain, and its newest backups.
- `ensure <domain> <type> <host> <value> [ttl] [mxpref]` - makes the record be there. TXT, MX and
  CAA records are matched by value, so an SPF record and a verification record live side by side on
  one host; A, AAAA and CNAME are one per host.
- `remove <domain> <type> <host> [value]` - removes that one record. It also takes a URL redirect
  type (`URL`, `URL301` or `FRAME`): `remove example.com URL @` removes the redirect on the domain
  itself, so an app's A record can take its place. Only removal: a redirect is never added or changed.
- `restore <domain> <backup id>` - puts back the records a backup holds.
- `check <domain> <host> <type>` - asks each of the domain's authoritative name servers directly
  what it serves for that name and type (or that it serves nothing), with the TTL, and the zone's
  negative TTL from its SOA: how long a look-up made before the name existed stays cached as "no
  such name". It changes nothing, calls no Namecheap API and publishes no `changed` event.

Types: A, AAAA, CNAME, MX, TXT and CAA, and `remove` for URL redirects. Every other record, and a
redirect nobody asked to remove, is kept as it is. `ensure` refuses an A, AAAA or CNAME on a host
that holds a redirect, naming the redirect and the `remove` command that takes it away. Whether the
redirect may go is a person's decision, which the plugin cannot know: the team's instructions say
when a person has approved replacing it. A value with spaces goes in quotes: `ensure example.com TXT @ "v=spf1 include:_spf.example.net ~all"`.
The skill `plugin-namecheap` (`skills/plugin-namecheap.md`) says what each answer and refusal
means, and which records a hosted app's custom domain and a mailbox usually need.

## The one hard rule

Namecheap's `setHosts` replaces a domain's whole record set. Every change therefore:

- reads the current set and changes only the named record;
- refuses when the read fails, the domain does not use Namecheap DNS, or it is not in `domains`;
- refuses a change that would switch the domain off a mail setting Namecheap manages itself
  (Private Email `OX`, email forwarding `FWD`, `GMAIL`), such as an MX record on a Private Email
  domain: a person changes the mail setting in Namecheap first when that is meant. `restore` may
  still put back the setting a backup was saved with;
- saves the set before the change as a backup in the member's workspace
  (`namecheap-backups/<domain>/<id>.json`) and answers its id;
- writes the whole set back, reads it again, and checks every record that was there is still
  there. When one is missing, the answer names it and the backup to restore;
- then asks the domain's own name servers (from its NS records; `dns1.registrar-servers.com` and
  `dns2.registrar-servers.com` for Namecheap BasicDNS) directly, never through a resolver, for each
  record added, changed or removed, every 5 s for up to 60 s, and says per record `live on the name
  servers` or `not yet served by <server> after 60 s`. A record not yet live is not a failure:
  Namecheap holds it, and `check` asks again. One run waits at most 5 minutes in all.

Dry run answers the set before and after and changes nothing. Calls stay under Namecheap's limit of
20 a minute, counted across the member's runs in `namecheap-calls.json` in its workspace. A backup
holds records only: never the API user or key.

## Events

- `plugin.namecheap.changed`: `domain`, `command`, `added`, `changed`, `removed`, `backupId`, `live`
  (true when the name servers serve every record of the change).
- `plugin.namecheap.record`: `domain`, `recordType`, `host`, `value`, `action` (added, changed or
  removed), `backupId`, `live` (true when the name servers serve this change). One per record a change in apply mode wrote, at most 25 per command; `changed`
  covers the rest. The Monitor shows each as a DNS line such as `TXT _dmarc.example.com`, marked added,
  changed or removed. Never for a list, a dry run or a refusal.
- `plugin.namecheap.refused`: `command`, `domain` when one was named, `reason`.

## Build and test

```sh
packages/namecheap/build.sh ~/plugins-build/namecheap/1.0.4     # one installable version
(cd packages/namecheap && go test ./...)                        # against a fake Namecheap API and fake name servers
```

`scripts/build.sh` puts the package zip in `dist/namecheap-1.0.4.zip`. The tests run the plugin
against a fake Namecheap API, a fake address service and two fake authoritative name servers on the
test machine's loopback address, with a test clock so no test waits the 60 s; they never reach the
network.

## 1.0.4

- After `ensure`, `remove` and `restore` in apply mode, each record added, changed or removed is
  confirmed on the domain's own name servers, asked directly; the answer says `live on the name
  servers` or `not yet served by <server> after 60 s`, and the `changed` and `record` events carry
  `live`.
- New `check <domain> <host> <type>`: what each authoritative name server serves, with its TTL, and
  the zone's negative TTL from the SOA.
- New setting `defaultTtl` (60 to 60000, default 1799): the TTL of a record `ensure` adds without one.
- The run timeout is 600 s, room for the name-server waits.
