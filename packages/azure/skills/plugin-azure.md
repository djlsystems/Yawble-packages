---
name: plugin-azure
description: Use when a team needs Azure resources - resource groups, Bicep or ARM deployments, DNS zones and records, role assignments for its managed identities, budgets - through the Azure plugin member: its commands, whatif versus apply, the ownership rule, and where app secrets go.
roles: manager member
---

# Azure (plugin member)

A plugin member, not an agent: it runs no model. Send it commands with `tell`, one per line; it
acts in one Azure subscription, signed in as the person who connected it, and hands back one
result. It can never do more than that person can in Azure. You never see a token, and never ask
anybody for one.

## whatif and apply

`mode` is a person's setting. In `whatif` (the default) every changing command answers what it
would do and changes nothing in Azure. In `apply` it makes the change. Neither you nor the member
can switch it: when a plan is ready, ask the person to set `mode` to `apply` in the member's
settings, then send the same command again. Ask them to set it back to `whatif` when the work is
done.

`allowDelete` is off by default; while off, every delete is refused, in either mode.

## The ownership rule

Everything the member creates carries the tags `yawble-owner: <team id>` and
`yawble-plugin: azure`. It changes only:

- a resource carrying this team's tags, or anything inside a resource group that does;
- a new database on a server in `sharedSqlServers`, which is tagged and then owned;
- images pushed to, and AcrPull granted on, a registry in `sharedRegistries`.

It reads anything the person can read. Anything else is refused, naming the resource, and
`plugin.azure.refused` is published.

## Commands

Quote a value with spaces (`"Storage Blob Data Contributor"`).

- `setup` - checks the sign-in, the subscription, that the person's role can create resource
  groups, the regions, each shared SQL server and registry, and that the resource providers it
  uses are registered (registering them in apply when the role allows). Ends with `Next:` and
  numbered steps in words, or `Ready:`. Pass the `Next:` steps to the person as they are.
- `whoami` - the account, the subscription and every setting.
- `list [resource group]` - the resource groups, or the resources in one; `[owned]` marks this
  team's.
- `show <resource id>` - one resource as Azure answers it, with any key-, password- or
  secret-named value hidden.
- `rg create <name> <region>` - a resource group named `namePrefix` + name, in a region from
  `regions`, tagged for the team, with the monthly budget (alerts at 80% and 100%) when
  `monthlyBudget` is set.
- `deploy <resource group> <template> [parameters file]` - a Bicep (`.bicep`) or ARM JSON
  (`.json`) template from the team's documents folder or a worktree of this run, into a resource
  group the team owns. A relative path is read from the team's documents folder. It always runs a
  what-if first and answers each change. Any change outside what the team owns - a nested
  deployment into another resource group included - refuses the whole deployment, naming each
  resource. In apply it then deploys exactly the bytes and parameters it checked (the answer names
  their sha256), in Incremental mode, so it never deletes. A template that calls `listKeys` or
  another key-reading function, a storage account without `allowSharedKeyAccess: false`, a
  custom role, or a role assignment the `role assign` rules would refuse, is refused.
- `role assign <role> <managed identity id> <scope id>` - grants one of Contributor, Storage
  Blob Data Contributor, Storage Table Data Contributor, Storage Queue Data Contributor, Key Vault
  Secrets Officer, Key Vault Secrets User or AcrPull, on a scope the team owns (or AcrPull on a
  shared registry), to a managed identity the team created: a user-assigned identity's resource
  id, or the resource id of an owned resource with a system-assigned identity. Never to a user or
  a group.
- `role list <scope id>` - the role assignments at a scope.
- `dns zone create <resource group> <zone>` - a zone in an owned group; answers the name servers
  to set at the registrar.
- `dns record set <zone> <name> <A|AAAA|CNAME|TXT|MX> <value...> [ttl=<seconds>]` - in a zone
  the team owns; `@` is the zone itself; MX values come as `<preference> <host>` pairs.
- `dns record delete <zone> <name> <type>` - needs `allowDelete`.
- `delete <resource id or resource group>` - owned only, with `allowDelete` on.

## Events

- `plugin.azure.planned` - what a changing command would do, or a deployment's what-if: counts
  by change type (`create`, `modify`, `delete`, `unchanged`) and `refusals`.
- `plugin.azure.applied` - a change made in apply, with `resourceIds`.
- `plugin.azure.refused` - `command` and `reason`.

None ever holds a token, key or connection string.

## Secrets

The member never reads a key, a connection string or a Key Vault secret value, and creates storage
accounts with shared key access off. Apps reach storage, Key Vault and SQL with their managed
identity and a role from the list above.

An app secret (a third-party API key, a webhook signing secret) goes in Key Vault, set by a person
or by another plugin. It never goes in a template, a parameters file, the repository or a message.
A template refers to it with a Key Vault reference, and the app reads it with its managed identity
(Key Vault Secrets User).
