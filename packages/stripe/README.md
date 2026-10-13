# Stripe 1.1.0

A plugin member that sets up a team's Stripe objects - products, one-time prices and webhook
endpoints - and reads checkouts, payments, customers and events. It works through a **restricted
key**, never a secret key. A webhook's signing secret goes from Stripe straight into an Azure Key
Vault, written through Azure management (Azure Resource Manager), so no agent, output, event or
file ever holds it. One member per Stripe account. It never
moves money: no charges, refunds or payouts.

Everything starts in **dry run**: each command looks things up and says what it would do, and
changes nothing until a person sets `mode` to `apply`.

## Set up, with no script

1. **Marketplace**, **Get** Stripe, then **Add member** on a team and choose the Stripe plugin.
2. Make a restricted key in the Stripe Dashboard (below) and put it in the member's `apiKey` field.
   The field takes the name of an instance secret, such as `STRIPE_RESTRICTED_KEY`; its value is
   set on the instance with `yawble secret set STRIPE_RESTRICTED_KEY`, and the Host reads it at its
   next start.
3. Optional, to create webhook endpoints: make a Key Vault ready (below), add its name to
   `keyVaults`, and sign in with Microsoft on the member's `microsoft` connection slot.
4. Tell the member `setup`. It checks each thing and says the next step in words:
   - the key is restricted (`rk_`), not a secret key (`sk_`, refused);
   - it has each permission the plugin needs, naming any that is missing;
   - whether it is a test or a live key;
   - each vault in `keyVaults` is found and reached through Azure management with the Microsoft
     connection. In dry run setup only reads: it sends no write to Stripe or Key Vault, reads the
     resource of the secret `stripe-plugin-check` (a read that answers no value), checks the write
     permissions by reading, and
     says the writes are confirmed when it runs with `mode` set to `apply`. In apply it also
     confirms each vault can be written by writing a secret named `stripe-plugin-check` holding
     the words `write check`, and confirms each write permission with an update of an object that
     does not exist, which changes nothing;
   - the Stripe account the key belongs to, which it keeps for this member (see
     [The expected account](#the-expected-account)).

## Make the restricted key

A restricted key can do only what its permissions allow. Make one key for this member, in test
mode first.

1. Sign in to the Stripe Dashboard and open **Developers**, **API keys**
   (https://dashboard.stripe.com/apikeys). The **Test mode** switch decides whether the key is a
   test key (`rk_test_...`) or a live key (`rk_live_...`).
2. Under **Restricted keys**, press **Create restricted key**. If Stripe first asks how the key will
   be used, choose the option that lets you set the permissions yourself.
3. Name it after the team, such as `Yawble stripe member`.
4. Set exactly these permissions, and leave every other one at **None**. The filter box above the
   list finds each row.

### The restricted key's permissions

| Permission | Level | What the plugin does with it |
|---|---|---|
| Products | Write | `product ensure` |
| Prices | Write | `price ensure` |
| Webhook Endpoints | Write | `webhook ensure`, `webhook rotate` |
| Checkout Sessions | Read | `checkout show` |
| PaymentIntents | Read | `payment show` |
| Charges | Read | the card brand and last four in `payment show` |
| Customers | Read | `customer find` |
| Events | Read | `events` |
| Balance | Read | `setup` checks the key with it |

5. Press **Create key**. Stripe may show the whole key only once: copy it now and set it on the
   instance as in step 2 of the setup. Never paste it into a chat, a card or a file in a
   repository.
6. Tell the member `setup`. A missing permission is named, such as
   `- permissions: missing Prices (write)`, and setup publishes `plugin.stripe.refused` naming it;
   edit the key in the Dashboard, set that permission, and run `setup` again. A key with only
   Read on Products, Prices or Webhook Endpoints is named missing when setup runs in apply.

When the test key works the way you want, make a live key the same way with the **Test mode**
switch off, and bind it instead.

## Make a Key Vault ready for webhook secrets

1. In the Azure portal, make a Key Vault, or choose one the application already reads. Its name
   goes in `keyVaults`, as the name (`payments-vault`) or the address
   (`https://<name>.vault.azure.net`). The plugin writes to no vault outside that list.
2. Give the Microsoft account you will sign in with the **Contributor** or **Key Vault
   Contributor** role on that vault (Access control (IAM), Add role assignment). The plugin writes
   secrets through Azure management, which needs no Key Vault data role (such as Key Vault Secrets
   Officer) and no access policy, whichever permission model the vault uses.
3. On the member's `microsoft` connection slot, sign in with that account. The slot asks for
   Azure management only (`https://management.azure.com/user_impersonation`); the plugin uses the
   connection for nothing else.
4. Run `setup` again. In dry run it answers `- Key Vault payments-vault: reached through Azure
   management with the Microsoft connection`; in apply it answers
   `- Key Vault payments-vault: can be written`.

The plugin finds each vault by its name: it lists the subscriptions the signed-in account can read
and the Key Vaults in each. A vault in none of them is refused with a sentence saying the account
cannot see a Key Vault of that name and needs a role on it, or that the name in `keyVaults` is
wrong. A secret is written with `PUT` on the vault's resource
(`.../providers/Microsoft.KeyVault/vaults/<name>/secrets/<secret name>`); its value is in that
request's body and nowhere else, and Azure management never answers it back.

The application reads the signing secret from Key Vault at runtime, with its own identity (a
managed identity, or a Key Vault reference in its configuration). The secret is never copied into
the application's repository, its settings files or a chat.

## Settings

All three are set by a person only; no command or instruction changes them.

- `mode`: `dryrun` (the default) changes nothing and says what it would do; `apply` makes the
  changes.
- `keyVaults`: the vaults a webhook signing secret may be written to. Any other vault is refused.
- `expectedAccount` (Advanced): the Stripe account (`acct_...`) the key must belong to.

### The expected account

`setup` reads which account the key belongs to. When `expectedAccount` is empty it keeps that
account for the member (in `stripe-account.json` in the member's workspace: the account id and
nothing else) and says so; copy the id into `expectedAccount` to make it permanent. From then on,
every run checks the key's account first and refuses a key for any other account, so a key swapped
for another account's changes nothing. Until `setup` has run, every command except `setup` and
`whoami` is refused.

## Commands

| Command | What it does |
|---|---|
| `setup` | Checks the key, its permissions, test or live, the account and each vault; says the next step. |
| `whoami` | The account, test or live, and the key's prefix only (`rk_test_…`). |
| `product ensure <lookup key> <name> [description]` | Creates the product, or updates its name and description. |
| `price ensure <lookup key> <product lookup key> <amount in cents> <currency>` | Creates a one-time price, or moves the lookup key to a new price when the amount or currency changed. |
| `webhook ensure <url> <event...> <vault>/<secret name>` | Creates the endpoint and writes its signing secret to Key Vault, or updates the events of the endpoint for that URL. |
| `webhook rotate <url> <vault>/<secret name>` | Makes a new endpoint and signing secret for the URL, then removes the old endpoint. |
| `checkout show <cs_ id>` | One Checkout Session. |
| `payment show <pi_ id>` | One PaymentIntent, with the card's brand and last four only. |
| `customer find <email>` | Customers with that email, at most 50. |
| `events [type] [since]` | Events, newest first, at most 50; `since` is a date (`2026-01-31`), a time or an age (`24h`, `7d`). |

Quote a name or description with spaces: `product ensure pro "Pro plan" "Everything in one plan"`.

Commands are safe to repeat. A product's lookup key is its Stripe id, and a price's is Stripe's
`lookup_key`, so ensuring the same thing twice finds it the second time and changes nothing; each
create also carries an idempotency key, so a retried request cannot make a second object.

## Events

- `plugin.stripe.changed`: `objectType` (`product`, `price` or `webhook_endpoint`), `id`,
  `lookupKey` (a webhook endpoint's URL), `environment` (`test` or `live`) and `action`
  (`created`, `updated` or `deleted`). Published in `apply` mode only, once per object changed.
- `plugin.stripe.refused`: `command` (without its arguments) and `reason`.

## Build and test

```sh
packages/stripe/build.sh ~/plugins-build/stripe/1.1.0
(cd packages/stripe && go test ./...)
```

The tests run the plugin against a fake Stripe API and a fake Azure Resource Manager (subscriptions,
each subscription's Key Vaults, and a vault's secrets) on a local test server; they reach no real
service.
