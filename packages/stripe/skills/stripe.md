---
name: plugin-stripe
description: Use when you need to set up Stripe products, one-time prices or webhook endpoints, rotate a webhook signing secret, check a Stripe key, or read a checkout, payment, customer or events through the Stripe plugin member - its commands, dry run versus apply, idempotency by lookup key, and where the application's keys live.
roles: manager member
---

# Stripe (plugin member)

A plugin member, not an agent: it runs no model. Send it one command per instruction with `tell`;
it acts on one Stripe account through a restricted key and hands back one result. It never moves
money. You never see the key or a webhook signing secret, and never ask anybody for either.

## Commands

The first words are the command. Quote a value with spaces (`"Pro plan"`).

- `setup` - checks the key is restricted (`rk_`; an `sk_` key is refused), names any missing
  permission (and publishes `plugin.stripe.refused` naming it), says test or live, keeps the key's
  account for this member, checks each vault in `keyVaults`, and ends with `Next:` (what a person
  does next) or `Ready:`. Pass the `Next:` line to the person as it is. In dryrun setup only reads:
  it sends no write to Stripe or Key Vault and says the writes are confirmed in apply. In apply it
  writes the check secret `stripe-plugin-check` to each vault to confirm the vault can be written.
- `whoami` - the account, `test` or `live`, and the key's prefix only.
- `product ensure <lookup key> <name> [description]` - the product whose lookup key this is exists
  with this name (and description, when given).
- `price ensure <lookup key> <product lookup key> <amount in cents> <currency>` - a one-time price
  with that lookup key exists on that product. `1500 usd` is 15.00 USD. A different amount or
  currency makes a new price, moves the lookup key to it and archives the old one. Ensure the
  product first.
- `webhook ensure <url> <event...> <vault>/<secret name>` - an endpoint for that https URL sends
  those events. When it creates the endpoint, it writes the signing secret into that Key Vault
  secret and answers only the secret's name and version. When an endpoint for the URL exists, it
  only updates its events; the secret stays as it is.
- `webhook rotate <url> <vault>/<secret name>` - makes a new endpoint with the same events and a new
  signing secret (a new version of that Key Vault secret), then removes the old endpoint.
- `checkout show <cs_ id>`, `payment show <pi_ id>`, `customer find <email>`,
  `events [type] [since]` - read only, at most 50 items. A card shows as its brand and last four,
  nothing more. `since` is `2026-01-31`, an RFC 3339 time, `24h` or `7d`.

## Dry run versus apply

`mode` is a person's setting. In `dryrun` (the default) every command looks things up and answers
`Would create ...` or `Would update ...`, ending `Dry run: nothing was changed`; nothing is made in
Stripe and nothing is written to Key Vault. In `apply` the same command makes the change and answers
`Created ...` or `Updated ...`. Plan with dry runs, show the person what they say, and ask the person
to set `mode` to `apply`; no instruction switches it.

## Idempotency by lookup key

Every object is named by a lookup key you choose (`pro`, `pro-monthly`): a product's lookup key is
its Stripe id, a price's is Stripe's `lookup_key`, a webhook endpoint's is its URL. Ensuring the same
thing again finds it and answers `already there; nothing to change`, so a command is safe to repeat
after a failure or a lost answer. Use stable, meaningful lookup keys, and refer to prices by lookup
key in the application, never by their `price_` id, so a changed amount needs no code change.

## Keys and secrets live in Key Vault

The application reads its Stripe keys and the webhook signing secret from Key Vault at runtime,
with its own identity. They are never written into the repository, a settings file, a card, a
chat or a document. To give the application a webhook secret, name the vault and secret in
`webhook ensure` and have the application read that secret; when it must change, use
`webhook rotate`. The vault must be one the person listed in `keyVaults`.

## Refusals

A refused command changes nothing and publishes `plugin.stripe.refused` with the command and the
reason. The reasons, in the plugin's words: an `sk_` or other non-restricted key; a key for another
account than `expectedAccount` or than the account setup kept (a person fixes the key, or sets
`expectedAccount` and runs `setup`); `setup` not yet run; a vault not in `keyVaults`; no Microsoft
connection on the `microsoft` slot; arguments of the wrong shape (the sentence shows the right one).
A missing permission makes Stripe answer 403; run `setup` to see which one.

## Events

- `plugin.stripe.changed` - `objectType`, `id`, `lookupKey`, `environment` (`test`/`live`),
  `action` (`created`/`updated`/`deleted`); in `apply` mode only.
- `plugin.stripe.refused` - `command`, `reason`.
