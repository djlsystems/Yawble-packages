---
name: plugin-namecheap-plugin
description: Use when you need to add, change, list or remove DNS records (A, AAAA, CNAME, MX, TXT, CAA), or remove a URL redirect record, for a domain at Namecheap, check whether its name servers serve a record yet, check Namecheap setup, or put back a domain's records from a backup through the Namecheap plugin member - its commands, dry run versus apply, the records a hosted app's custom domain and a mailbox usually need, and what its refusals mean.
roles: manager member
---

# Namecheap DNS (plugin member)

A plugin member, not an agent: it runs no model. Send it one command per instruction with `tell`;
it acts on the person's Namecheap account and hands back one result. It changes only the domains
in its `domains` setting, and only domains that use Namecheap DNS.

It never loses a record. Namecheap replaces a domain's whole record set on every write, so each
change reads the current records, changes only the one named, saves the records before the change
as a backup, writes them all back, and reads them again to check every earlier record is still
there. URL redirect records, and any other type it does not change, are written back as they were,
unless `remove` names a redirect to take away.

## Commands

The first word is the command. A value with spaces goes in double or single quotes. `host` is `@`
for the domain itself, or the name below it: `www`, `_dmarc`, `selector1._domainkey`
(`www.example.com` is read as `www`).

- `setup` - checks, in order: a Namecheap API-key connection with an API user and key is bound to
  the `namecheap` slot; this computer's internet address (named; the connection's Whitelisted IP when
  it has one); API access is on; that address is on Namecheap's API allow-list; each listed domain is
  in the account and uses Namecheap DNS. It ends with `Next step:` in words, or `Ready`. It changes
  nothing. Pass the next step to the person as it is written.
- `list <domain>` - every record, as `TYPE host value [pref N] ttl N`, and the newest backup ids.
- `ensure <domain> <type> <host> <value> [ttl] [mxpref]` - makes the record be there.
  - TXT, MX and CAA are matched by value: a verification TXT is added beside the SPF TXT on `@`,
    never in place of it. A host holds one SPF (`v=spf1`) and one DMARC record; a second is
    refused, naming the one there.
  - A, AAAA and CNAME are one per host: the one record there takes the new value. When the host
    has several, it is refused; remove the unwanted ones by value first.
  - A CNAME cannot sit beside other records on its host, either way round.
  - The record already there with that value is left alone, unless a ttl or mxpref given differs.
  - ttl is 60 to 60000 seconds; when not given, the member's `defaultTtl` setting (1799,
    Namecheap's Automatic, unless a person changed it). mxpref defaults to 10.
- `remove <domain> <type> <host> [value]` - removes that one record. Without a value the host must
  hold exactly one record of that type. The type may also be a URL redirect, `URL`, `URL301` or
  `FRAME` as `list` shows it: `remove example.com URL @` takes away the redirect on the domain
  itself. A redirect is only ever removed, never added or changed.
- `restore <domain> <backup id>` - puts back the records a backup holds. It saves the records it
  replaces as a new backup first, so a restore can be undone the same way.
- `check <domain> <host> <type>` - asks each of the domain's authoritative name servers directly
  what it serves for that name and type, with the TTL, or that it serves nothing (`no such name`,
  or the name exists without that type), and the zone's negative TTL from its SOA. It ends `Served
  by every name server: live on the name servers.` or `Not served by <server>.` It changes nothing.

Examples:

    ensure example.com CNAME www app-host.example.net
    ensure example.com TXT @ "site-verification=placeholder-token"
    ensure example.com MX @ mx1.example.net 3600 10
    remove example.com TXT _acme-challenge "old-token-placeholder"
    remove example.com URL @ https://www.example.net
    restore example.com 20260101T120000Z
    check example.com asuid TXT

## Dry run versus apply

`mode` is set by a person only. In `dryrun`, the default, `ensure`, `remove` and `restore` answer
`Dry run for <domain>: nothing was changed.` with the change in words and the whole record set
before and after. Nothing is written and no backup is made. In `apply` the change is made and the
answer names the backup id, with the records as Namecheap now holds them.

Show the person the dry run's before and after, then ask them to switch mode, for example: "Here is
what changes on example.com. To make it, set mode to apply in the Namecheap member's settings, then
tell me." Then send the same command again. You cannot change `mode`, `domains`, `clientIp`,
`sandbox` or the Namecheap connection yourself; never ask anybody for the API key in a message. The
API user, API key and Whitelisted IP are set by a person in Team settings, Connections, as a
Namecheap API-key connection bound to this member's `namecheap` slot - never in the member's
settings. A person may set
`mode` back to `dryrun` when the work is done.

## Live on the name servers

After every change in apply mode the plugin asks the domain's own name servers, directly and not
through any resolver, for each record it added, changed or removed, for up to 60 s, and says per
record `live on the name servers` or `not yet served by <server> after 60 s`. Not yet live is not a
failure; `check` asks again.

1. **Write first, then check, then look.** Write every record a step needs (a certificate, a
   domain verification), then `check` each one, then run the step that needs them. Never look a
   name up, or ask anything else to (a hosting service's verify button, a certificate request), before it
   exists: a premature look-up is cached as "no such name" for the zone's negative TTL, which
   `check` shows (3601 s on Namecheap), and nothing can clear that cache.
2. **TTL 300 for a record that may change during a build** (`ensure ... <value> 300`), so a
   correction is seen in minutes, not half an hour. Leave mail records (MX, SPF, DKIM, DMARC) at
   their own TTL.
3. **"Added" is Namecheap's answer; "live on the name servers" is the check's.** Report the second:
   say a record is in place only when the change or `check` says it is live.
4. **When another service still cannot see a record `check` shows live**, the cause is a cache on
   that side (often a "no such name" cached from an earlier look-up). Say so, and wait for its TTL
   (the negative TTL `check` names, for a name looked up before it existed). Never ask a person to
   set a record that exists.

## Records a hosted app's custom domain usually needs

The hosting service names the exact hosts and values; use them as given.

- `www` (or another subdomain): `CNAME` to the host the service names.
- The domain itself (`@`): an `A` record to the service's IPv4 address (and `AAAA` for IPv6). A
  CNAME cannot sit on `@` beside the domain's other records.
- A host that holds a URL redirect (`URL`, `URL301` or `FRAME` in `list`) takes no A, AAAA or
  CNAME: `ensure` is refused, naming the redirect and the exact `remove` command. Removing it stops
  the redirect, which a person may rely on, so remove it only when a person has approved replacing
  it (the team's instructions, or their answer when you ask). Then send the `remove`, dry run first
  as for any change, and `ensure` again. Its backup puts the redirect back with `restore`.
- Ownership verification: a `TXT` record on the host the service names (often `@`, or a name such
  as `_verify` or `asuid.www`), with its token as the value.
- Certificates: a `CAA` record on `@` allowing the service's certificate authority, as
  `ensure example.com CAA @ "0 issue ca.example.net"`, when the domain has CAA records already (with
  none, every authority may issue). Namecheap stores the value as `0 issue "ca.example.net"` and
  `list` shows it so; the plugin takes it typed either way. A DNS-01 check asks for a `TXT` on `_acme-challenge` (or `_acme-challenge.www`).

## Records a mailbox usually needs

The mail provider names the values.

First `list` the domain: when its mail setting is `OX` (Namecheap Private Email), `FWD` (Namecheap
email forwarding) or `GMAIL`, Namecheap itself serves the MX and SPF records and they are not host
records. Add none of them: an MX record would switch the setting to custom MX, and the plugin
refuses it, saying so. Only DKIM (when the provider does not already hold it) and DMARC are added.

- `MX` on `@`, one per mail host the provider lists, each with its preference. Adding MX records
  switches the domain's mail setting to custom MX; the answer says so when it changes.
- SPF: one `TXT` on `@` starting `v=spf1`, such as `"v=spf1 include:_spf.example.net ~all"`. To
  add a second sender, remove the old SPF by value and ensure one merged value.
- DKIM: a `TXT` (or `CNAME`) on `<selector>._domainkey`, as the provider gives it.
- DMARC: one `TXT` on `_dmarc`, such as `"v=DMARC1; p=none; rua=mailto:dmarc@example.com"`.

## Events

- `plugin.namecheap-plugin.changed` - `domain`, `command`, `added`, `changed`, `removed` (each a list of
  records in words), `backupId` and `live` (true when the name servers serve every record of it).
  Only in apply, after the read-back check passed.
- `plugin.namecheap-plugin.record` - one per record added, changed or removed in apply: `domain`,
  `recordType`, `host`, `value` (the public DNS value as written), `action`, `backupId` and `live`. At most 25
  per command; `changed` covers the rest. The Monitor shows each as `<type> <host>.<domain>` marked
  added, changed or removed; a list, a dry run or a refusal shows nothing there.
- `plugin.namecheap-plugin.refused` - `command`, `domain` when one was named, and `reason`.

## Refusals and failures

Every refusal ends "Nothing was changed."

- `Refused: no Namecheap API user and API key are bound` - a person adds a Namecheap API-key
  connection in Team settings, Connections, assigns it to the team and binds it to the `namecheap`
  slot. A connection carried over from an older version reads Needs you until a person fills in its
  Whitelisted IP once; pass that on as it is.
- `... is not in this member's domains setting` - a person adds the domain to `domains`.
- `... holds a URL redirect record ...` - the answer names the `remove` command; send it once a
  person has approved replacing the redirect, then ensure again. No change in Namecheap's own
  screens is needed.
- `... does not use Namecheap DNS` - its records are served elsewhere; change them there.
- `... records could not be read` - run `setup`; it names what is missing (often the address is
  no longer on the allow-list).
- `The write to ... failed` - the answer names the backup; `list` the domain and `restore` if
  anything is lost.
- `... reading it back shows N record(s) missing` - the records named were lost by the write;
  `restore <domain> <backup id>` as the answer says, at once.
- Calls stay under Namecheap's limit of 20 a minute: a run that would pass it says it is waiting
  and carries on.
